package segment

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"

	"github.com/alexj212/minutes/internal/manifest"
)

// collector captures the manifest entries a writer emits.
type collector struct{ segs []manifest.Segment }

func (c *collector) on(s manifest.Segment) error {
	for i := range c.segs {
		if c.segs[i].Index == s.Index {
			c.segs[i] = s
			return nil
		}
	}
	c.segs = append(c.segs, s)
	return nil
}

func newTestWriter(t *testing.T, dir, track string, rate, ch int, segSec float64) (*Writer, *collector) {
	t.Helper()
	w, err := NewWriter(dir, track, rate, ch, segSec)
	if err != nil {
		t.Fatal(err)
	}
	c := &collector{}
	w.OnSegment = c.on
	return w, c
}

func wavFrames(t *testing.T, path string, channels int) int {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	declared := binary.LittleEndian.Uint32(b[40:])
	if int(declared) != len(b)-44 {
		t.Fatalf("%s: header declares %d data bytes, file carries %d", path, declared, len(b)-44)
	}
	return int(declared) / (channels * 2)
}

// Segment k of every track must cover the same wall-clock window, even though
// the tracks run at different sample rates. This is what lets a later phase
// transcribe the two separately and merge them without re-deriving alignment,
// so it is the property worth pinning down.
func TestSegmentBoundariesAgreeAcrossSampleRates(t *testing.T) {
	dir := t.TempDir()
	const segSec = 5.0

	mic, micSegs := newTestWriter(t, dir, "mic", 48000, 2, segSec)
	sys, sysSegs := newTestWriter(t, dir, "system", 44100, 2, segSec)

	// One frame of audio at each of these instants, in seconds.
	for _, at := range []float64{0, 4.9, 5.1, 9.9, 10.1} {
		if err := mic.WriteAt(uint64(at*48000), []int16{1000, 1000}, 0); err != nil {
			t.Fatal(err)
		}
		if err := sys.WriteAt(uint64(at*44100), []int16{1000, 1000}, 0); err != nil {
			t.Fatal(err)
		}
	}
	if err := mic.Close(); err != nil {
		t.Fatal(err)
	}
	if err := sys.Close(); err != nil {
		t.Fatal(err)
	}

	if len(micSegs.segs) != len(sysSegs.segs) {
		t.Fatalf("mic produced %d segments, system %d — they disagree on boundaries",
			len(micSegs.segs), len(sysSegs.segs))
	}
	if len(micSegs.segs) != 3 {
		t.Fatalf("got %d segments, want 3 (audio at 0-5s, 5-10s, 10-15s)", len(micSegs.segs))
	}
	for i := range micSegs.segs {
		m, s := micSegs.segs[i], sysSegs.segs[i]
		if m.Index != s.Index {
			t.Errorf("segment %d: mic index %d, system index %d", i, m.Index, s.Index)
		}
		if m.StartSeconds != s.StartSeconds {
			t.Errorf("segment %d starts at %vs on mic but %vs on system",
				m.Index, m.StartSeconds, s.StartSeconds)
		}
		if want := float64(m.Index) * segSec; m.StartSeconds != want {
			t.Errorf("segment %d starts at %vs, want %vs", m.Index, m.StartSeconds, want)
		}
	}
}

