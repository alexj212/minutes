// Package capture drives the platform capture helper and writes each track to
// disk as a series of segments, keeping a manifest beside them.
//
// It does not decide when to record or where the notes go. Both of those are
// judgment calls, and a session makes them; this only does the part that has to
// happen on the machine with the audio hardware.
package capture

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"time"

	"github.com/alexj/minutes/internal/frame"
	"github.com/alexj/minutes/internal/manifest"
	"github.com/alexj/minutes/internal/segment"
	"github.com/alexj/minutes/internal/timeline"
	"github.com/alexj/minutes/internal/wav"
)

// Options configures a recording.
type Options struct {
	Helper   string
	Manifest *manifest.Manifest
	// Duration bounds the recording. Zero records until ctx is cancelled.
	Duration time.Duration
	Log      func(string, ...any)
}

// Run records until Duration elapses or ctx is cancelled.
//
// The manifest is updated as segments open and close, so it describes what is
// on disk at every moment rather than only at the end.
func Run(ctx context.Context, opt Options) error {
	if opt.Log == nil {
		opt.Log = func(string, ...any) {}
	}
	m := opt.Manifest
	if m == nil {
		return fmt.Errorf("capture needs a manifest")
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
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("starting capture helper: %w", err)
	}

	// Cancelling closes the helper's stdin, letting it finish the packet in
	// hand and emit its END frames rather than dying mid-write.
	stopped := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			stdin.Close()
		case <-stopped:
		}
	}()

	type trackState struct {
		info   frame.TrackInfo
		writer *segment.Writer
		// place is created with the first audio packet, because it needs the
		// shared epoch and that is not known until one arrives.
		place *timeline.Track
	}
	tracks := map[frame.Track]*trackState{}
	var epoch uint64
	var epochSet bool
	var runErr error

	reader := frame.NewReader(bufio.NewReaderSize(stdout, 1<<20))

loop:
	for {
		f, err := reader.Next()
		if err != nil {
			if err != io.EOF {
				runErr = fmt.Errorf("reading capture stream: %w", err)
			}
			break
		}

		switch f.Type {
		case frame.TypeTrackInfo:
			info, err := frame.ParseTrackInfo(f.Payload)
			if err != nil {
				runErr = err
				break loop
			}
			name := f.Track.String()
			sw, err := segment.NewWriter(m.Dir(), name,
				int(info.SampleRate), int(info.Channels), m.SegmentSeconds)
			if err != nil {
				runErr = err
				break loop
			}
			sw.OnSegment = func(seg manifest.Segment) error {
				return m.PutSegment(name, seg)
			}
			if err := m.SetTrack(name, info.Device, int(info.SampleRate), int(info.Channels)); err != nil {
				runErr = err
				break loop
			}
			tracks[f.Track] = &trackState{info: info, writer: sw}
			opt.Log("track %s: %s, %d Hz, %d ch", name, info.Device, info.SampleRate, info.Channels)

		case frame.TypeAudio:
			ts := tracks[f.Track]
			if ts == nil {
				// Audio before its TRACK_INFO means the stream is malformed.
				// Guessing a format here produces a file that plays as noise,
				// which is worse than stopping.
				runErr = fmt.Errorf("audio for %s before its track info", f.Track)
				break loop
			}
			if !epochSet {
				epoch, epochSet = f.QPC100ns, true
				if err := m.SetEpoch(epoch); err != nil {
					runErr = err
					break loop
				}
			}
			samples, err := wav.ToInt16(f.Payload, ts.info.FormatTag, ts.info.BitsPerSample)
			if err != nil {
				runErr = err
				break loop
			}
			if ts.place == nil {
				ts.place = timeline.NewTrack(uint64(ts.info.SampleRate), epoch)
			}
			offset := ts.place.Place(f.QPC100ns, f.DevicePos)
			if err := ts.writer.WriteAt(offset, samples, f.Flags); err != nil {
				runErr = err
				break loop
			}

		case frame.TypeLog:
			opt.Log("helper[%s]: %s", f.Track, string(f.Payload))

		case frame.TypeEnd:
			opt.Log("track %s ended after %d audio frames", f.Track, f.DevicePos)
		}
	}

	close(stopped)

	// Close the segments before reporting anything, so the manifest is final
	// even on the failure path. A recording that ended badly still has whatever
	// it captured, and that is the part worth keeping.
	for id, ts := range tracks {
		if err := ts.writer.Close(); err != nil && runErr == nil {
			runErr = err
		}
		// A re-anchor means the endpoint's sample counter and the wall clock
		// disagreed by more than jitter. It is recorded rather than swallowed,
		// because a later phase merges two transcripts on this timeline.
		if ts.place != nil && ts.place.Reanchors > 0 {
			opt.Log("track %s: re-anchored %d time(s) — the device clock and the wall clock disagreed",
				id, ts.place.Reanchors)
			if err := m.SetReanchors(id.String(), ts.place.Reanchors); err != nil && runErr == nil {
				runErr = err
			}
		}
	}

	waitErr := cmd.Wait()
	if runErr == nil && waitErr != nil {
		runErr = fmt.Errorf("capture helper failed: %w", waitErr)
	}
	return runErr
}
