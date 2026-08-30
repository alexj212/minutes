package transcript

import (
	"context"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alexj212/minutes/internal/manifest"
	"github.com/alexj212/minutes/internal/transcribe"
	"github.com/alexj212/minutes/internal/wav"
)

// fakeTranscriber returns canned utterances per file, and records what it was
// asked to transcribe — which is how the silence-skipping is checked.
type fakeTranscriber struct {
	byFile map[string][]transcribe.Utterance
	asked  []string
	remote bool
}

func (f *fakeTranscriber) Name() string               { return "fake" }
func (f *fakeTranscriber) SendsAudioOffMachine() bool { return f.remote }
func (f *fakeTranscriber) Transcribe(_ context.Context, paths []string) ([][]transcribe.Utterance, error) {
	out := make([][]transcribe.Utterance, len(paths))
	for i, p := range paths {
		f.asked = append(f.asked, filepath.Base(p))
		out[i] = f.byFile[filepath.Base(p)]
	}
	return out, nil
}

// buildFixture writes a recording directory with the given segments and returns
// its manifest.
//
// The segment files are real WAVs, because Build reads them: it trims leading
// silence before handing audio to a speech model, and a fixture of arbitrary
// bytes would not exercise that.
func buildFixture(t *testing.T, segs map[string][]manifest.Segment) *manifest.Manifest {
	t.Helper()
	return buildFixtureWithSilence(t, segs, nil)
}

// buildFixtureWithSilence writes fixtures where named files open with the given
// number of seconds of digital silence.
func buildFixtureWithSilence(t *testing.T, segs map[string][]manifest.Segment, silence map[string]float64) *manifest.Manifest {
	t.Helper()
	dir := t.TempDir()
	m := manifest.New(dir, "rec-1", "standup", 10)
	for track, list := range segs {
		rate := 48000
		if track == "system" {
			rate = 44100
		}
		if err := m.SetTrack(track, track+" device", rate, 2); err != nil {
			t.Fatal(err)
		}
		for _, s := range list {
			writeWAV(t, filepath.Join(dir, s.File), rate, 2, silence[s.File])
			if err := m.PutSegment(track, s); err != nil {
				t.Fatal(err)
			}
		}
	}
	return m
}

// writeWAV writes leadSilence seconds of digital silence followed by a second
// of tone.
func writeWAV(t *testing.T, path string, rate, channels int, leadSilence float64) {
	t.Helper()
	w, err := wav.NewWriter(path, rate, channels)
	if err != nil {
		t.Fatal(err)
	}
	silentFrames := int(leadSilence * float64(rate))
	tone := make([]int16, rate*channels)
	for i := range tone {
		tone[i] = int16(1000 + i%500)
	}
	if err := w.WriteAt(uint64(silentFrames), tone); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
}

