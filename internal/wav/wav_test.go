package wav

import (
	"encoding/binary"
	"math"
	"os"
	"path/filepath"
	"testing"
)

// readWAV returns the parsed header fields and the sample data of a file.
func readWAV(t *testing.T, path string) (sampleRate int, channels int, data []int16) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	if len(b) < headerSize {
		t.Fatalf("file is %d bytes, shorter than a header", len(b))
	}
	if string(b[0:4]) != "RIFF" || string(b[8:12]) != "WAVE" {
		t.Fatalf("not a RIFF/WAVE file")
	}
	channels = int(binary.LittleEndian.Uint16(b[22:]))
	sampleRate = int(binary.LittleEndian.Uint32(b[24:]))
	declared := binary.LittleEndian.Uint32(b[40:])
	if int(declared) != len(b)-headerSize {
		t.Fatalf("data chunk declares %d bytes, file carries %d", declared, len(b)-headerSize)
	}
	payload := b[headerSize:]
	data = make([]int16, len(payload)/2)
	for i := range data {
		data[i] = int16(binary.LittleEndian.Uint16(payload[i*2:]))
	}
	return sampleRate, channels, data
}

// A packet that arrives after a gap must land at its timestamp, not at the end
// of what was written. This is the whole reason timestamps are carried through
// the pipeline: a loopback stream delivers nothing while the machine is silent,
// so appending would slide every later packet earlier by the length of the
// quiet.
func TestWriteAtFillsGapWithSilence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gap.wav")
	w, err := NewWriter(path, 48000, 2)
	if err != nil {
		t.Fatal(err)
	}

	// One stereo frame of signal at offset 0, another at offset 1000.
	if err := w.WriteAt(0, []int16{100, 100}); err != nil {
		t.Fatal(err)
	}
	if err := w.WriteAt(1000, []int16{-200, -200}); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	rate, ch, data := readWAV(t, path)
	if rate != 48000 || ch != 2 {
		t.Fatalf("header says %d Hz %d ch, want 48000 Hz 2 ch", rate, ch)
	}

	// 1000 frames of gap plus the frame written into it: 1001 frames, 2002 samples.
	if got, want := len(data), 1001*2; got != want {
		t.Fatalf("file holds %d samples, want %d — the gap was not filled", got, want)
	}
	if data[0] != 100 || data[1] != 100 {
		t.Errorf("first frame is %v, want [100 100]", data[0:2])
	}
	for i := 2; i < 2000; i++ {
		if data[i] != 0 {
			t.Fatalf("sample %d in the gap is %d, want silence", i, data[i])
		}
	}
	if data[2000] != -200 || data[2001] != -200 {
		t.Errorf("frame after the gap is %v, want [-200 -200] — it did not land at its timestamp",
			data[2000:2002])
	}
}

// PaddedFrames must account for gap-fill, because a system track that is mostly
// padding means nothing was playing, and that is worth reporting rather than
// discovering in an empty transcript.
func TestPaddedFramesCountsOnlyGapFill(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pad.wav")
	w, err := NewWriter(path, 48000, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.WriteAt(0, []int16{1}); err != nil {
		t.Fatal(err)
	}
	if err := w.WriteAt(500, []int16{1}); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if got, want := w.PaddedFrames, uint64(499); got != want {
		t.Errorf("PaddedFrames = %d, want %d", got, want)
	}
}

// A packet whose timestamp is behind what is already written must not rewind
// over committed audio.
func TestWriteAtDoesNotRewind(t *testing.T) {
	path := filepath.Join(t.TempDir(), "overlap.wav")
	w, err := NewWriter(path, 48000, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.WriteAt(0, []int16{1, 2, 3, 4}); err != nil {
		t.Fatal(err)
	}
	if err := w.WriteAt(1, []int16{9}); err != nil { // behind the write head
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	_, _, data := readWAV(t, path)
	want := []int16{1, 2, 3, 4, 9}
	if len(data) != len(want) {
		t.Fatalf("got %d samples %v, want %d %v", len(data), data, len(want), want)
	}
	for i := range want {
		if data[i] != want[i] {
			t.Fatalf("got %v, want %v — a late packet overwrote committed audio", data, want)
		}
	}
}

func TestDurationTracksSampleRate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dur.wav")
	w, err := NewWriter(path, 44100, 2)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.WriteAt(44100, nil); err != nil { // one second of gap
		t.Fatal(err)
	}
	if got := w.Duration(); math.Abs(got-1.0) > 1e-9 {
		t.Errorf("Duration = %v, want 1.0", got)
	}
	w.Close()
}

func TestToInt16Float32(t *testing.T) {
	payload := make([]byte, 4*4)
	for i, v := range []float32{0, 1, -1, 0.5} {
		binary.LittleEndian.PutUint32(payload[i*4:], math.Float32bits(v))
	}
	got, err := ToInt16(payload, 3, 32)
	if err != nil {
		t.Fatal(err)
	}
	want := []int16{0, 32767, -32767, 16384}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("sample %d = %d, want %d", i, got[i], want[i])
		}
	}
}

