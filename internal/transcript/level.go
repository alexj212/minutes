package transcript

import (
	"encoding/binary"
	"fmt"
	"math"
	"os"
	"path/filepath"

	"github.com/alexj/minutes/internal/manifest"
)

// Measuring how loud a line was.
//
// Text comparison cannot catch every echo. A one-word fragment of the far end
// arriving through the air — "all", from "...the old endpoint alive" — does not
// match the far-end transcript when that transcript was cut at "the old end",
// so word overlap scores zero and the fragment is attributed to you.
//
// Level can catch it, because bleed is quieter than speaking into the
// microphone. That is physics rather than a heuristic: the far end reaches the
// microphone across a room, and you do not.

const wavHeaderSize = 44

// levelReader measures the loudness of a time range on one track by reading the
// segment that covers it.
type levelReader struct {
	dir  string
	seg  float64
	spec manifest.Track
}

func newLevelReader(dir string, seg float64, t manifest.Track) *levelReader {
	return &levelReader{dir: dir, seg: seg, spec: t}
}

// read returns the samples between start and end, in seconds from the recording
// epoch, or ok=false when the range is not on disk.
func (lr *levelReader) read(start, end float64) ([]int16, bool) {
	if lr.spec.SampleRate == 0 || lr.spec.Channels == 0 || lr.seg <= 0 {
		return nil, false
	}
	index := int(start / lr.seg)
	within := start - float64(index)*lr.seg
	if within < 0 {
		return nil, false
	}
	// A range crossing a segment boundary is measured only up to it. Fragments
	// are a second or so; losing the tail of one changes nothing.
	if end-start <= 0 {
		return nil, false
	}
	if within+(end-start) > lr.seg {
		end = start + (lr.seg - within)
	}

	path := filepath.Join(lr.dir, fmt.Sprintf("%s-%03d.wav", lr.spec.Name, index))
	f, err := os.Open(path)
	if err != nil {
		return nil, false
	}
	defer f.Close()

	blockAlign := int64(lr.spec.Channels * 2)
	rate := float64(lr.spec.SampleRate)
	off := wavHeaderSize + int64(within*rate)*blockAlign
	n := int64((end-start)*rate) * blockAlign
	if n <= 0 {
		return nil, false
	}

	buf := make([]byte, n)
	read, err := f.ReadAt(buf, off)
	if read <= 0 {
		return nil, false
	}
	_ = err // a short read at the end of a segment is fine; measure what is there
	buf = buf[:read-read%2]

	out := make([]int16, 0, len(buf)/2)
	for i := 0; i+1 < len(buf); i += 2 {
		out = append(out, int16(binary.LittleEndian.Uint16(buf[i:])))
	}
	return out, true
}

// peakDBFS returns the loudest sample in a range, in dBFS.
func (lr *levelReader) peakDBFS(start, end float64) (float64, bool) {
	samples, ok := lr.read(start, end)
	if !ok {
		return 0, false
	}
	var peak int16
	for _, v := range samples {
		if v == math.MinInt16 {
			v = math.MaxInt16
		} else if v < 0 {
			v = -v
		}
		if v > peak {
			peak = v
		}
	}
	if peak <= 0 {
		return silentDBFS, true
	}
	return 20 * math.Log10(float64(peak)/32767), true
}

const silentDBFS = -999.0

// referenceLevel is how loud this person is when actually speaking.
//
// Taken from the median of a sample of long microphone lines, because a median
// is unmoved by the handful of quiet fragments this is trying to find. Long
// lines only: a fragment is exactly what must not set the reference.
func referenceLevel(lines []Line, lr *levelReader) (float64, bool) {
	const (
		wantSamples = 40
		longEnough  = 25 // characters
	)
	var candidates []Line
	for _, l := range lines {
		if l.Track == "mic" && len(l.Text) >= longEnough && l.End > l.Start {
			candidates = append(candidates, l)
		}
	}
	if len(candidates) < 5 {
		return 0, false
	}
	step := len(candidates) / wantSamples
	if step < 1 {
		step = 1
	}
	var levels []float64
	for i := 0; i < len(candidates); i += step {
		if db, ok := lr.peakDBFS(candidates[i].Start, candidates[i].End); ok && db > silentDBFS {
			levels = append(levels, db)
		}
	}
	if len(levels) < 5 {
		return 0, false
	}
	// Median without sorting the whole thing twice.
	for i := 1; i < len(levels); i++ {
		for j := i; j > 0 && levels[j] < levels[j-1]; j-- {
			levels[j], levels[j-1] = levels[j-1], levels[j]
		}
	}
	return levels[len(levels)/2], true
}

// rms returns the root-mean-square level of a range, in dBFS.
func (lr *levelReader) rms(start, end float64) (float64, bool) {
	samples, ok := lr.read(start, end)
	if !ok || len(samples) == 0 {
		return 0, false
	}
	var sum float64
	for _, v := range samples {
		f := float64(v) / 32767
		sum += f * f
	}
	mean := math.Sqrt(sum / float64(len(samples)))
	if mean <= 0 {
		return silentDBFS, true
	}
	return 20 * math.Log10(mean), true
}

// speechCrestDB is the peak-to-RMS ratio below which a track is not carrying
// speech, whatever else it is carrying.
//
// Speech is spiky: it runs 15 to 20 dB between its peaks and its average. A
// pure tone is exactly 3.01 dB and steady noise around 10. Measured on a real
// recording, a track that had captured a 440 Hz tone rather than a meeting came
// out at 3.0, which is how it was identified.
const speechCrestDB = 12.0

// crestDB samples a track and returns its peak-to-RMS ratio.
//
// Sampled rather than measured whole: a two-hour recording is gigabytes, and
// twenty windows spread across it answer the question just as well as all of it.
func crestDB(lr *levelReader, duration float64) (float64, bool) {
	const windows = 20
	const window = 1.0
	if duration < window {
		return 0, false
	}
	peak, rmsSum, n := silentDBFS, 0.0, 0
	for i := 0; i < windows; i++ {
		at := duration * float64(i) / float64(windows)
		if at+window > duration {
			break
		}
		p, okP := lr.peakDBFS(at, at+window)
		r, okR := lr.rms(at, at+window)
		if !okP || !okR || r <= silentDBFS {
			continue
		}
		if p > peak {
			peak = p
		}
		rmsSum += r
		n++
	}
	if n == 0 || peak <= silentDBFS {
		return 0, false
	}
	return peak - rmsSum/float64(n), true
}
