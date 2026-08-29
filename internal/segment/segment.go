// Package segment writes one track as a series of fixed-length chunks.
//
// Chunking bounds what a crash costs. It also bounds what a single file costs
// to move, hash or re-transcribe, which matters more than it sounds: a
// ninety-minute meeting in one file is a unit of work nothing can subdivide.
//
// Boundaries are wall-clock positions measured from the recording's shared
// epoch, not counts of packets or of bytes. That is what makes segment k of the
// microphone cover the same window as segment k of the system track even though
// the two run at different sample rates — on the target machine, 48000 and
// 44100. A later phase can transcribe the pair and merge them without
// re-deriving any alignment.
package segment

import (
	"fmt"
	"math"
	"path/filepath"

	"github.com/alexj/minutes/internal/manifest"
	"github.com/alexj/minutes/internal/wav"
)

// syncEvery bounds how much audio may be written before the open segment's
// header and manifest entry are refreshed. It is the size of the window a kill
// can destroy: a WAV whose header still says zero bytes has its audio on disk
// but not addressable, so the interval is the real answer to "what does a crash
// cost".
const syncEvery = 5.0

// syncInterval is syncEvery, or a quarter of the segment if the segment is
// shorter. Without the second clause a segment briefer than syncEvery never
// syncs at all, and an interrupted one is left with a zeroed header — measured:
// a 4-second segment killed in progress declared 0 data bytes while holding
// 4972 of them.
func syncInterval(segmentSeconds float64) float64 {
	if q := segmentSeconds / 4; q < syncEvery {
		return q
	}
	return syncEvery
}

// Writer accumulates one track into rotating segment files.
type Writer struct {
	dir        string
	track      string
	sampleRate int
	channels   int

	framesPerSegment uint64
	framesPerSync    uint64

	cur        *wav.Writer
	curIndex   int
	curPackets int
	curPeak    int16
	// curMin and curMax are the raw extremes, unrectified, for deciding whether
	// the signal is constant. curPeak cannot answer that: it is an amplitude,
	// so a track pinned at a non-zero DC offset and a track carrying real audio
	// both give it something to report.
	curMin       int16
	curMax       int16
	sawData      bool
	haveCur      bool
	framesToSync uint64

	// OnSegment is called when a segment opens and again when it closes, so the
	// manifest on disk always names the file currently being written.
	OnSegment func(manifest.Segment) error
}

// NewWriter creates a segmented writer. segmentSeconds must be positive.
func NewWriter(dir, track string, sampleRate, channels int, segmentSeconds float64) (*Writer, error) {
	if segmentSeconds <= 0 {
		return nil, fmt.Errorf("segment length must be positive, got %v", segmentSeconds)
	}
	if sampleRate <= 0 || channels <= 0 {
		return nil, fmt.Errorf("invalid track format: rate=%d channels=%d", sampleRate, channels)
	}
	fps := uint64(math.Round(segmentSeconds * float64(sampleRate)))
	if fps == 0 {
		return nil, fmt.Errorf("segment of %vs is shorter than one frame at %d Hz", segmentSeconds, sampleRate)
	}
	sync := uint64(math.Round(syncInterval(segmentSeconds) * float64(sampleRate)))
	if sync == 0 {
		sync = 1
	}
	return &Writer{
		dir:              dir,
		track:            track,
		sampleRate:       sampleRate,
		channels:         channels,
		framesPerSegment: fps,
		framesPerSync:    sync,
	}, nil
}

// FileName is the on-disk name of a segment.
func FileName(track string, index int) string {
	return fmt.Sprintf("%s-%03d.wav", track, index)
}