// A packet that straddles a boundary must be split, not pushed into one side.
// Letting it spill would move the boundary, and the two tracks would stop
// agreeing on where segment k ends.
func TestPacketStraddlingABoundaryIsSplit(t *testing.T) {
	dir := t.TempDir()
	const rate, ch = 1000, 1
	w, c := newTestWriter(t, dir, "mic", rate, ch, 1.0) // 1000 frames per segment

	// 100 frames starting 50 before the boundary: 50 in segment 0, 50 in 1.
	samples := make([]int16, 100)
	for i := range samples {
		samples[i] = int16(i + 1)
	}
	if err := w.WriteAt(950, samples, 0); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	if len(c.segs) != 2 {
		t.Fatalf("got %d segments, want 2", len(c.segs))
	}
	if got := wavFrames(t, filepath.Join(dir, FileName("mic", 0)), ch); got != 1000 {
		t.Errorf("segment 0 holds %d frames, want 1000 (padded to the boundary)", got)
	}
	if got := wavFrames(t, filepath.Join(dir, FileName("mic", 1)), ch); got != 50 {
		t.Errorf("segment 1 holds %d frames, want 50 (the spill)", got)
	}

	// The split must not duplicate or drop samples.
	b0, _ := os.ReadFile(filepath.Join(dir, FileName("mic", 0)))
	last := int16(binary.LittleEndian.Uint16(b0[44+999*2:]))
	if last != 50 {
		t.Errorf("last sample of segment 0 is %d, want 50", last)
	}
	b1, _ := os.ReadFile(filepath.Join(dir, FileName("mic", 1)))
	first := int16(binary.LittleEndian.Uint16(b1[44:]))
	if first != 51 {
		t.Errorf("first sample of segment 1 is %d, want 51 — the split lost or repeated a sample", first)
	}
}

// A gap long enough to skip whole segments must not write them. Five minutes of
// zeros per skipped chunk is tens of megabytes to say nothing; the missing
// index in the manifest says it for free.
func TestSkippedSegmentsAreNotCreated(t *testing.T) {
	dir := t.TempDir()
	w, c := newTestWriter(t, dir, "system", 1000, 1, 1.0)

	if err := w.WriteAt(0, []int16{1}, 0); err != nil {
		t.Fatal(err)
	}
	// Jump to segment 5, skipping 1 through 4.
	if err := w.WriteAt(5000, []int16{1}, 0); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	if len(c.segs) != 2 {
		t.Fatalf("got %d segments, want 2", len(c.segs))
	}
	if c.segs[0].Index != 0 || c.segs[1].Index != 5 {
		t.Fatalf("segment indexes are %d and %d, want 0 and 5", c.segs[0].Index, c.segs[1].Index)
	}
	for _, missing := range []int{1, 2, 3, 4} {
		p := filepath.Join(dir, FileName("system", missing))
		if _, err := os.Stat(p); err == nil {
			t.Errorf("segment %d was written; skipped segments must not exist", missing)
		}
	}
}