// A segment's utterance times are relative to that segment. Turning them into
// positions on the recording's timeline is the step that makes the two tracks
// comparable at all, so it is worth pinning down.
func TestSegmentRelativeTimesBecomeAbsolute(t *testing.T) {
	m := buildFixture(t, map[string][]manifest.Segment{
		"mic": {{Index: 1, File: "mic-001.wav", StartSeconds: 10, Frames: 480000, PeakDBFS: -8}},
	})
	fake := &fakeTranscriber{byFile: map[string][]transcribe.Utterance{
		"mic-001.wav": {{Start: 2.5, End: 4.0, Text: "hello"}},
	}}

	tr, err := Build(context.Background(), m, fake, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(tr.Lines) != 1 {
		t.Fatalf("got %d lines, want 1", len(tr.Lines))
	}
	if got, want := tr.Lines[0].Start, 12.5; got != want {
		t.Errorf("Start = %v, want %v (segment start 10 + utterance 2.5)", got, want)
	}
	if got, want := tr.Lines[0].End, 14.0; got != want {
		t.Errorf("End = %v, want %v", got, want)
	}
}

// Your track is you; the other track is everyone else. That is the entire
// return on never mixing them.
func TestTracksAreAttributedAndOrdered(t *testing.T) {
	m := buildFixture(t, map[string][]manifest.Segment{
		"mic":    {{Index: 0, File: "mic-000.wav", StartSeconds: 0, Frames: 1, PeakDBFS: -8}},
		"system": {{Index: 0, File: "system-000.wav", StartSeconds: 0, Frames: 1, PeakDBFS: -8}},
	})
	fake := &fakeTranscriber{byFile: map[string][]transcribe.Utterance{
		"mic-000.wav":    {{Start: 5, End: 6, Text: "my second thing"}, {Start: 1, End: 2, Text: "my first thing"}},
		"system-000.wav": {{Start: 3, End: 4, Text: "their thing in between"}},
	}}

	tr, err := Build(context.Background(), m, fake, nil)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, l := range tr.Lines {
		got = append(got, l.Speaker+": "+l.Text)
	}
	want := []string{
		"You: my first thing",
		"Others: their thing in between",
		"You: my second thing",
	}
	if len(got) != len(want) {
		t.Fatalf("got %d lines %v, want %d", len(got), got, len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("line %d = %q, want %q", i, got[i], want[i])
		}
	}
}

// A speech model given silence invents speech. Skipping silent segments is what
// keeps "Thank you." out of the notes as something somebody said.
func TestSilentSegmentsAreNotSentToTheModel(t *testing.T) {
	m := buildFixture(t, map[string][]manifest.Segment{
		"system": {
			{Index: 0, File: "system-000.wav", StartSeconds: 0, Frames: 100, PeakDBFS: -999},
			{Index: 1, File: "system-001.wav", StartSeconds: 10, Frames: 100, PeakDBFS: -12},
		},
	})
	fake := &fakeTranscriber{byFile: map[string][]transcribe.Utterance{
		"system-001.wav": {{Start: 0, End: 1, Text: "real speech"}},
	}}

	if _, err := Build(context.Background(), m, fake, nil); err != nil {
		t.Fatal(err)
	}
	for _, asked := range fake.asked {
		if asked == "system-000.wav" {
			t.Error("a silent segment was sent to the model")
		}
	}
	if len(fake.asked) != 1 {
		t.Errorf("model was asked for %v, want only the segment with audio", fake.asked)
	}
}

func TestNothingAudibleIsAnError(t *testing.T) {
	m := buildFixture(t, map[string][]manifest.Segment{
		"system": {{Index: 0, File: "system-000.wav", Frames: 100, PeakDBFS: -999}},
	})
	if _, err := Build(context.Background(), m, &fakeTranscriber{}, nil); err == nil {
		t.Error("a recording with nothing audible transcribed without complaint")
	}
}

// Whether a meeting was sent to a third party has to survive into the artifact,
// because it is a question somebody may have to answer later.
func TestTranscriptRecordsWhereTheAudioWent(t *testing.T) {
	m := buildFixture(t, map[string][]manifest.Segment{
		"mic": {{Index: 0, File: "mic-000.wav", Frames: 1, PeakDBFS: -8}},
	})
	fake := &fakeTranscriber{
		byFile: map[string][]transcribe.Utterance{"mic-000.wav": {{Text: "x"}}},
		remote: true,
	}
	tr, err := Build(context.Background(), m, fake, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !tr.AudioLeftMachine {
		t.Error("a hosted backend produced a transcript that does not say the audio left")
	}
	if !strings.Contains(tr.Text(), "was sent off this machine") {
		t.Errorf("the readable transcript does not say the audio left:\n%s", tr.Text())
	}
	if !strings.Contains(tr.Text(), "This meeting was recorded.") {
		t.Error("the readable transcript does not say the meeting was recorded")
	}
}

func TestWriteProducesBothFiles(t *testing.T) {
	dir := t.TempDir()
	tr := &Transcript{RecordingID: "rec-1", Lines: []Line{{Start: 65, Speaker: "You", Text: "hello"}}}
	if err := tr.Write(dir); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(filepath.Join(dir, JSONName))
	if err != nil {
		t.Fatal(err)
	}
	var back Transcript
	if err := json.Unmarshal(body, &back); err != nil {
		t.Fatalf("transcript.json is not valid JSON: %v", err)
	}
	if len(back.Lines) != 1 || back.Lines[0].Text != "hello" {
		t.Errorf("round trip lost the lines: %+v", back.Lines)
	}
	text, err := os.ReadFile(filepath.Join(dir, TextName))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(text), "[00:01:05]") {
		t.Errorf("timestamp not rendered as a clock:\n%s", text)
	}
}

// A speech model given a file that opens with silence anchors its first
// utterance at zero rather than where the speech is. The silence is therefore
// trimmed before the model sees it, and the amount trimmed has to come back —
// otherwise the opening line of the system track, which begins with the idle
// lead in every recording, lands at the start of the meeting instead of where
// it was said.
func TestTrimmedLeadingSilenceIsAddedBackToTimestamps(t *testing.T) {
	m := buildFixtureWithSilence(t,
		map[string][]manifest.Segment{
			"system": {{Index: 0, File: "system-000.wav", StartSeconds: 0, Frames: 100, PeakDBFS: -8}},
		},
		map[string]float64{"system-000.wav": 5.0},
	)
	// The model sees trimmed audio and reports the speech at its very start,
	// which is exactly the behaviour being corrected for.
	fake := &fakeTranscriber{byFile: map[string][]transcribe.Utterance{
		"system-000.wav": {{Start: 0, End: 1, Text: "the first thing anybody said"}},
	}}

	tr, err := Build(context.Background(), m, fake, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(tr.Lines) != 1 {
		t.Fatalf("got %d lines, want 1", len(tr.Lines))
	}
	if got := tr.Lines[0].Start; got < 4.9 || got > 5.1 {
		t.Errorf("line placed at %.2fs, want about 5.0 — the trimmed silence was not added back", got)
	}
}

// A speech model given silence does not return nothing. It invents a plausible
// sentence and hands it over with no hedge, and the recorder then puts those
// words in somebody's mouth.
//
// Measured on the target machine: a microphone track at -56 dBFS produced one
// nine-second line — "Department of Education." — attributed to the operator,
// on a recording where the microphone had captured nothing at all. Whisper
// reported no_speech_prob 0.908 for it, and this pipeline was discarding that
// field. Real speech reports 0.001.
func TestModelFlaggedNonSpeechIsDiscarded(t *testing.T) {
	m := buildFixture(t, map[string][]manifest.Segment{
		"mic": {{Index: 0, File: "mic-000.wav", StartSeconds: 0, Frames: 100, PeakDBFS: -55.7}},
	})
	fake := &fakeTranscriber{byFile: map[string][]transcribe.Utterance{
		"mic-000.wav": {
			{Start: 1, End: 10, Text: "Department of Education.", NoSpeechProb: 0.908},
			{Start: 11, End: 13, Text: "something actually said", NoSpeechProb: 0.001},
		},
	}}

	tr, err := Build(context.Background(), m, fake, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range tr.Lines {
		if l.Text == "Department of Education." {
			t.Error("published a line the model itself said was probably not speech")
		}
	}
	if len(tr.Lines) != 1 || tr.Lines[0].Text != "something actually said" {
		t.Fatalf("lines = %+v, want only the real one", tr.Lines)
	}
	if tr.ModelDoubted != 1 {
		t.Errorf("ModelDoubted = %d, want 1 — a discarded line must be counted, not hidden", tr.ModelDoubted)
	}
}

// Confident output must survive. Over-filtering would lose real speech, which
// is a worse trade than an occasional invention.
func TestConfidentSpeechIsKept(t *testing.T) {
	m := buildFixture(t, map[string][]manifest.Segment{
		"system": {{Index: 0, File: "system-000.wav", StartSeconds: 0, Frames: 100, PeakDBFS: -8}},
	})
	fake := &fakeTranscriber{byFile: map[string][]transcribe.Utterance{
		"system-000.wav": {
			{Start: 0, End: 2, Text: "we ship on Thursday", NoSpeechProb: 0.001},
			{Start: 2, End: 4, Text: "and Karyn owns the notes", NoSpeechProb: 0.4},
		},
	}}
	tr, err := Build(context.Background(), m, fake, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(tr.Lines) != 2 {
		t.Fatalf("kept %d lines, want 2 — confident speech was filtered", len(tr.Lines))
	}
	if tr.ModelDoubted != 0 {
		t.Errorf("ModelDoubted = %d, want 0", tr.ModelDoubted)
	}
}

// Three passes can now remove a line — an echo of the far end, a quiet fragment
// of it, and a span the model said was not speech. A count alone gives nobody a
// way to check the judgment, and a transcript that cannot show its own
// omissions is asking to be trusted rather than read.
func TestWithheldLinesAreKeptWithTheirReason(t *testing.T) {
	m := buildFixture(t, map[string][]manifest.Segment{
		"mic": {{Index: 0, File: "mic-000.wav", StartSeconds: 0, Frames: 100, PeakDBFS: -8}},
	})
	fake := &fakeTranscriber{byFile: map[string][]transcribe.Utterance{
		"mic-000.wav": {
			{Start: 1, End: 10, Text: "Department of Education.", NoSpeechProb: 0.908},
			{Start: 11, End: 13, Text: "something actually said", NoSpeechProb: 0.001},
		},
	}}

	tr, err := Build(context.Background(), m, fake, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(tr.Lines) != 1 {
		t.Fatalf("kept %d lines, want 1", len(tr.Lines))
	}
	if len(tr.Withheld) != 1 {
		t.Fatalf("withheld %d lines, want 1 — a dropped line must remain inspectable", len(tr.Withheld))
	}
	w := tr.Withheld[0]
	if w.Text != "Department of Education." {
		t.Errorf("withheld the wrong line: %q", w.Text)
	}
	if w.Suppressed == "" {
		t.Error("a withheld line does not say why it went")
	}
	if !strings.Contains(w.Suppressed, "no speech") {
		t.Errorf("the reason does not explain itself: %q", w.Suppressed)
	}

	// And the readable transcript must say some exist, without printing them.
	text := tr.Text()
	if strings.Contains(text, "Department of Education") {
		t.Error("a withheld line appeared in the readable transcript")
	}
	if !strings.Contains(text, "withheld") {
		t.Errorf("the readable transcript does not mention that lines were withheld:\n%s", text)
	}
}

// An echo removed from the microphone track must be inspectable too, and say
// what it was.
func TestSuppressedEchoesAreRecorded(t *testing.T) {
	m := buildFixture(t, map[string][]manifest.Segment{
		"mic":    {{Index: 0, File: "mic-000.wav", StartSeconds: 0, Frames: 100, PeakDBFS: -8}},
		"system": {{Index: 0, File: "system-000.wav", StartSeconds: 0, Frames: 100, PeakDBFS: -8}},
	})
	fake := &fakeTranscriber{byFile: map[string][]transcribe.Utterance{
		"system-000.wav": {{Start: 4, End: 8, Text: "We agreed to ship the recorder on Thursday."}},
		"mic-000.wav":    {{Start: 4, End: 8, Text: "we agreed to ship the recorder on thursday"}},
	}}
	tr, err := Build(context.Background(), m, fake, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(tr.Withheld) != 1 {
		t.Fatalf("withheld %d, want the echo", len(tr.Withheld))
	}
	if !strings.Contains(tr.Withheld[0].Suppressed, "echo") {
		t.Errorf("reason = %q, want it to name the echo", tr.Withheld[0].Suppressed)
	}
	if tr.Withheld[0].Track != "mic" {
		t.Errorf("withheld the %s track; only the microphone copy should go", tr.Withheld[0].Track)
	}
}

// A model can report an end past the end of the audio. Observed on a real
// recording: a span stamped 12.06s on a 9.98s file, and later 10.083s on a
// 9.973s one. Harmless where it was seen, and not harmless at all for anything
// that seeks to those offsets.
func TestLineEndIsClampedToTheTrack(t *testing.T) {
	m := buildFixture(t, map[string][]manifest.Segment{
		"mic": {{Index: 0, File: "mic-000.wav", StartSeconds: 0,
			DurationSeconds: 9.973, Frames: 478704, PeakDBFS: -8}},
	})
	fake := &fakeTranscriber{byFile: map[string][]transcribe.Utterance{
		"mic-000.wav": {{Start: 5.14, End: 12.06, Text: "runs past the end", NoSpeechProb: 0.001}},
	}}
	tr, err := Build(context.Background(), m, fake, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(tr.Lines) != 1 {
		t.Fatalf("got %d lines, want 1", len(tr.Lines))
	}
	if got := tr.Lines[0].End; got > 9.973 {
		t.Errorf("line ends at %.3f on a 9.973s track — %.3fs past the audio it describes", got, got-9.973)
	}
	if tr.Lines[0].Start != 5.14 {
		t.Errorf("start moved to %v; only the end should be clamped", tr.Lines[0].Start)
	}
}

// Speaker attribution rests entirely on the two tracks holding different audio.
// If the far-end track did not capture the far end — the wrong application was
// targeted, or it was muted — then the microphone's acoustic pickup of the far
// end has nothing to be compared against and is published as the operator.
// Real words, wrong mouth, and the operator's is the worst one to get wrong.
//
// Measured on a real recording: a track that had captured a 440 Hz tone rather
// than a meeting showed a 3.0 dB peak-to-average ratio, against 15-20 for
// speech, and two lines of room echo were labelled "You".
func TestAttributionIsFlaggedWhenTheFarEndHoldsNoSpeech(t *testing.T) {
	dir := t.TempDir()
	m := manifest.New(dir, "rec", "wrong target", 60)
	m.App = "powershell.exe"
	if err := m.SetTrack("system", "process 1 (powershell.exe)", 48000, 2); err != nil {
		t.Fatal(err)
	}
	if err := m.SetTrack("mic", "Mic", 48000, 2); err != nil {
		t.Fatal(err)
	}
	// A pure tone on the far-end track: steady, so a 3 dB crest.
	writeTone(t, filepath.Join(dir, "system-000.wav"), 48000, 2, 6, 440, 0.5)
	writeTone(t, filepath.Join(dir, "mic-000.wav"), 48000, 2, 6, 300, 0.5)
	for _, name := range []string{"system", "mic"} {
		if err := m.PutSegment(name, manifest.Segment{
			Index: 0, File: name + "-000.wav", StartSeconds: 0,
			DurationSeconds: 6, Frames: 288000, PeakDBFS: -6, Complete: true,
		}); err != nil {
			t.Fatal(err)
		}
	}
	fake := &fakeTranscriber{byFile: map[string][]transcribe.Utterance{
		"mic-000.wav": {{Start: 0, End: 3, Text: "words the room heard", NoSpeechProb: 0.001}},
	}}

	tr, err := Build(context.Background(), m, fake, nil)
	if err != nil {
		t.Fatal(err)
	}
	if tr.AttributionUnreliable == "" {
		t.Fatal("a far-end track holding a steady tone did not flag attribution as unreliable")
	}
	for _, want := range []string{"peak-to-average", "labelled as you"} {
		if !strings.Contains(tr.AttributionUnreliable, want) {
			t.Errorf("the warning does not mention %q: %s", want, tr.AttributionUnreliable)
		}
	}
	if !strings.Contains(tr.Text(), "NOT RELIABLE") {
		t.Error("the readable transcript does not carry the warning")
	}
}

// And speech on the far-end track must not trip it, or the warning becomes
// noise and gets ignored on the recording that needs it.
func TestSpeechLikeFarEndDoesNotFlagAttribution(t *testing.T) {
	dir := t.TempDir()
	m := manifest.New(dir, "rec", "ok", 60)
	if err := m.SetTrack("system", "Speakers", 48000, 2); err != nil {
		t.Fatal(err)
	}
	if err := m.SetTrack("mic", "Mic", 48000, 2); err != nil {
		t.Fatal(err)
	}
	writeSpeechLike(t, filepath.Join(dir, "system-000.wav"), 48000, 2, 6)
	writeTone(t, filepath.Join(dir, "mic-000.wav"), 48000, 2, 6, 300, 0.5)
	for _, name := range []string{"system", "mic"} {
		if err := m.PutSegment(name, manifest.Segment{
			Index: 0, File: name + "-000.wav", StartSeconds: 0,
			DurationSeconds: 6, Frames: 288000, PeakDBFS: -6, Complete: true,
		}); err != nil {
			t.Fatal(err)
		}
	}
	fake := &fakeTranscriber{byFile: map[string][]transcribe.Utterance{
		"mic-000.wav": {{Start: 0, End: 3, Text: "something", NoSpeechProb: 0.001}},
	}}
	tr, err := Build(context.Background(), m, fake, nil)
	if err != nil {
		t.Fatal(err)
	}
	if tr.AttributionUnreliable != "" {
		t.Errorf("a speech-like far-end track was flagged: %s", tr.AttributionUnreliable)
	}
}

// writeTone writes a steady sine: peak-to-average 3.01 dB by construction.
func writeTone(t *testing.T, path string, rate, channels int, secs float64, hz, amp float64) {
	t.Helper()
	w, err := wav.NewWriter(path, rate, channels)
	if err != nil {
		t.Fatal(err)
	}
	n := int(secs * float64(rate))
	buf := make([]int16, n*channels)
	for i := 0; i < n; i++ {
		v := int16(amp * 32767 * math.Sin(2*math.Pi*hz*float64(i)/float64(rate)))
		for c := 0; c < channels; c++ {
			buf[i*channels+c] = v
		}
	}
	if err := w.WriteAt(0, buf); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
}

// writeSpeechLike writes bursts separated by near-silence, which is what gives
// speech its 15-20 dB peak-to-average ratio.
func writeSpeechLike(t *testing.T, path string, rate, channels int, secs float64) {
	t.Helper()
	w, err := wav.NewWriter(path, rate, channels)
	if err != nil {
		t.Fatal(err)
	}
	n := int(secs * float64(rate))
	buf := make([]int16, n*channels)
	for i := 0; i < n; i++ {
		// Loud for a twentieth of each period, quiet for the rest.
		phase := float64(i%(rate/2)) / float64(rate/2)
		amp := 0.004
		if phase < 0.05 {
			amp = 0.9
		}
		v := int16(amp * 32767 * math.Sin(2*math.Pi*220*float64(i)/float64(rate)))
		for c := 0; c < channels; c++ {
			buf[i*channels+c] = v
		}
	}
	if err := w.WriteAt(0, buf); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
}

// A withheld span must carry the model's raw judgement, not only a sentence
// with the number formatted into it.
//
// A reviewer found a block reporting exactly "0.600" — the threshold, to three
// decimals, on two spans at once — and asked whether it was measured or
// synthesized. The formatted string could not answer that. Real values look
// like 0.0006066110800020397, so the raw number is the evidence.
func TestWithheldSpanCarriesTheRawProbability(t *testing.T) {
	m := buildFixture(t, map[string][]manifest.Segment{
		"mic": {{Index: 0, File: "mic-000.wav", StartSeconds: 0,
			DurationSeconds: 10, Frames: 480000, PeakDBFS: -8}},
	})
	const raw = 0.9083104133605957
	fake := &fakeTranscriber{byFile: map[string][]transcribe.Utterance{
		"mic-000.wav": {{Start: 1, End: 9, Text: "Department of Education.", NoSpeechProb: raw}},
	}}
	tr, err := Build(context.Background(), m, fake, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(tr.Withheld) != 1 {
		t.Fatalf("withheld %d, want 1", len(tr.Withheld))
	}
	if got := tr.Withheld[0].NoSpeechProb; got != raw {
		t.Errorf("recorded %v, want the model's own %v — precision was lost", got, raw)
	}
}

// The robust test must decide before the fragile one.
//
// Measured on a real recording: an echo of the far end was withheld on a
// no-speech probability of 0.6001495718955994 against a 0.6 threshold — a
// margin of 0.00015. The outcome was right and it was right by luck. A slightly
// different room or microphone gain flips it, and the far end is published as
// the operator.
//
// So a line that is both an echo and marginally doubted must be withheld as an
// echo, by the test built for it, not by a coin-flip on a threshold.
func TestEchoDecidesBeforeMarginalModelDoubt(t *testing.T) {
	m := buildFixture(t, map[string][]manifest.Segment{
		"mic":    {{Index: 0, File: "mic-000.wav", StartSeconds: 0, DurationSeconds: 10, Frames: 480000, PeakDBFS: -8}},
		"system": {{Index: 0, File: "system-000.wav", StartSeconds: 0, DurationSeconds: 10, Frames: 441000, PeakDBFS: -8}},
	})
	const marginal = 0.6001495718955994
	fake := &fakeTranscriber{byFile: map[string][]transcribe.Utterance{
		"system-000.wav": {{Start: 0, End: 6, Text: "We agree to ship the recorder on Thursday and Karen owns the migration notes.", NoSpeechProb: 0.001}},
		"mic-000.wav":    {{Start: 0, End: 6, Text: "We agree to ship the recorder on Thursday and Karen owns the migration notes.", NoSpeechProb: marginal}},
	}}

	tr, err := Build(context.Background(), m, fake, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(tr.Withheld) != 1 {
		t.Fatalf("withheld %d, want 1", len(tr.Withheld))
	}
	if !strings.Contains(tr.Withheld[0].Suppressed, "echo") {
		t.Errorf("withheld for %q — a marginal probability decided a case the echo test owns",
			tr.Withheld[0].Suppressed)
	}
	if tr.ModelDoubted != 0 {
		t.Errorf("ModelDoubted = %d; the echo test should have claimed it first", tr.ModelDoubted)
	}
	// The raw judgement still travels, so the margin is inspectable.
	if tr.Withheld[0].NoSpeechProb != marginal {
		t.Errorf("the model's value was lost: %v", tr.Withheld[0].NoSpeechProb)
	}
}

// And invention over silence still has to be caught, because there is no far
// end to compare it against and nothing else can catch it.
func TestModelDoubtStillCatchesInventionOverSilence(t *testing.T) {
	m := buildFixture(t, map[string][]manifest.Segment{
		"mic": {{Index: 0, File: "mic-000.wav", StartSeconds: 0, DurationSeconds: 10, Frames: 480000, PeakDBFS: -55}},
	})
	fake := &fakeTranscriber{byFile: map[string][]transcribe.Utterance{
		"mic-000.wav": {{Start: 1, End: 10, Text: "Department of Education.", NoSpeechProb: 0.908}},
	}}
	tr, err := Build(context.Background(), m, fake, nil)
	if err != nil {
		t.Fatal(err)
	}
	if tr.ModelDoubted != 1 {
		t.Fatalf("ModelDoubted = %d, want 1 — nothing else can catch invention over silence", tr.ModelDoubted)
	}
	if len(tr.Lines) != 0 {
		t.Errorf("published %d invented line(s)", len(tr.Lines))
	}
}

// A microphone track means "the operator" only because the other track holds
// everyone else. With no far-end track that inference collapses: the microphone
// holds whoever was audible in the room, and labelling all of it "You" puts
// other people's words in the operator's mouth.
//
// Reachable today with `minutes record --mic-only`, and it is the normal case
// for any device that cannot capture system audio at all.
func TestOneSourceRecordingCannotSayWhoSpoke(t *testing.T) {
	m := buildFixture(t, map[string][]manifest.Segment{
		"mic": {{Index: 0, File: "mic-000.wav", StartSeconds: 0,
			DurationSeconds: 10, Frames: 480000, PeakDBFS: -8}},
	})
	fake := &fakeTranscriber{byFile: map[string][]transcribe.Utterance{
		"mic-000.wav": {
			{Start: 0, End: 3, Text: "shall we start", NoSpeechProb: 0.001},
			{Start: 3, End: 6, Text: "yes go ahead", NoSpeechProb: 0.001},
		},
	}}

	tr, err := Build(context.Background(), m, fake, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(tr.Lines) != 2 {
		t.Fatalf("got %d lines, want 2", len(tr.Lines))
	}
	for _, l := range tr.Lines {
		if l.Speaker == SpeakerYou {
			t.Errorf("line %q was attributed to the operator on a one-source recording", l.Text)
		}
		if l.Speaker != SpeakerUnattributed {
			t.Errorf("speaker = %q, want %q", l.Speaker, SpeakerUnattributed)
		}
	}
	if !tr.Unattributed {
		t.Error("the transcript does not record that it carries no speaker labels")
	}

	// And the readable form must say so where somebody reading it will see it.
	text := tr.Text()
	if !strings.Contains(text, "CANNOT SAY WHO SPOKE") {
		t.Errorf("the readable transcript does not say it cannot attribute:\n%s", text)
	}
	if strings.Contains(text, "] You:") {
		t.Error("the readable transcript still labels a line as the operator")
	}
}

// A declared track that was never written to is the same thing as no track.
// Treating them differently would be the empty-versus-unknown mistake again.
func TestFarEndTrackWithNoAudioCountsAsAbsent(t *testing.T) {
	m := buildFixture(t, map[string][]manifest.Segment{
		"mic":    {{Index: 0, File: "mic-000.wav", StartSeconds: 0, DurationSeconds: 10, Frames: 480000, PeakDBFS: -8}},
		"system": {{Index: 0, File: "system-000.wav", StartSeconds: 0, DurationSeconds: 10, Frames: 0, PeakDBFS: -999}},
	})
	fake := &fakeTranscriber{byFile: map[string][]transcribe.Utterance{
		"mic-000.wav": {{Start: 0, End: 3, Text: "something", NoSpeechProb: 0.001}},
	}}
	tr, err := Build(context.Background(), m, fake, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !tr.Unattributed {
		t.Error("a far-end track that captured nothing was treated as a far end")
	}
	if tr.Lines[0].Speaker != SpeakerUnattributed {
		t.Errorf("speaker = %q, want unattributed", tr.Lines[0].Speaker)
	}
}

// And a genuine two-track recording must still attribute, or the fix has simply
// removed the feature.
func TestTwoTrackRecordingStillAttributes(t *testing.T) {
	m := buildFixture(t, map[string][]manifest.Segment{
		"mic":    {{Index: 0, File: "mic-000.wav", StartSeconds: 0, DurationSeconds: 10, Frames: 480000, PeakDBFS: -8}},
		"system": {{Index: 0, File: "system-000.wav", StartSeconds: 0, DurationSeconds: 10, Frames: 441000, PeakDBFS: -8}},
	})
	fake := &fakeTranscriber{byFile: map[string][]transcribe.Utterance{
		"mic-000.wav":    {{Start: 0, End: 3, Text: "my own words entirely", NoSpeechProb: 0.001}},
		"system-000.wav": {{Start: 4, End: 7, Text: "and something quite different", NoSpeechProb: 0.001}},
	}}
	tr, err := Build(context.Background(), m, fake, nil)
	if err != nil {
		t.Fatal(err)
	}
	if tr.Unattributed {
		t.Fatal("a two-track recording was stripped of its labels")
	}
	var you, others int
	for _, l := range tr.Lines {
		switch l.Speaker {
		case SpeakerYou:
			you++
		case SpeakerOthers:
			others++
		}
	}
	if you != 1 || others != 1 {
		t.Errorf("got %d you and %d others, want one each", you, others)
	}
}

// A track name this build does not recognise must not be attributed to anybody.
//
// speakerFor returned "Others" for everything that was not the microphone, so a
// source called anything else — a room recording, a future track kind, a typo
// in a manifest — was published as the far end on the basis of a name not
// matching. Guessing here puts somebody's words in somebody else's mouth;
// refusing leaves a line nobody is credited with, which is the direction a
// mistake should fall.
//
// The question came from minutes-mac, asking what an honest track name would be
// for a device that records a room. The answer is that there is not one yet,
// and until there is, an unknown name must assert nothing.
func TestUnknownTrackNameIsNotAttributed(t *testing.T) {
	cases := map[string]string{
		"mic":    SpeakerYou,
		"system": SpeakerOthers,
		"room":   SpeakerUnattributed,
		"phone":  SpeakerUnattributed,
		"":       SpeakerUnattributed,
		"System": SpeakerUnattributed, // case matters; a near-miss is still unknown
	}
	for track, want := range cases {
		if got := speakerFor(track); got != want {
			t.Errorf("speakerFor(%q) = %q, want %q", track, got, want)
		}
	}
}

// A recording that cannot say who spoke must not credit anybody in its
// withheld lines either.
//
// The unattributed pass rewrote the readable lines and left Withheld alone, so
// a one-source recording shipped `{"speaker":"You","text":"Thanks for
// watching!"}` in transcript.json — under a header stating in capitals that
// the recording cannot say who spoke. Withheld lines are where a reader goes
// when a line is missing, so that is the moment somebody is looking closely;
// and the JSON is read by machines that cannot see the warning at all.
//
// Found in the first real recording made on macOS, not by a test.
func TestWithheldLinesAreUnattributedToo(t *testing.T) {
	out := &Transcript{
		Lines: []Line{
			{Speaker: SpeakerYou, Text: "something said"},
		},
		Withheld: []Line{
			{Speaker: SpeakerYou, Text: "Thanks for watching!", Suppressed: "no speech"},
		},
	}
	unattribute(out, func(string, ...any) {})

	if !out.Unattributed {
		t.Fatal("markUnattributed did not set the flag")
	}
	for i, l := range out.Lines {
		if l.Speaker != SpeakerUnattributed {
			t.Errorf("kept line %d has speaker %q, want %q", i, l.Speaker, SpeakerUnattributed)
		}
	}
	for i, l := range out.Withheld {
		if l.Speaker != SpeakerUnattributed {
			t.Errorf("withheld line %d has speaker %q on an unattributed recording — "+
				"it credits somebody the recording cannot identify", i, l.Speaker)
		}
	}
}

// The far end being missing was checked from the start; the microphone was not,
// because it is the track you assume is there. A real 44-minute standup was
// transcribed with zero microphone frames, attributed entirely to "Others", and
// delivered — with the helper's own "track mic ended after 0 audio frames"
// sitting unread in the log.
//
// Asserted as a pair. A test that only knew what the missing-microphone case
// says would pass just as happily if every transcript said it, which is the
// blindness that let the asymmetry survive in the first place.
func TestAMissingMicrophoneIsDisclosed(t *testing.T) {
	build := func(micFrames uint64) *Transcript {
		m := &manifest.Manifest{Tracks: []manifest.Track{
			{Name: "system", SampleRate: 44100, Segments: []manifest.Segment{{Frames: 44100}}},
			{Name: "mic", SampleRate: 48000, Segments: []manifest.Segment{{Frames: micFrames}}},
		}}
		out := &Transcript{RecordingID: "r", Backend: "b", Recorded: true}
		if !trackHasAudio(m, "mic") {
			out.MicrophoneLost = true
		}
		return out
	}

	present, lost := build(48000), build(0)
	if present.MicrophoneLost {
		t.Error("a microphone that captured audio was reported lost")
	}
	if !lost.MicrophoneLost {
		t.Fatal("a microphone that captured nothing was not reported lost")
	}
	if present.Text() == lost.Text() {
		t.Fatal("a recording missing the microphone reads identically to one that is complete")
	}
	if !strings.Contains(lost.Text(), "YOUR OWN AUDIO IS MISSING") {
		t.Errorf("no banner on a transcript missing the microphone:\n%s", lost.Text())
	}
	if strings.Contains(present.Text(), "MISSING") {
		t.Errorf("a complete recording claims something is missing:\n%s", present.Text())
	}
}

// A segment below the silence floor is never sent to the model, so it produces
// no line and no withheld line either — it is simply absent, and nothing
// downstream can tell a missing stretch of meeting from a quiet one.
//
// It was reported only in the log, which for a supervised recording is a file:
// the same place the helper's report of a dead microphone sat unread for two
// days.
//
// Asserted as a pair. A transcript with skipped segments and one without must
// read differently, or the notice is either always there or never there and
// both look the same from one example.
func TestSkippedSilentSegmentsAreDisclosed(t *testing.T) {
	quiet := &Transcript{RecordingID: "r", Backend: "b", Recorded: true, SilentSegments: 3}
	full := &Transcript{RecordingID: "r", Backend: "b", Recorded: true}

	if quiet.Text() == full.Text() {
		t.Fatal("a transcript missing whole segments reads identically to a complete one")
	}
	if !strings.Contains(quiet.Text(), "never transcribed") {
		t.Errorf("skipped segments are not disclosed:\n%s", quiet.Text())
	}
	if strings.Contains(full.Text(), "never transcribed") {
		t.Errorf("a complete transcript claims segments were skipped:\n%s", full.Text())
	}
}
