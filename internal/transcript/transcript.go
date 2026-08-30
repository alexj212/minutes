// Package transcript turns a recording's segments into one ordered conversation.
//
// The two tracks are transcribed separately and merged on the shared clock.
// That is the whole payoff of never mixing them: the microphone track is you
// and the system track is everyone else, so every line arrives already
// attributed, with no diarization model involved and nothing to get wrong.
package transcript

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/alexj212/minutes/internal/manifest"
	"github.com/alexj212/minutes/internal/transcribe"
	"github.com/alexj212/minutes/internal/wav"
)

// File names inside a recording directory.
const (
	JSONName = "transcript.json"
	TextName = "transcript.txt"
)

// silenceFloorDBFS is the level below which a segment is not sent to a speech
// model at all.
//
// Speech peaks far above this — the proof recordings measured -7 dBFS for a
// voice and -70 for an empty room. Below the floor there is nothing to
// transcribe, and asking anyway is worse than useless: given silence, whisper
// invents. It will confidently return "Thank you." for a minute of nothing, and
// that lands in the notes as something somebody said.
const silenceFloorDBFS = -60

// noSpeechThreshold is the model's own confidence that a span held no speech,
// above which its words are discarded.
//
// A speech model given silence does not return nothing. It invents a plausible
// sentence — "Thank you.", "Department of Education." — and hands it over with
// no hedge, and the recorder then puts those words in somebody's mouth. A
// missing disclosure gets noticed; a fabricated quote gets believed.
//
// Whisper reports its own doubt and this pipeline used to throw it away.
// Measured on the target machine: real speech reports 0.001, a microphone track
// at -56 dBFS reported 0.908 and produced one nine-second line of invention
// attributed to the operator. 0.6 is whisper's own default and sits in the
// enormous gap between those two.
const noSpeechThreshold = 0.6

// Speaker labels. Your track is you; the other track is everyone else.
const (
	SpeakerYou    = "You"
	SpeakerOthers = "Others"
	// SpeakerUnattributed is used when the recording cannot support a label at
	// all, rather than guessing one.
	//
	// A microphone track means "the operator" *because* the other track holds
	// everyone else. Remove the far-end track and that inference collapses: the
	// microphone holds whoever was audible in the room, and calling all of it
	// "You" puts other people's words in the operator's mouth. A missing
	// disclosure gets noticed; a fabricated quote gets believed.
	SpeakerUnattributed = "Unattributed"
)

// Line is one utterance placed on the recording's timeline.
type Line struct {
	// Start and End are seconds from the recording epoch — the instant both
	// tracks call zero.
	Start   float64 `json:"start"`
	End     float64 `json:"end"`
	Track   string  `json:"track"`
	Speaker string  `json:"speaker"`
	Text    string  `json:"text"`
	// Suppressed names why a line was withheld from the readable transcript.
	// Empty on everything that survived.
	Suppressed string `json:"suppressed,omitempty"`
	// NoSpeechProb is the model's raw judgement on this span, at full precision.
	//
	// Recorded as a number rather than only formatted into the sentence above.
	// A reviewer found a withheld block reporting exactly "0.600" — the
	// threshold, to three decimals, on two spans at once with whole-second
	// bounds — and asked whether it had been measured or synthesized. The
	// formatted string could not answer that, and neither could I: real values
	// look like 0.0006066110800020397. Whatever produced it, the next
	// occurrence will carry the number it was actually given.
	NoSpeechProb float64 `json:"noSpeechProb,omitempty"`
	// FarEndSilent marks a line spoken while the other side had been silent
	// long enough that the call may not have been running. What the microphone
	// picked up then may be the room rather than the meeting.
	FarEndSilent bool `json:"farEndSilent,omitempty"`
}

