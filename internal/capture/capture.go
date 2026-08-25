// Package capture drives the platform capture helper and writes one file per
// track.
//
// This is R1's proof harness rather than the orchestrator: no segments, no
// manifest, no start/stop lifecycle. It exists to demonstrate that two aligned,
// non-silent tracks come out of a real machine, which is the only claim R1
// makes.
package capture

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/alexj/minutes/internal/frame"
	"github.com/alexj/minutes/internal/wav"
)

// Options configures a recording.
type Options struct {
	Helper string
	OutDir string
	Prefix string
	// Duration bounds the recording. Zero records until the context is
	// cancelled.
	Duration time.Duration
	// Log receives progress lines. Recording is a trust matter, so this is
	// used to make an active recording obvious rather than quiet.
	Log func(string, ...any)
}

// TrackSummary describes what one track actually captured.
type TrackSummary struct {
	Track         frame.Track
	Path          string
	Device        string
	SampleRate    int
	Channels      int
	Packets       int
	Duration      float64
	PaddedSeconds float64
	PeakDBFS      float64
	Discontinuity int
}

// Silent reports whether the track carries no signal above the floor of a
// 16-bit file. A track that is silent is the failure this whole design exists
// to catch, so it is computed and reported rather than left for someone to
// notice later.
func (t TrackSummary) Silent() bool { return t.PeakDBFS <= -90 }

// Summary is the outcome of a recording.
type Summary struct {
	Tracks []TrackSummary
	// EpochQPC100ns is the instant both files call sample zero. Their shared
	// origin is what makes the two transcripts mergeable later.
	EpochQPC100ns uint64
}

type trackState struct {
	info    frame.TrackInfo
	writer  *wav.Writer
	packets int
	peak    int16
	discont int
}

// Run records until Duration elapses or ctx is cancelled, and returns what was
// captured.
func Run(ctx context.Context, opt Options) (*Summary, error) {
	if opt.Log == nil {
		opt.Log = func(string, ...any) {}
	}
	if opt.Prefix == "" {
		opt.Prefix = "recording"
	}
	if err := os.MkdirAll(opt.OutDir, 0o755); err != nil {
		return nil, err
	}

	args := []string{}
	if opt.Duration > 0 {
		args = append(args, "--duration-ms", fmt.Sprintf("%d", opt.Duration.Milliseconds()))
	}

	cmd := exec.Command(opt.Helper, args...)

	// stdin is held open deliberately: closing it is how a Linux parent stops a
	// Windows child across the interop boundary, with no control channel and no
	// signal that survives the crossing.
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("starting capture helper: %w", err)
	}

	// Cancelling stops the helper by closing its stdin, letting it finish the
	// packet in hand and emit its END frames rather than dying mid-write.
	stopped := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			stdin.Close()
		case <-stopped:
		}
	}()

	tracks := map[frame.Track]*trackState{}
	var epoch uint64
	var epochSet bool

	reader := frame.NewReader(bufio.NewReaderSize(stdout, 1<<20))
	var readErr error

	for {
		f, err := reader.Next()
		if err != nil {
			if err != io.EOF {
				readErr = err
			}
			break
		}

		switch f.Type {
		case frame.TypeTrackInfo:
			info, err := frame.ParseTrackInfo(f.Payload)
			if err != nil {
				readErr = err
				break
			}
			path := filepath.Join(opt.OutDir, fmt.Sprintf("%s-%s.wav", opt.Prefix, f.Track))
			w, err := wav.NewWriter(path, int(info.SampleRate), int(info.Channels))
			if err != nil {
				readErr = err
				break
			}
			tracks[f.Track] = &trackState{info: info, writer: w}
			opt.Log("track %s: %s, %d Hz, %d ch -> %s",
				f.Track, info.Device, info.SampleRate, info.Channels, filepath.Base(path))

		case frame.TypeAudio:
			ts := tracks[f.Track]
			if ts == nil {
				// Audio before its TRACK_INFO means the stream is malformed;
				// guessing a format here would produce a file that plays as
				// noise, which is worse than stopping.
				readErr = fmt.Errorf("audio for %s before its track info", f.Track)
				break
			}
			if !epochSet {
				epoch, epochSet = f.QPC100ns, true
			}
			samples, err := wav.ToInt16(f.Payload, ts.info.FormatTag, ts.info.BitsPerSample)
			if err != nil {
				readErr = err
				break
			}

			// Where this packet belongs, from the clock both tracks share.
			var offset uint64
			if f.QPC100ns > epoch {
				offset = (f.QPC100ns - epoch) * uint64(ts.info.SampleRate) / 10_000_000
			}
			if err := ts.writer.WriteAt(offset, samples); err != nil {
				readErr = err
				break
			}
			ts.packets++
			if f.Flags&frame.FlagDiscontinuity != 0 {
				ts.discont++
			}
			for _, s := range samples {
				v := s
				if v == math.MinInt16 {
					v = math.MaxInt16
				} else if v < 0 {
					v = -v
				}
				if v > ts.peak {
					ts.peak = v
				}
			}

		case frame.TypeLog:
			opt.Log("helper[%s]: %s", f.Track, string(f.Payload))

		case frame.TypeEnd:
			opt.Log("track %s ended after %d audio frames", f.Track, f.DevicePos)
		}

		if readErr != nil {
			break
		}
	}

	close(stopped)
	waitErr := cmd.Wait()

	sum := &Summary{EpochQPC100ns: epoch}
	for _, id := range []frame.Track{frame.TrackMic, frame.TrackSystem} {
		ts, ok := tracks[id]
		if !ok {
			continue
		}
		t := TrackSummary{
			Track:         id,
			Path:          ts.writer.Path(),
			Device:        ts.info.Device,
			SampleRate:    int(ts.info.SampleRate),
			Channels:      int(ts.info.Channels),
			Packets:       ts.packets,
			Duration:      ts.writer.Duration(),
			PaddedSeconds: float64(ts.writer.PaddedFrames) / float64(ts.info.SampleRate),
			Discontinuity: ts.discont,
			PeakDBFS:      dbfs(ts.peak),
		}
		if err := ts.writer.Close(); err != nil {
			return nil, fmt.Errorf("closing %s: %w", t.Path, err)
		}
		sum.Tracks = append(sum.Tracks, t)
	}

	if readErr != nil {
		return sum, fmt.Errorf("reading capture stream: %w", readErr)
	}
	if waitErr != nil {
		return sum, fmt.Errorf("capture helper failed: %w", waitErr)
	}
	return sum, nil
}

func dbfs(peak int16) float64 {
	if peak <= 0 {
		return math.Inf(-1)
	}
	return 20 * math.Log10(float64(peak)/32767)
}
