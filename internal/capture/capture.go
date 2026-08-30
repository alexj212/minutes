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
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/alexj212/minutes/internal/frame"
	"github.com/alexj212/minutes/internal/manifest"
	"github.com/alexj212/minutes/internal/segment"
	"github.com/alexj212/minutes/internal/timeline"
	"github.com/alexj212/minutes/internal/wav"
)

// Options configures a recording.
type Options struct {
	Helper   string
	Manifest *manifest.Manifest
	// Duration bounds the recording. Zero records until ctx is cancelled.
	Duration time.Duration
	// AppPID captures only that process and its children rather than
	// everything the machine plays. Zero means system-wide.
	AppPID int
	Log    func(string, ...any)
	// OnNoAudio is called once per track that has been declared but has not
	// delivered a single audio frame, after NoAudioAfter has elapsed.
	//
	// It exists because the log is not where this can be reported. A supervised
	// recording writes the log to a file, and on 2026-08-27 the helper's own
	// "track mic ended after 0 audio frames" sat in one for two days while a
	// 44-minute meeting was transcribed and delivered with half of it missing.
	// Whoever wires this is expected to put it somewhere a person will see it
	// while the meeting is still happening.
	OnNoAudio func(track string, since time.Duration)
	// NoAudioAfter is how long a declared track may deliver nothing before
	// OnNoAudio fires. Zero uses DefaultNoAudioAfter.
	NoAudioAfter time.Duration
}

// DefaultNoAudioAfter is how long a track may deliver nothing before it is
// reported.
//
// Long enough not to fire on the ordinary case: a loopback stream delivers no
// packets at all while the render endpoint is idle, and it is idle at the start
// of every recording, until something plays. A minute of a meeting with nothing
// audible is possible; it is also worth being told about, which is why the
// wording reports the fact rather than diagnosing it.
const DefaultNoAudioAfter = 60 * time.Second

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
	if opt.AppPID > 0 {
		args = append(args, "--app-pid", fmt.Sprintf("%d", opt.AppPID))
	}
	cmd := exec.Command(opt.Helper, args...)

	// Its own process group, so a terminal Ctrl-C does not reach it.
	//
	// Ctrl-C signals the whole foreground group, and the helper caught in that
	// died non-zero — which the orchestrator then reported as a failed
	// recording, over audio that was captured perfectly well. Stopping is
	// supposed to happen by closing the helper's stdin, and that is the only
	// route that lets it finish the packet in hand and emit its END frames.
	//
	// Nothing is orphaned by this: if the orchestrator dies without closing
	// stdin, the pipe closes when its process exits, and the helper sees EOF
	// and stops anyway.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

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
		// audio counts packets, not samples. The question it answers is
		// "has anything at all arrived on this track", which is the one nobody
		// was asking.
		audio    uint64
		reported bool
	}
	tracks := map[frame.Track]*trackState{}
	// Guards tracks against the watcher below. The reader owns every write;
	// the watcher only reads.
	var mu sync.Mutex
	var epoch uint64
	var epochSet bool
	var runErr error
	// The helper reports why it died in a LOG frame. Keeping the last one per
	// track means a failed recording's manifest can say "the audio device was
	// removed" rather than "exit status 1".
	lastLog := map[frame.Track]string{}

	// Watching for a track that never produces anything.
	//
	// Deliberately not a level check in preflight: on 2026-08-27 the device
	// opened cleanly, reported its name and its sample rate, and then delivered
	// nothing for 44 minutes. Preflight was happy. The only place this is
	// visible is here, while the frames are — or are not — arriving.
	after := opt.NoAudioAfter
	if after <= 0 {
		after = DefaultNoAudioAfter
	}
	if opt.OnNoAudio != nil {
		go func() {
			// Derived from the threshold rather than fixed, so detection is
			// prompt at any threshold and a test does not have to wait out a
			// production-sized one.
			tick := after / 10
			if tick < 10*time.Millisecond {
				tick = 10 * time.Millisecond
			}
			t := time.NewTicker(tick)
			defer t.Stop()
			started := time.Now()
			for {
				select {
				case <-stopped:
					return
				case <-t.C:
				}
				elapsed := time.Since(started)
				if elapsed < after {
					continue
				}
				mu.Lock()
				var quiet []string
				for id, ts := range tracks {
					if ts.audio == 0 && !ts.reported {
						ts.reported = true
						quiet = append(quiet, id.String())
					}
				}
				mu.Unlock()
				for _, name := range quiet {
					opt.OnNoAudio(name, elapsed)
				}
			}
		}()
	}

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
			info.ProcessScoped = f.Flags&frame.FlagProcessScoped != 0
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
			mu.Lock()
			tracks[f.Track] = &trackState{info: info, writer: sw}
			mu.Unlock()
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
				if ts.info.ProcessScoped {
					ts.place = timeline.NewClockTrack(uint64(ts.info.SampleRate), epoch)
				} else {
					ts.place = timeline.NewTrack(uint64(ts.info.SampleRate), epoch)
				}
			}
			mu.Lock()
			ts.audio++
			mu.Unlock()
			offset := ts.place.Place(f.QPC100ns, f.DevicePos)
			if err := ts.writer.WriteAt(offset, samples, f.Flags); err != nil {
				runErr = err
				break loop
			}

		case frame.TypeLog:
			lastLog[f.Track] = string(f.Payload)
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
		// A non-zero exit means a track never started or died while running.
		// Either way this recording is half a meeting, and the manifest is about
		// to record it as failed — so it should carry the reason.
		if why := reasons(lastLog); why != "" {
			runErr = fmt.Errorf("capture helper failed: %s", why)
		} else {
			runErr = fmt.Errorf("capture helper failed: %w", waitErr)
		}
	}
	return runErr
}

// reasons renders what each track last reported, in a stable order.
func reasons(lastLog map[frame.Track]string) string {
	var parts []string
	for _, id := range []frame.Track{frame.TrackMic, frame.TrackSystem} {
		if msg := lastLog[id]; msg != "" {
			parts = append(parts, fmt.Sprintf("%s track: %s", id, msg))
		}
	}
	return strings.Join(parts, "; ")
}
