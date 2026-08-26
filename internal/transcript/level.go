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

// peakDBFS returns the loudest sample between start and end, in seconds from
// the recording epoch, or ok=false when the range is not on disk.
func (lr *levelReader) peakDBFS(start, end float64) (float64, bool) {
	if lr.spec.SampleRate == 0 || lr.spec.Channels == 0 || lr.seg <= 0 {
		return 0, false
	}
	index := int(start / lr.seg)
	within := start - float64(index)*lr.seg
	if within < 0 {
		return 0, false
	}
	// A range crossing a segment boundary is measured only up to it. Fragments
	// are a second or so; losing the tail of one changes nothing.
	if end-start <= 0 {
		return 0, false
	}
	if within+(end-start) > lr.seg {
		end = start + (lr.seg - within)
	}

	path := filepath.Join(lr.dir, fmt.Sprintf("%s-%03d.wav", lr.spec.Name, index))
	f, err := os.Open(path)
	if err != nil {
		return 0, false
	}
	defer f.Close()

	blockAlign := int64(lr.spec.Channels * 2)
	rate := float64(lr.spec.SampleRate)
	off := wavHeaderSize + int64(within*rate)*blockAlign
	n := int64((end-start)*rate) * blockAlign
	if n <= 0 {
		return 0, false
	}

	buf := make([]byte, n)
	read, err := f.ReadAt(buf, off)
	if read <= 0 {
		return 0, false
	}
	_ = err // a short read at the end of a segment is fine; measure what is there
	buf = buf[:read-read%2]

	var peak int16
	for i := 0; i+1 < len(buf); i += 2 {
		v := int16(binary.LittleEndian.Uint16(buf[i:]))
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