// Transcript is the merged conversation.
type Transcript struct {
	RecordingID string    `json:"recordingId"`
	Name        string    `json:"name,omitempty"`
	CreatedAt   time.Time `json:"createdAt"`
	Backend     string    `json:"backend"`
	// AudioLeftMachine records whether producing this transcript uploaded the
	// meeting. It is written down because it is the kind of thing somebody will
	// need to answer later, possibly to somebody else.
	AudioLeftMachine bool `json:"audioLeftMachine"`
	// Recorded is carried through from the manifest: a summary built from this
	// should be able to say the meeting was recorded without inferring it.
	Recorded bool `json:"recorded"`
	// BleedSuppressed counts microphone lines dropped as echoes of the system
	// track. A large number means the meeting was played through speakers.
	BleedSuppressed int `json:"bleedSuppressed,omitempty"`
	// Unattributed means this recording carries no speaker labels at all,
	// because it had only one source. Distinct from labels that exist and are
	// doubtful.
	Unattributed bool `json:"unattributed,omitempty"`
	// AttributionUnreliable explains why the speaker labels below cannot be
	// trusted, and is empty when they can.
	//
	// Speaker attribution rests entirely on the two tracks holding different
	// audio. If the far-end track did not capture the far end — the wrong
	// application was targeted, or it was muted — then the microphone's
	// acoustic pickup of the far end has nothing to be compared against, and it
	// is published as the operator. Real words, wrong mouth, and the operator's
	// is the worst one to get wrong.
	AttributionUnreliable string `json:"attributionUnreliable,omitempty"`
	// SilentSegments counts whole segments never sent to the model because
	// their peak was below the silence floor.
	//
	// Different from a withheld line, and it has to be counted separately: a
	// withheld line was transcribed and then set aside with a reason attached,
	// and this is audio that was never looked at. Nothing downstream can tell
	// that a stretch of the meeting is missing rather than quiet.
	//
	// It went to the log until now, which for a supervised recording is a file.
	// That is the same place the helper's report of a dead microphone sat unread
	// for two days.
	//
	// Measured across 106 segments of real meetings: zero. So this is silent in
	// the ordinary case and worth reading when it is not.
	SilentSegments int `json:"silentSegments,omitempty"`
	// MicrophoneLost records that the operator's own track captured nothing.
	//
	// Distinct from every other disclosure here, which is about labels being
	// wrong. These labels are right: everything present really was the far end.
	// What is wrong is the *record* — one side of the conversation is simply
	// absent, and a transcript in which the operator never speaks is
	// indistinguishable from a meeting in which they never spoke.
	//
	// The far end was checked from the beginning and the microphone was not,
	// because the microphone is the one you assume is there. A real 44-minute
	// standup was transcribed, attributed entirely to "Others" and delivered to
	// another project before anybody noticed the helper had already said
	// "track mic ended after 0 audio frames" in the log.
	MicrophoneLost bool `json:"microphoneLost,omitempty"`
	// ModelDoubted counts spans the model judged unlikely to contain speech.
	//
	// Not "invented": the model doubting a span does not establish that the
	// words were fabricated. Some are — given silence it produces a sentence —
	// and some are real speech that was merely unclear. Collapsing the two into
	// one counter would hide whichever turns out to be rarer, so this says what
	// was measured rather than what it was assumed to mean.
	ModelDoubted int `json:"modelDoubted,omitempty"`
	// Withheld holds every line that was dropped, and why.
	//
	// Three passes can now remove a line — an echo of the far end, a quiet
	// fragment of it, and a span the model said was not speech — and a count
	// alone gives nobody a way to check the judgment. These are kept out of the
	// readable transcript and written down here, because a transcript that
	// cannot show its own omissions is asking to be trusted rather than read.
	Withheld []Line `json:"withheld,omitempty"`
	// FarEndSilent lists stretches where only the microphone carried speech.
	FarEndSilent []Silence `json:"farEndSilent,omitempty"`
	Lines        []Line    `json:"lines"`
}

// speakerFor maps a track name to who is on it.
// speakerFor maps a track to who is on it, and refuses to guess.
//
// Only two names carry a meaning: the microphone is the operator, the system
// track is everyone else. Anything else — a source this build does not know
// about, a typo in a manifest, a device that records a room rather than a
// person — gets no speaker at all.
//
// It used to return "Others" for everything that was not the microphone, which
// is an assertion about who spoke made on the basis of a name not matching. A
// track called "room" would have been published as the far end. The failure
// mode of guessing here is somebody's words in somebody else's mouth, and the
// failure mode of refusing is a line nobody is credited with — which is the
// direction a mistake should fall.
func speakerFor(track string) string {
	switch track {
	case "mic":
		return SpeakerYou
	case "system":
		return SpeakerOthers
	}
	return SpeakerUnattributed
}