// WriteAt places samples at an absolute frame offset from the recording epoch,
// splitting the packet across segment boundaries where it straddles one.
func (w *Writer) WriteAt(frameOffset uint64, samples []int16, packetFlags uint32) error {
	if len(samples) == 0 {
		return nil
	}
	if len(samples)%w.channels != 0 {
		return fmt.Errorf("packet of %d samples is not whole frames at %d channels", len(samples), w.channels)
	}

	counted := false
	offset := frameOffset
	rest := samples

	for len(rest) > 0 {
		index := int(offset / w.framesPerSegment)
		within := offset % w.framesPerSegment

		if !w.haveCur || index != w.curIndex {
			if err := w.rotate(index); err != nil {
				return err
			}
		}

		// How many frames fit before this segment's boundary. A packet that
		// straddles one is split rather than pushed into either side, because
		// letting it spill would move the boundary and the two tracks would
		// stop agreeing on where segment k ends.
		space := w.framesPerSegment - within
		frames := uint64(len(rest) / w.channels)
		take := frames
		if take > space {
			take = space
		}

		chunk := rest[:take*uint64(w.channels)]
		if err := w.cur.WriteAt(within, chunk); err != nil {
			return err
		}
		for _, s := range chunk {
			if !w.sawData {
				w.curMin, w.curMax, w.sawData = s, s, true
			} else {
				if s < w.curMin {
					w.curMin = s
				}
				if s > w.curMax {
					w.curMax = s
				}
			}
			v := s
			if v == math.MinInt16 {
				v = math.MaxInt16
			} else if v < 0 {
				v = -v
			}
			if v > w.curPeak {
				w.curPeak = v
			}
		}
		// A packet split across a boundary is one packet, counted where it
		// started, so the manifest's packet counts sum to what was captured.
		if !counted {
			w.curPackets++
			counted = true
		}

		w.framesToSync += take
		if w.framesToSync >= w.framesPerSync {
			if err := w.cur.Sync(); err != nil {
				return err
			}
			// Refresh the manifest entry alongside the header. Written only at
			// open and close, an in-progress segment's entry would stay at zero
			// frames for up to a whole chunk, so an interrupted recording would
			// understate audio that is sitting on the disk.
			if err := w.emit(false); err != nil {
				return err
			}
			w.framesToSync = 0
		}

		rest = rest[take*uint64(w.channels):]
		offset += take
	}
	return nil
}

// rotate closes the open segment and opens the one at index.
//
// Segments between them are never created. A gap long enough to skip a whole
// segment means nothing was captured for that window, and writing five minutes
// of zeros to say so would cost tens of megabytes per skipped chunk; the
// manifest's missing index says it for free.
func (w *Writer) rotate(index int) error {
	if err := w.closeCurrent(); err != nil {
		return err
	}
	path := filepath.Join(w.dir, FileName(w.track, index))
	f, err := wav.NewWriter(path, w.sampleRate, w.channels)
	if err != nil {
		return err
	}
	w.cur, w.curIndex, w.haveCur = f, index, true
	w.curPackets, w.curPeak, w.framesToSync = 0, 0, 0
	w.curMin, w.curMax, w.sawData = 0, 0, false

	// Announced before any audio is in it, so a recording killed one second
	// later still has this file named in the manifest rather than orphaned
	// beside it.
	return w.emit(false)
}

func (w *Writer) closeCurrent() error {
	if !w.haveCur {
		return nil
	}
	if err := w.cur.Close(); err != nil {
		return err
	}
	err := w.emit(true)
	w.haveCur = false
	w.cur = nil
	return err
}

func (w *Writer) emit(complete bool) error {
	if w.OnSegment == nil {
		return nil
	}
	seg := manifest.Segment{
		Index:           w.curIndex,
		File:            FileName(w.track, w.curIndex),
		StartSeconds:    float64(w.curIndex) * float64(w.framesPerSegment) / float64(w.sampleRate),
		DurationSeconds: w.cur.Duration(),
		Frames:          w.cur.Frames(),
		PaddedFrames:    w.cur.PaddedFrames,
		PeakDBFS:        dbfs(w.curPeak),
		Constant:        w.sawData && w.curMin == w.curMax,
		Packets:         w.curPackets,
		Complete:        complete,
	}
	if complete {
		if sum, size, err := manifest.HashFile(w.cur.Path()); err == nil {
			seg.SHA256, seg.Size = sum, size
		}
	}
	return w.OnSegment(seg)
}

// Close finishes the open segment.
func (w *Writer) Close() error { return w.closeCurrent() }

// silentDBFS is the floor reported for a segment with no signal.
//
// Not -Inf: the manifest is JSON, and encoding/json refuses infinities. A
// silent track is precisely the case that most needs to be written down, so the
// value that represents it has to survive being written down.
const silentDBFS = -999.0

func dbfs(peak int16) float64 {
	if peak <= 0 {
		return silentDBFS
	}
	return 20 * math.Log10(float64(peak)/32767)
}
