package manifest

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestSaveAndLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	m := New(dir, "2026-08-25-101530-standup", "standup", 300)
	if err := m.SetTrack("mic", "Some Microphone", 48000, 2); err != nil {
		t.Fatal(err)
	}
	if err := m.PutSegment("mic", Segment{
		Index: 0, File: "mic-000.wav", StartSeconds: 0,
		DurationSeconds: 300, Frames: 14400000, PeakDBFS: -7.3, Complete: true,
	}); err != nil {
		t.Fatal(err)
	}

	got, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != m.ID || got.Name != "standup" || got.State != StateRecording {
		t.Fatalf("round trip changed the recording: %+v", got)
	}
	if !got.Recorded {
		t.Error("Recorded is false; the fact that a meeting was recorded must survive to whatever reads this")
	}
	if len(got.Tracks) != 1 || got.Tracks[0].Device != "Some Microphone" {
		t.Fatalf("tracks did not survive: %+v", got.Tracks)
	}
	if len(got.Tracks[0].Segments) != 1 || got.Tracks[0].Segments[0].Frames != 14400000 {
		t.Fatalf("segments did not survive: %+v", got.Tracks[0].Segments)
	}
	if got.Dir() != dir {
		t.Errorf("Dir() = %q, want %q", got.Dir(), dir)
	}
}

// Re-putting an index must replace it, not append. A segment is written once
// when it opens and again when it closes, and a manifest that listed both would
// double-count every chunk.
func TestPutSegmentReplacesByIndex(t *testing.T) {
	dir := t.TempDir()
	m := New(dir, "id", "", 300)
	if err := m.PutSegment("mic", Segment{Index: 0, File: "mic-000.wav", Complete: false}); err != nil {
		t.Fatal(err)
	}
	if err := m.PutSegment("mic", Segment{Index: 0, File: "mic-000.wav", Complete: true, Frames: 99}); err != nil {
		t.Fatal(err)
	}
	got, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(got.Tracks[0].Segments); n != 1 {
		t.Fatalf("manifest holds %d entries for one segment, want 1", n)
	}
	if !got.Tracks[0].Segments[0].Complete || got.Tracks[0].Segments[0].Frames != 99 {
		t.Errorf("the second put did not replace the first: %+v", got.Tracks[0].Segments[0])
	}
}

// The manifest is the only thing that says what the WAV files beside it are, so
// a crash during the write must leave the previous one intact rather than a
// half-written file.
func TestSaveIsAtomic(t *testing.T) {
	dir := t.TempDir()
	m := New(dir, "id", "", 300)
	if err := m.Save(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".manifest.tmp")); err == nil {
		t.Error("the temporary file was left behind, so it was not renamed into place")
	}
	body, err := os.ReadFile(filepath.Join(dir, Name))
	if err != nil {
		t.Fatal(err)
	}
	var any map[string]any
	if err := json.Unmarshal(body, &any); err != nil {
		t.Fatalf("the manifest on disk is not valid JSON: %v", err)
	}
}

func TestFinishRecordsFailure(t *testing.T) {
	dir := t.TempDir()
	m := New(dir, "id", "", 300)
	if err := m.Finish(os.ErrDeadlineExceeded); err != nil {
		t.Fatal(err)
	}
	got, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != StateFailed {
		t.Errorf("state = %q, want %q", got.State, StateFailed)
	}
	if got.Error == "" {
		t.Error("a failed recording does not say why")
	}
	if got.StoppedAt == nil {
		t.Error("a finished recording has no stop time")
	}
}

func TestTrackDurationSpansSegments(t *testing.T) {
	tr := Track{SampleRate: 48000, Segments: []Segment{
		{Index: 0, StartSeconds: 0, DurationSeconds: 300},
		{Index: 2, StartSeconds: 600, DurationSeconds: 120}, // segment 1 skipped
	}}
	if got, want := tr.Duration(), 720.0; got != want {
		t.Errorf("Duration = %v, want %v", got, want)
	}
}

func TestSilentTrackIsReported(t *testing.T) {
	quiet := Track{Segments: []Segment{{PeakDBFS: -999}, {PeakDBFS: -999}}}
	if !quiet.Silent() {
		t.Error("a track with no signal is not reported silent")
	}
	loud := Track{Segments: []Segment{{PeakDBFS: -999}, {PeakDBFS: -20}}}
	if loud.Silent() {
		t.Error("a track with signal in one segment is reported silent")
	}
	if got := loud.PeakDBFS(); got != -20 {
		t.Errorf("PeakDBFS = %v, want -20", got)
	}
}