// Tally is how many lines each speaker has, counted rather than inferred.
//
// This exists because the same mistake has now been made four times in four
// places: `if speaker == You { you++ } else { others++ }`. That else-branch is
// the assertion "anything that is not you is the far end", and on a recording
// with one source it produced "0 you, 3 everyone else" — in the console, and in
// the brief sent to the session that writes the notes — directly contradicting
// a disclosure saying the recording could not say who spoke.
//
// Three call sites each decided independently and all three decided wrong, so
// the counting is here once. A speaker this build does not recognise lands in
// Unattributed, which is the same refusal speakerFor makes.
type Tally struct {
	You          int
	Others       int
	Unattributed int
}

// Count tallies lines by speaker.
func Count(lines []Line) Tally {
	var t Tally
	for _, l := range lines {
		switch SpeakerLabel(l.Speaker) {
		case SpeakerYou:
			t.You++
		case SpeakerOthers:
			t.Others++
		default:
			t.Unattributed++
		}
	}
	return t
}

// SpeakerLabel is what to print for a stored speaker, and it names the unknown
// case rather than leaving it blank.
//
// The zero value of a Line's Speaker is "", which is not one of the three
// labels — so a Line nothing set rendered as a bare ":" with the text beside
// it, indistinguishable from a formatting fault. That is reachable rather than
// theoretical: Load reads transcript.json back from disk and `minutes deliver`
// renders it, so a file written by any build that left the field unset, or one
// somebody edited, arrives here.
//
// Worse, Count already defaulted an unrecognised speaker to Unattributed while
// the renderer did not, so the brief said "3 unattributed" over a transcript
// showing three blank labels — a disclosure and a body disagreeing, which reads
// as a glitch rather than as a claim. Both go through here now, which is the
// same fix Count itself was: decide once, in one place.
//
// Found by asking a rule this project sent to the fleet — make the zero value
// the unknown one — of its own code, after shabadoo asked it of theirs and
// found an instance an hour old.
func SpeakerLabel(speaker string) string {
	switch speaker {
	case SpeakerYou:
		return SpeakerYou
	case SpeakerOthers:
		return SpeakerOthers
	}
	return SpeakerUnattributed
}

// job is one segment queued for transcription.
type job struct {
	track string
	start float64
	path  string
}

