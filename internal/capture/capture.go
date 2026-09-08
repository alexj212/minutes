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
	"math"
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

	// OnSilence is called once when every track that is delivering audio has
	// been below SilenceThresholdDBFS for SilenceAfter — nobody speaking and
	// nothing playing.
	//
	// EVERY track, because either one alone means the opposite of what it looks
	// like. A quiet microphone while the far end talks is somebody listening; a
	// quiet system track while the microphone carries speech is an ordinary
	// meeting with nothing being played. Only both together mean the room has
	// gone.
	//
	// Fires once per Run. The caller stops the recording; nothing here does.
	OnSilence func(since time.Duration)
	// SilenceAfter arms OnSilence. Zero disables it, and that is the default:
	// stopping a recording that is still a meeting loses the rest of it, so
	// this is opt-in rather than something every recording inherits.
	SilenceAfter time.Duration
	// SilenceThresholdDBFS is the level at or below which a packet counts as
	// silence. Zero uses DefaultSilenceThresholdDBFS.
	SilenceThresholdDBFS float64

	// Resume continues a recording that was stopped, rather than starting one.
	//
	// The manifest's epoch is kept, so the resumed audio is placed at its true
	// offset and the gap appears as the silence it was, and segments already on
	// disk are reopened rather than replaced.
	Resume bool
}

// peakAmplitude is the loudest sample in a packet, rectified.
func peakAmplitude(samples []int16) int16 {
	var pk int16
	for _, v := range samples {
		if v == math.MinInt16 {
			return math.MaxInt16
		}
		if v < 0 {
			v = -v
		}
		if v > pk {
			pk = v
		}
	}
	return pk
}

// DefaultSilenceThresholdDBFS is the level below which audio counts as silence
// for auto-stop.
//
// Above the floors already used downstream — -60 dBFS skips transcription
// entirely, and a track peaking below -40 dBFS is reported as carrying no
// speech — because this decides whether to stop a meeting rather than whether
// to transcribe a segment, and the two failures cost differently. A room that
// measured -55.7 dBFS with nothing happening in it is the case this must not
// treat as speech.
const DefaultSilenceThresholdDBFS = -45

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
		// lastLoud is when this track last delivered a packet above the
		// silence threshold. Zero means never, which is not the same as "long
		// ago": a track that has delivered nothing has established nothing
		// about whether the room is quiet.
		lastLoud time.Time
	}
	tracks := map[frame.Track]*trackState{}
	// Guards tracks against the watcher below. The reader owns every write;
	// the watcher only reads.
	var mu sync.Mutex
	var epoch uint64
	var epochSet bool
	// A resumed recording keeps the epoch it already had. Taking a new one from
	// the first packet after the gap would place the resumed audio at zero, so
	// it would overwrite the beginning of the meeting instead of following it.
	if opt.Resume && m.EpochQPC100ns != 0 {
		epoch, epochSet = m.EpochQPC100ns, true
	}
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

	// Silence is compared as an amplitude rather than in dBFS, so the check on
	// the hot path is an integer compare instead of a logarithm per packet.
	silenceArmed := opt.OnSilence != nil && opt.SilenceAfter > 0
	silenceAmp := int16(0)
	if silenceArmed {
		th := opt.SilenceThresholdDBFS
		if th == 0 {
			th = DefaultSilenceThresholdDBFS
		}
		if a := math.Round(math.MaxInt16 * math.Pow(10, th/20)); a >= 1 && a < math.MaxInt16 {
			silenceAmp = int16(a)
		}
		opt.Log("auto-stop armed: stopping after %s with every track at or below %.0f dBFS",
			opt.SilenceAfter, th)

		go func() {
			tick := opt.SilenceAfter / 10
			if tick < 10*time.Millisecond {
				tick = 10 * time.Millisecond
			}
			t := time.NewTicker(tick)
			defer t.Stop()
			saidUnarmed := false
			for {
				select {
				case <-stopped:
					return
				case <-t.C:
				}
				now := time.Now()

				mu.Lock()
				// A track that has delivered nothing has established nothing.
				// Treating it as quiet would let one dead endpoint stop a
				// meeting the other track is still recording, so it disarms the
				// check instead — and says so, because an auto-stop that
				// silently never fires is the failure this project keeps
				// finding.
				var unknown []string
				quiet, armed := 0, 0
				oldest := now
				for id, ts := range tracks {
					if ts.audio == 0 {
						unknown = append(unknown, id.String())
						continue
					}
					armed++
					if now.Sub(ts.lastLoud) >= opt.SilenceAfter {
						quiet++
					}
					if ts.lastLoud.Before(oldest) {
						oldest = ts.lastLoud
					}
				}
				fire := armed > 0 && len(unknown) == 0 && quiet == armed
				mu.Unlock()

				if len(unknown) > 0 {
					if !saidUnarmed {
						saidUnarmed = true
						opt.Log("auto-stop is not armed: %s has delivered no audio at all, "+
							"so there is nothing to call quiet. The recording will run until "+
							"it is stopped.", strings.Join(unknown, " and "))
					}
					continue
				}
				if fire {
					opt.OnSilence(now.Sub(oldest))
					return
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
			if opt.Resume {
				// Only when resuming. On a fresh recording a segment file that
				// somehow already exists is debris, and appending this
				// recording's audio to it would be worse than replacing it.
				sw.PriorSegment = func(index int) (manifest.Segment, bool) {
					return m.FindSegment(name, index)
				}
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
			if silenceArmed {
				// Set on the first packet whatever its level, so "quiet since"
				// is measured from when the track started delivering rather
				// than from the zero time, which would fire instantly.
				if ts.lastLoud.IsZero() || peakAmplitude(samples) > silenceAmp {
					ts.lastLoud = time.Now()
				}
			}
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