// A manifest carrying an infinity cannot be written at all, and the segment
// that would carry one is a silent segment — the case that most needs writing
// down.
func TestInfiniteLevelWouldNotSerialise(t *testing.T) {
	if _, err := json.Marshal(Segment{PeakDBFS: math.Inf(-1)}); err == nil {
		t.Skip("encoding/json now accepts infinities; the -999 floor is no longer load-bearing")
	}
	dir := t.TempDir()
	m := New(dir, "id", "", 300)
	if err := m.PutSegment("system", Segment{Index: 0, PeakDBFS: -999}); err != nil {
		t.Fatalf("a floored silent level could not be written: %v", err)
	}
}

func TestHashFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "x")
	if err := os.WriteFile(p, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	sum, size, err := HashFile(p)
	if err != nil {
		t.Fatal(err)
	}
	const want = "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824"
	if sum != want {
		t.Errorf("sha256 = %s, want %s", sum, want)
	}
	if size != 5 {
		t.Errorf("size = %d, want 5", size)
	}
}

func TestLoadRejectsCorruptManifest(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, Name), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(dir); err == nil {
		t.Error("a corrupt manifest loaded without error")
	}
}

// "Did I send this one?" has to be answerable from the recording rather than
// from memory, because it is asked days later.
func TestDeliveryIsRecorded(t *testing.T) {
	dir := t.TempDir()
	m := New(dir, "id", "", 300)
	if err := m.SetDelivery(DeliveryRecord{To: "homelab", At: time.Now()}); err != nil {
		t.Fatal(err)
	}
	got, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got.Delivery == nil || got.Delivery.To != "homelab" {
		t.Fatalf("delivery did not survive: %+v", got.Delivery)
	}
	if got.Delivery.Degraded {
		t.Error("a real delivery was recorded as degraded")
	}
}

// Written to disk because the agent was unreachable is not the same as
// delivered, and must not look like it in a listing.
func TestDegradedDeliveryIsDistinguishable(t *testing.T) {
	dir := t.TempDir()
	m := New(dir, "id", "", 300)
	if err := m.SetDelivery(DeliveryRecord{To: "homelab", At: time.Now(), Degraded: true}); err != nil {
		t.Fatal(err)
	}
	got, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Delivery.Degraded {
		t.Error("a brief written to disk was recorded as a real delivery")
	}
}

// Speech peaks around -6 to -20 dBFS. A track far below that held nothing
// anybody said, even though it is not digital silence — which is exactly the
// case that used to be transcribed into invention.
func TestCarriesSpeechSeparatesAMutedMicFromDigitalSilence(t *testing.T) {
	cases := []struct {
		name string
		peak float64
		want bool
	}{
		{"normal speech", -8, true},
		{"quiet speech", -30, true},
		{"a muted microphone, measured on a real recording", -55.7, false},
		{"digital silence", -999, false},
	}
	for _, c := range cases {
		tr := Track{Segments: []Segment{{PeakDBFS: c.peak}}}
		if got := tr.CarriesSpeech(); got != c.want {
			t.Errorf("%s (%.1f dBFS): CarriesSpeech = %v, want %v", c.name, c.peak, got, c.want)
		}
	}
}

// A manifest must not claim a machine it was not recorded on.
//
// This was hardcoded to "wsl/windows" for as long as that was the only thing
// that could record, and stayed hardcoded once macOS could — so every Mac
// recording said it came from Windows. Nothing downstream had any reason to
// doubt it, which is what makes a confidently wrong provenance field worse
// than a missing one.
//
// The check is honest about its own reach: on a WSL kernel the old hardcoded
// value was correct, so this can only fail on a host where the two differ.
// That is exactly the host the bug was on, and it is the reason it survived.
func TestPlatformDescribesThisMachineRatherThanAHardcodedOne(t *testing.T) {
	// Through New(), not platform() directly: the first version of this test
	// called the helper and passed happily while the field New() actually
	// writes was still hardcoded. A test that exercises a function nobody's
	// output depends on proves nothing.
	m := New(t.TempDir(), "id", "name", 300)
	got := m.Platform
	if got == "" {
		t.Fatal("platform() is empty; a manifest with no provenance cannot be checked later")
	}

	want := map[string]string{"darwin": "macos", "windows": "windows"}[runtime.GOOS]
	if want != "" && got != want {
		t.Errorf("platform() = %q on %s, want %q", got, runtime.GOOS, want)
	}
	if runtime.GOOS != "linux" && got == "wsl/windows" {
		t.Errorf("platform() = %q on %s — the manifest names a machine this is not",
			got, runtime.GOOS)
	}
}