// Build transcribes a recording and merges its tracks.
func Build(ctx context.Context, m *manifest.Manifest, t transcribe.Transcriber, log func(string, ...any)) (*Transcript, error) {
	if log == nil {
		log = func(string, ...any) {}
	}

	var jobs []job
	var skipped int
	for _, track := range m.Tracks {
		for _, seg := range track.Segments {
			if seg.Frames == 0 {
				continue
			}
			if seg.PeakDBFS <= silenceFloorDBFS {
				skipped++
				continue
			}
			path := filepath.Join(m.Dir(), seg.File)
			if _, err := os.Stat(path); err != nil {
				// A segment named in the manifest but missing from disk is
				// worth saying out loud rather than quietly producing a
				// transcript with a hole in it.
				log("segment %s is in the manifest but not on disk; skipping", seg.File)
				continue
			}
			jobs = append(jobs, job{track: track.Name, start: seg.StartSeconds, path: path})
		}
	}
	if skipped > 0 {
		log("skipped %d silent segment(s): below %d dBFS there is nothing to transcribe, and a speech model asked to transcribe silence invents text", skipped, silenceFloorDBFS)
	}
	if len(jobs) == 0 {
		return nil, fmt.Errorf("nothing to transcribe: no segment in %s carries audio above %d dBFS", m.Dir(), silenceFloorDBFS)
	}

	// Trim each segment's leading silence and carry the offset forward. A
	// speech model given a file that opens with silence anchors its first
	// utterance at zero instead of where the speech is, and the system track
	// opens that way in every recording because the render endpoint is idle
	// until something plays.
	trimDir, err := os.MkdirTemp("", "minutes-trim-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(trimDir)

	paths := make([]string, 0, len(jobs))
	prepared := make([]job, 0, len(jobs))
	for _, j := range jobs {
		trimmed := filepath.Join(trimDir, filepath.Base(j.path))
		skipped, err := wav.TrimLeadingSilence(j.path, trimmed)
		if err != nil {
			return nil, fmt.Errorf("preparing %s: %w", filepath.Base(j.path), err)
		}
		j.path = trimmed
		j.start += skipped
		paths = append(paths, j.path)
		prepared = append(prepared, j)
	}
	jobs = prepared

	// One call for every segment of both tracks, so the model is loaded once.
	results, err := t.Transcribe(ctx, paths)
	if err != nil {
		return nil, err
	}
	if len(results) != len(jobs) {
		return nil, fmt.Errorf("transcriber returned %d results for %d files", len(results), len(jobs))
	}

	out := &Transcript{
		RecordingID:      m.ID,
		Name:             m.Name,
		CreatedAt:        time.Now(),
		Backend:          t.Name(),
		AudioLeftMachine: t.SendsAudioOffMachine(),
		Recorded:         m.Recorded,
	}
	// Counted where the segments were read and recorded here, because it has to
	// survive into the file: the log is not a place anybody looks afterwards.
	out.SilentSegments = skipped
	// A model can report an end past the end of the audio. Left alone that
	// stamps a line as running beyond the recording — observed: 12.06s on a
	// 9.98s file — which makes a transcript disagree with its own manifest.
	trackEnd := map[string]float64{}
	for _, track := range m.Tracks {
		trackEnd[track.Name] = track.Duration()
	}

	for i, j := range jobs {
		for _, u := range results[i] {
			start := j.start + u.Start
			end := j.start + u.End
			// Clamp only when the track is genuinely longer than where this
			// line begins. A limit below the start would move the end before
			// it, which is worse than an end that overruns — and it means the
			// track metadata is wrong, not the model.
			if limit, ok := trackEnd[j.track]; ok && limit > start && end > limit {
				end = limit
			}
			line := Line{
				// Segment-relative times become absolute here. This is the
				// only place the two tracks are related to each other, and it
				// is arithmetic because the segments were cut on a shared
				// clock in the first place.
				Start:   start,
				End:     end,
				Track:   j.track,
				Speaker: speakerFor(j.track),
				Text:    u.Text,
			}
			line.NoSpeechProb = u.NoSpeechProb
			out.Lines = append(out.Lines, line)
		}
	}
	Sort(out.Lines)

	kept, echoes := SuppressBleed(out.Lines)
	out.Lines = kept
	dropped := len(echoes)
	for _, l := range echoes {
		l.Suppressed = "an echo of the far end, arriving through the air"
		out.Withheld = append(out.Withheld, l)
	}

	// Second pass, by level rather than by words. A fragment of the far end
	// whose words never made it into the far-end transcript cannot be matched
	// textually, but it is still quieter than speech into the microphone.
	if micTrack, ok := trackNamed(m, "mic"); ok {
		lr := newLevelReader(m.Dir(), m.SegmentSeconds, micTrack)
		if ref, ok := referenceLevel(out.Lines, lr); ok {
			quiet, faint := suppressQuietFragments(out.Lines, ref,
				func(l Line) (float64, bool) { return lr.peakDBFS(l.Start, l.End) })
			if n := len(faint); n > 0 {
				out.Lines = quiet
				dropped += n
				for _, l := range faint {
					l.Suppressed = fmt.Sprintf("a fragment %.0f dB or more below your speaking level while the far end was talking", quietMarginDB)
					out.Withheld = append(out.Withheld, l)
				}
				log("dropped %d quiet microphone fragment(s): %.0f dB or more below your speaking "+
					"level while the other side was talking, which is the far end arriving through the air", n, quietMarginDB)
			}
		}
	}

	// Model doubt last, and only on what the echo detectors did not claim.
	//
	// It used to run first, which meant a borderline value decided cases the
	// echo detector was built for. Measured on a real recording: an echo was
	// withheld on a no-speech probability of 0.6001495718955994 against a 0.6
	// threshold — a margin of 0.00015, four hundredths of a percent of the way
	// from the threshold to certainty. The outcome was right and it was right
	// by luck; a slightly different room or mic gain flips it and the far end
	// is published as the operator.
	//
	// So the robust test decides first, and the model's own doubt is what
	// catches what remains — chiefly invention over silence, where there is no
	// far end to compare against and nothing else can catch it.
	var doubted int
	surviving := make([]Line, 0, len(out.Lines))
	for _, l := range out.Lines {
		if l.NoSpeechProb >= noSpeechThreshold {
			doubted++
			l.Suppressed = fmt.Sprintf("the model put %.3f probability on this span containing no speech", l.NoSpeechProb)
			out.Withheld = append(out.Withheld, l)
			continue
		}
		surviving = append(surviving, l)
	}
	out.Lines = surviving
	Sort(out.Withheld)

	if doubted > 0 {
		out.ModelDoubted = doubted
		log("withheld %d span(s) the model itself judged unlikely to be speech, after the "+
			"echo tests had already claimed what they could. Given silence a model produces "+
			"a plausible sentence and hands it over without a hedge; some of those words may "+
			"instead have been real and merely unclear. They are in %s with the probability, "+
			"rather than thrown away", doubted, JSONName)
	}

	if dropped > 0 {
		out.BleedSuppressed = dropped
		log("suppressed %d microphone line(s) as echoes of the system track: "+
			"this meeting was on speakers rather than headphones, so the microphone "+
			"also heard the far end", dropped)
	}
	// Asked of the microphone as well as the far end. A recording missing the
	// operator's own audio still produces correct labels, so nothing else here
	// would ever notice it.
	if !trackHasAudio(m, "mic") {
		out.MicrophoneLost = true
		log("nothing was captured from the microphone: everything you said in this " +
			"meeting is absent from the transcript. The labels below are right and the " +
			"record is not complete")
	}

	// Whether this recording can support speaker labels at all, before asking
	// whether the ones it has are any good.
	if !hasFarEndTrack(m) {
		unattribute(out, log)
	} else {
		// A precondition for attribution rather than a quality check: when the
		// far-end track holds no speech, echo suppression has no reference and
		// the room is published as the operator.
		checkAttribution(m, out, log)
	}

	out.FarEndSilent = findFarEndSilence(out.Lines)
	markFarEndSilence(out.Lines, out.FarEndSilent)
	for _, g := range out.FarEndSilent {
		log("the other side was silent from %s for %s (%d lines): "+
			"the call may have dropped, and what the microphone heard then may be the room",
			clock(g.Start), roughMinutes(g.Duration()), g.Lines)
	}
	return out, nil
}

// hasFarEndTrack reports whether anything was captured from the far end.
//
// Not whether a track is listed — whether it holds audio. A track declared and
// never written to is the same thing as no track, and treating them differently
// would be the empty-versus-unknown mistake in a third place.
func hasFarEndTrack(m *manifest.Manifest) bool {
	return trackHasAudio(m, "system")
}

// trackHasAudio reports whether a named track captured anything at all.
//
// Not whether it is listed — whether it holds frames. This is the same
// question for every track, which is the point: it was originally asked only
// of the far end, because the microphone was assumed to be there. It was not,
// once, for forty-four minutes.
func trackHasAudio(m *manifest.Manifest, name string) bool {
	t, ok := trackNamed(m, name)
	if !ok {
		return false
	}
	for _, seg := range t.Segments {
		if seg.Frames > 0 {
			return true
		}
	}
	return false
}

// unattribute strips every speaker label, because this recording cannot support
// one.
//
// Deliberately not a warning header over labels that stay. A summariser reads
// the lines and can skim the header; it cannot skim a label that is not there.
// Making the assertion impossible is the only version of this that survives
// contact with something that only reads the transcript.
func unattribute(out *Transcript, log func(string, ...any)) {
	if len(out.Lines) == 0 {
		return
	}
	for i := range out.Lines {
		out.Lines[i].Speaker = SpeakerUnattributed
	}
	// Withheld lines too. They are the ones a reader reaches for when a line
	// is missing from the readable transcript, so a speaker asserted there is
	// asserted at exactly the moment somebody is looking closely — and it is
	// in the JSON, which is read by machines that cannot notice the warning
	// in the text header.
	for i := range out.Withheld {
		out.Withheld[i].Speaker = SpeakerUnattributed
	}
	out.Unattributed = true
	out.AttributionUnreliable = fmt.Sprintf(
		"Nothing was captured from the far end, so this recording is one source and "+
			"cannot say who spoke. A microphone track means \"the operator\" only because "+
			"the other track holds everyone else; with no other track it holds whoever was "+
			"audible in the room. All %d line(s) below are unattributed for that reason, "+
			"and no speaker should be inferred from them.", len(out.Lines))
	log("%s", out.AttributionUnreliable)
}

// checkAttribution decides whether the speaker labels mean anything.
//
// The test is the far-end track's crest factor, because level alone does not
// separate "captured the meeting quietly" from "captured a tone at a healthy
// level". Speech runs 15 to 20 dB between peak and average; a pure tone is
// 3.01; steady noise is around 10.
func checkAttribution(m *manifest.Manifest, out *Transcript, log func(string, ...any)) {
	far, ok := trackNamed(m, "system")
	if !ok || far.Duration() <= 0 {
		return
	}
	// Only worth asking if the microphone actually produced lines. With nothing
	// attributed to anybody there is nothing to mislabel.
	micLines := 0
	for _, l := range out.Lines {
		if l.Track == "mic" {
			micLines++
		}
	}
	if micLines == 0 {
		return
	}

	lr := newLevelReader(m.Dir(), m.SegmentSeconds, far)
	crest, ok := crestDB(lr, far.Duration())
	if !ok || crest >= speechCrestDB {
		return
	}

	what := "the far-end track"
	if m.App != "" {
		what = fmt.Sprintf("the %s track", m.App)
	}
	out.AttributionUnreliable = fmt.Sprintf(
		"%s has a peak-to-average ratio of %.1f dB, and speech runs 15 to 20. "+
			"It did not capture speech, so anything the microphone picked up acoustically "+
			"has nothing to be compared against and is labelled as you. "+
			"%d line(s) below carry that label and may be the far end.",
		what, crest, micLines)
	log("WARNING: %s", out.AttributionUnreliable)
}

// trackNamed finds a track's format in the manifest.
func trackNamed(m *manifest.Manifest, name string) (manifest.Track, bool) {
	for _, t := range m.Tracks {
		if t.Name == name {
			return t, true
		}
	}
	return manifest.Track{}, false
}

// Sort orders lines by when they were said. Ties break by track so the result
// is stable, which matters because a transcript that reorders itself between
// runs is impossible to diff.
func Sort(lines []Line) {
	sort.SliceStable(lines, func(i, j int) bool {
		if lines[i].Start != lines[j].Start {
			return lines[i].Start < lines[j].Start
		}
		return lines[i].Track < lines[j].Track
	})
}

// Write saves the transcript beside the audio, as JSON and as text.
func (t *Transcript) Write(dir string) error {
	body, err := json.MarshalIndent(t, "", "  ")
	if err != nil {
		return err
	}
	body = append(body, '\n')
	if err := writeAtomic(filepath.Join(dir, JSONName), body); err != nil {
		return err
	}
	return writeAtomic(filepath.Join(dir, TextName), []byte(t.Text()))
}

func writeAtomic(path string, body []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, body, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Text renders the transcript for a person to read.
func (t *Transcript) Text() string {
	var b strings.Builder
	fmt.Fprintf(&b, "# %s\n", t.RecordingID)
	if t.Name != "" {
		fmt.Fprintf(&b, "# %s\n", t.Name)
	}
	// Said in the transcript itself, not only in the metadata: whoever reads
	// this should not have to go looking to find out it was recorded.
	b.WriteString("#\n# This meeting was recorded.\n")
	fmt.Fprintf(&b, "# Transcribed by %s; audio %s this machine.\n\n",
		t.Backend, map[bool]string{true: "was sent off", false: "stayed on"}[t.AudioLeftMachine])

	if t.AttributionUnreliable != "" {
		banner := "# ⚠ SPEAKER LABELS BELOW ARE NOT RELIABLE"
		if t.Unattributed {
			banner = "# ⚠ THIS RECORDING CANNOT SAY WHO SPOKE"
		}
		b.WriteString("#\n" + banner + "\n#\n")
		for _, line := range strings.Split(wrapAt(t.AttributionUnreliable, 72), "\n") {
			fmt.Fprintf(&b, "# %s\n", line)
		}
		b.WriteString("#\n")
	}
	// Above the withheld notice, because this is about what is not here at all
	// rather than about what was set aside with a reason.
	if t.MicrophoneLost {
		b.WriteString("#\n# \u26a0 YOUR OWN AUDIO IS MISSING FROM THIS RECORDING\n#\n")
		for _, line := range strings.Split(wrapAt("Nothing was captured from the microphone, so "+
			"everything you said is absent. The speaker labels below are correct — what is "+
			"here really was the other side — but this is one half of a conversation, and a "+
			"transcript in which you never speak looks exactly like a meeting in which you "+
			"did not.", 72), "\n") {
			fmt.Fprintf(&b, "# %s\n", line)
		}
		b.WriteString("#\n")
	}
	if len(t.Withheld) > 0 {
		fmt.Fprintf(&b, "# %d line(s) were withheld — echoes of the far end, or spans the model\n"+
			"# said were not speech. They are in %s with the reason for each.\n\n",
			len(t.Withheld), JSONName)
	}
	if t.SilentSegments > 0 {
		fmt.Fprintf(&b, "# %d segment(s) were never transcribed: their peak was below %d dBFS,\n"+
			"# which is quieter than a room. That part of the meeting is not below.\n\n",
			t.SilentSegments, silenceFloorDBFS)
	}
	if len(t.FarEndSilent) > 0 {
		fmt.Fprintf(&b, "# %d stretch(es) below are marked: the other side was silent, so what\n"+
			"# the microphone picked up there may be the room rather than the meeting.\n\n", len(t.FarEndSilent))
	}

	marked := map[int]Silence{}
	for _, g := range t.FarEndSilent {
		for i, l := range t.Lines {
			if l.Start >= g.Start {
				if _, seen := marked[i]; !seen {
					marked[i] = g
				}
				break
			}
		}
	}
	for i, l := range t.Lines {
		if g, ok := marked[i]; ok {
			fmt.Fprintf(&b, "\n%s\n", describeSilence(g))
		}
		fmt.Fprintf(&b, "[%s] %-14s %s\n", clock(l.Start), SpeakerLabel(l.Speaker)+":", l.Text)
	}
	return b.String()
}

// wrapAt breaks text so a warning in a comment block stays readable.
func wrapAt(s string, width int) string {
	var out strings.Builder
	line := 0
	for i, w := range strings.Fields(s) {
		if line > 0 && line+1+len(w) > width {
			out.WriteString("\n")
			line = 0
		} else if i > 0 && line > 0 {
			out.WriteString(" ")
			line++
		}
		out.WriteString(w)
		line += len(w)
	}
	return out.String()
}

func clock(seconds float64) string {
	if seconds < 0 {
		seconds = 0
	}
	total := int(seconds)
	return fmt.Sprintf("%02d:%02d:%02d", total/3600, (total/60)%60, total%60)
}

// Load reads a transcript previously written into a recording directory.
func Load(dir string) (*Transcript, error) {
	body, err := os.ReadFile(filepath.Join(dir, JSONName))
	if err != nil {
		return nil, err
	}
	var t Transcript
	if err := json.Unmarshal(body, &t); err != nil {
		return nil, fmt.Errorf("transcript in %s is unreadable: %w", dir, err)
	}
	return &t, nil
}