// A segment is named in the manifest before any audio is in it, so a recording
// killed a second later has the file listed rather than orphaned beside the
// manifest.
func TestSegmentIsAnnouncedBeforeItIsComplete(t *testing.T) {
	dir := t.TempDir()
	w, c := newTestWriter(t, dir, "mic", 1000, 1, 1.0)
	if err := w.WriteAt(0, []int16{1}, 0); err != nil {
		t.Fatal(err)
	}
	if len(c.segs) != 1 {
		t.Fatalf("got %d segments before close, want 1", len(c.segs))
	}
	if c.segs[0].Complete {
		t.Error("an open segment is marked complete")
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if !c.segs[0].Complete {
		t.Error("a closed segment is not marked complete")
	}
	if c.segs[0].SHA256 == "" || c.segs[0].Size == 0 {
		t.Error("a closed segment has no checksum or size")
	}
}

// A silent segment must report a finite level, because the manifest is JSON and
// encoding/json refuses infinities — and a silent track is the case that most
// needs to be written down.
func TestSilentSegmentReportsAFiniteLevel(t *testing.T) {
	dir := t.TempDir()
	w, c := newTestWriter(t, dir, "system", 1000, 1, 1.0)
	if err := w.WriteAt(0, []int16{0, 0, 0}, 0); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if len(c.segs) != 1 {
		t.Fatalf("got %d segments, want 1", len(c.segs))
	}
	m := manifest.New(dir, "id", "", 1.0)
	if err := m.PutSegment("system", c.segs[0]); err != nil {
		t.Fatalf("a silent segment could not be written to the manifest: %v", err)
	}
}

func TestRejectsNonsenseParameters(t *testing.T) {
	dir := t.TempDir()
	if _, err := NewWriter(dir, "mic", 48000, 2, 0); err == nil {
		t.Error("accepted a zero-length segment")
	}
	if _, err := NewWriter(dir, "mic", 0, 2, 5); err == nil {
		t.Error("accepted a zero sample rate")
	}
	w, _ := newTestWriter(t, dir, "mic", 1000, 2, 1.0)
	if err := w.WriteAt(0, []int16{1, 2, 3}, 0); err == nil {
		t.Error("accepted 3 samples on a 2-channel track, which is not whole frames")
	}
}

// An interrupted segment must still be readable. The header is patched only on
// sync, so a segment shorter than the sync interval would never be patched at
// all — measured before this was fixed, a 4-second segment killed in progress
// declared 0 data bytes while holding 4972 of them.
func TestOpenSegmentIsReadableBeforeItIsClosed(t *testing.T) {
	dir := t.TempDir()
	const rate, ch = 1000, 1
	w, c := newTestWriter(t, dir, "system", rate, ch, 4.0) // 4s segment

	// Two seconds of audio, then walk away without closing, as a kill would.
	for i := 0; i < 2000; i++ {
		if err := w.WriteAt(uint64(i), []int16{int16(i%100 + 1)}, 0); err != nil {
			t.Fatal(err)
		}
	}

	b, err := os.ReadFile(filepath.Join(dir, FileName("system", 0)))
	if err != nil {
		t.Fatal(err)
	}
	declared := binary.LittleEndian.Uint32(b[40:])
	if declared == 0 {
		t.Fatal("the open segment's header declares 0 data bytes: its audio is on disk but not addressable")
	}
	// Everything up to the last sync must be reachable through the header.
	if int(declared) > len(b)-44 {
		t.Fatalf("header declares %d data bytes but the file holds %d", declared, len(b)-44)
	}
	if got := int(declared) / (ch * 2); got < 1000 {
		t.Errorf("only %d frames are addressable after writing 2000; the sync interval is too coarse", got)
	}

	// The manifest entry must have been refreshed alongside the header, or an
	// interrupted recording understates audio that is sitting on the disk.
	if len(c.segs) != 1 {
		t.Fatalf("got %d segment entries, want 1", len(c.segs))
	}
	if c.segs[0].Frames == 0 {
		t.Error("the manifest entry still says 0 frames while the file holds audio")
	}
}

func TestSyncIntervalShrinksForShortSegments(t *testing.T) {
	if got := syncInterval(300); got != syncEvery {
		t.Errorf("syncInterval(300) = %v, want %v", got, syncEvery)
	}
	if got, want := syncInterval(4), 1.0; got != want {
		t.Errorf("syncInterval(4) = %v, want %v", got, want)
	}
}

// A denied microphone on macOS opens, starts, reports its format and hands over
// zeros, so nothing in the capture path can tell. The signature that survives
// into the manifest is that the signal never varies.
//
// Asserted as a pair against real audio, because "constant" that is true of
// everything is exactly as useless as one true of nothing — and the failing
// direction here deletes a recording's credibility rather than a line.
func TestConstantMarksANullSignalAndNotRealAudio(t *testing.T) {
	write := func(name string, samples []int16) manifest.Segment {
		t.Helper()
		dir := t.TempDir()
		var got manifest.Segment
		w, err := NewWriter(dir, name, 48000, 1, 60)
		if err != nil {
			t.Fatal(err)
		}
		w.OnSegment = func(s manifest.Segment) error { got = s; return nil }
		if err := w.WriteAt(0, samples, 0); err != nil {
			t.Fatal(err)
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
		return got
	}

	zeros := make([]int16, 4800)
	dc := make([]int16, 4800)
	for i := range dc {
		dc[i] = 1200 // a pinned device: not silent, and not varying
	}
	room := make([]int16, 4800)
	for i := range room {
		room[i] = int16(i%7) - 3 // a noise floor, barely above nothing
	}

	if s := write("mic", zeros); !s.Constant {
		t.Error("a null signal was not marked constant")
	}
	if s := write("mic", dc); !s.Constant {
		t.Error("a signal pinned at a non-zero value was not marked constant — " +
			"a zero-check would have missed this one")
	}
	if s := write("mic", room); s.Constant {
		t.Errorf("a quiet room was marked constant (peak %.1f dBFS)", s.PeakDBFS)
	}
}
