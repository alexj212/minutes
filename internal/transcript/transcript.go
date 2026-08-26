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

	"github.com/alexj/minutes/internal/manifest"
	"github.com/alexj/minutes/internal/transcribe"
	"github.com/alexj/minutes/internal/wav"
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
	// Invented counts spans the model itself flagged as probably not speech,
	// whose words were discarded rather than attributed to anybody.
	Invented int `json:"invented,omitempty"`
	// FarEndSilent lists stretches where only the microphone carried speech.
	FarEndSilent []Silence `json:"farEndSilent,omitempty"`
	Lines        []Line    `json:"lines"`
}

// speakerFor maps a track name to who is on it.
func speakerFor(track string) string {
	if track == "mic" {
		return SpeakerYou
	}
	return SpeakerOthers
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
	var invented int
	for i, j := range jobs {
		for _, u := range results[i] {
			// The model said it was probably not listening to speech. Believe
			// it, rather than publishing what it invented anyway.
			if u.NoSpeechProb >= noSpeechThreshold {
				invented++
				continue
			}
			out.Lines = append(out.Lines, Line{
				// Segment-relative times become absolute here. This is the
				// only place the two tracks are related to each other, and it
				// is arithmetic because the segments were cut on a shared
				// clock in the first place.
				Start:   j.start + u.Start,
				End:     j.start + u.End,
				Track:   j.track,
				Speaker: speakerFor(j.track),
				Text:    u.Text,
			})
		}
	}
	if invented > 0 {
		out.Invented = invented
		log("discarded %d line(s) the model itself flagged as probably not speech: "+
			"given silence it invents a plausible sentence and hands it over without a hedge, "+
			"and those words would have been attributed to somebody", invented)
	}

	Sort(out.Lines)

	kept, dropped := SuppressBleed(out.Lines)
	out.Lines = kept

	// Second pass, by level rather than by words. A fragment of the far end
	// whose words never made it into the far-end transcript cannot be matched
	// textually, but it is still quieter than speech into the microphone.
	if micTrack, ok := trackNamed(m, "mic"); ok {
		lr := newLevelReader(m.Dir(), m.SegmentSeconds, micTrack)
		if ref, ok := referenceLevel(out.Lines, lr); ok {
			quiet, n := suppressQuietFragments(out.Lines, ref,
				func(l Line) (float64, bool) { return lr.peakDBFS(l.Start, l.End) })
			if n > 0 {
				out.Lines = quiet
				dropped += n
				log("dropped %d quiet microphone fragment(s): %.0f dB or more below your speaking "+
					"level while the other side was talking, which is the far end arriving through the air", n, quietMarginDB)
			}
		}
	}

	if dropped > 0 {
		out.BleedSuppressed = dropped
		log("suppressed %d microphone line(s) as echoes of the system track: "+
			"this meeting was on speakers rather than headphones, so the microphone "+
			"also heard the far end", dropped)
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
		fmt.Fprintf(&b, "[%s] %-6s %s\n", clock(l.Start), l.Speaker+":", l.Text)
	}
	return b.String()
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
