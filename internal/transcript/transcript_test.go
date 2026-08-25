package transcript

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alexj/minutes/internal/manifest"
	"github.com/alexj/minutes/internal/transcribe"
	"github.com/alexj/minutes/internal/wav"
)

// fakeTranscriber returns canned utterances per file, and records what it was
// asked to transcribe — which is how the silence-skipping is checked.
type fakeTranscriber struct {
	byFile map[string][]transcribe.Utterance
	asked  []string
	remote bool
}

func (f *fakeTranscriber) Name() string                { return "fake" }
func (f *fakeTranscriber) SendsAudioOffMachine() bool  { return f.remote }
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