// A shared-mode float mix is not guaranteed to stay inside unity. Wrapping an
// out-of-range sample turns a loud moment into a bang, so it must clamp.
func TestToInt16ClampsOutOfRangeFloats(t *testing.T) {
	payload := make([]byte, 2*4)
	binary.LittleEndian.PutUint32(payload[0:], math.Float32bits(2.5))
	binary.LittleEndian.PutUint32(payload[4:], math.Float32bits(-3.0))
	got, err := ToInt16(payload, 3, 32)
	if err != nil {
		t.Fatal(err)
	}
	if got[0] != 32767 {
		t.Errorf("2.5 became %d, want 32767 (clamped)", got[0])
	}
	if got[1] != -32767 {
		t.Errorf("-3.0 became %d, want -32767 (clamped)", got[1])
	}
}

func TestToInt16RejectsUnknownFormat(t *testing.T) {
	if _, err := ToInt16(make([]byte, 8), 7, 24); err == nil {
		t.Error("expected an error for an unsupported format, got nil")
	}
}

// Leading silence is trimmed so a speech model does not anchor its first
// utterance at zero, and the amount removed is reported so it can be added back
// to every timestamp.
func TestTrimLeadingSilence(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.wav")
	w, err := NewWriter(src, 1000, 2)
	if err != nil {
		t.Fatal(err)
	}
	// 2 seconds of silence, then 1 second of tone.
	tone := make([]int16, 1000*2)
	for i := range tone {
		tone[i] = int16(500 + i%100)
	}
	if err := w.WriteAt(2000, tone); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	dst := filepath.Join(dir, "dst.wav")
	skipped, err := TrimLeadingSilence(src, dst)
	if err != nil {
		t.Fatal(err)
	}
	if skipped < 1.99 || skipped > 2.01 {
		t.Errorf("skipped %v seconds, want 2", skipped)
	}
	rate, ch, data := readWAV(t, dst)
	if rate != 1000 || ch != 2 {
		t.Errorf("trimmed file is %d Hz %d ch, want 1000 Hz 2 ch", rate, ch)
	}
	if got, want := len(data), 1000*2; got != want {
		t.Fatalf("trimmed file holds %d samples, want %d", got, want)
	}
	if data[0] != 500 {
		t.Errorf("first sample of the trimmed file is %d, want 500 — the trim landed in the wrong place", data[0])
	}
}

// A file that does not begin with silence must be passed through unchanged: a
// rewrite is only a chance to lose something.
func TestTrimLeavesFileWithoutLeadingSilenceAlone(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.wav")
	w, err := NewWriter(src, 1000, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.WriteAt(0, []int16{7, 8, 9}); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	dst := filepath.Join(dir, "dst.wav")
	skipped, err := TrimLeadingSilence(src, dst)
	if err != nil {
		t.Fatal(err)
	}
	if skipped != 0 {
		t.Errorf("skipped %v seconds of a file that opens with audio", skipped)
	}
	_, _, data := readWAV(t, dst)
	if len(data) != 3 || data[0] != 7 {
		t.Errorf("passthrough changed the data: %v", data)
	}
}

// Only exact digital silence is trimmed, so quiet speech and room tone survive.
// Gap-fill is exact zeros; a microphone's noise floor is not.
func TestTrimDoesNotRemoveQuietAudio(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.wav")
	w, err := NewWriter(src, 1000, 1)
	if err != nil {
		t.Fatal(err)
	}
	// A very quiet first sample, well below anything audible, but not zero.
	if err := w.WriteAt(0, []int16{1, 0, 0, 500}); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	dst := filepath.Join(dir, "dst.wav")
	skipped, err := TrimLeadingSilence(src, dst)
	if err != nil {
		t.Fatal(err)
	}
	if skipped != 0 {
		t.Errorf("trimmed %v seconds starting at a non-zero sample", skipped)
	}
}
