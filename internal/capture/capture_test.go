package capture

import (
	"context"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/alexj212/minutes/internal/frame"
	"github.com/alexj212/minutes/internal/manifest"
)

// buildFrame assembles one frame in the layout docs/protocol.md specifies.
func buildFrame(typ frame.Type, track frame.Track, qpc, devicePos uint64, flags uint32, payload []byte) []byte {
	h := make([]byte, frame.HeaderSize)
	binary.LittleEndian.PutUint32(h[0:], frame.Magic)
	binary.LittleEndian.PutUint16(h[4:], uint16(typ))
	binary.LittleEndian.PutUint16(h[6:], uint16(track))
	binary.LittleEndian.PutUint64(h[8:], qpc)
	binary.LittleEndian.PutUint64(h[16:], devicePos)
	binary.LittleEndian.PutUint32(h[24:], uint32(len(payload)))
	binary.LittleEndian.PutUint32(h[28:], flags)
	return append(h, payload...)
}

func trackInfoPayload(rate uint32, channels, bits, tag, blockAlign uint16, device string) []byte {
	p := make([]byte, 24+len(device))
	binary.LittleEndian.PutUint32(p[0:], rate)
	binary.LittleEndian.PutUint16(p[4:], channels)
	binary.LittleEndian.PutUint16(p[6:], bits)
	binary.LittleEndian.PutUint16(p[8:], tag)
	binary.LittleEndian.PutUint16(p[10:], blockAlign)
	binary.LittleEndian.PutUint64(p[12:], 10_000_000)
	binary.LittleEndian.PutUint32(p[20:], uint32(len(device)))
	copy(p[24:], device)
	return p
}

// pcm16 returns n mono frames of non-silent 16-bit audio.
func pcm16(n int) []byte {
	b := make([]byte, n*2)
	for i := 0; i < n; i++ {
		binary.LittleEndian.PutUint16(b[i*2:], uint16(int16(1000+i%500)))
	}
	return b
}

// fakeHelper writes an executable that emits the given bytes on stdout and then
// exits with the given code, standing in for the Windows capture helper.
func fakeHelper(t *testing.T, stdout []byte, exitCode int) string {
	t.Helper()
	dir := t.TempDir()
	data := filepath.Join(dir, "frames.bin")
	if err := os.WriteFile(data, stdout, 0o644); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(dir, "helper")
	body := fmt.Sprintf("#!/bin/sh\ncat %q\nexit %d\n", data, exitCode)
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return script
}

func newManifest(t *testing.T) *manifest.Manifest {
	t.Helper()
	m := manifest.New(t.TempDir(), "rec-1", "", 60)
	if err := m.Save(); err != nil {
		t.Fatal(err)
	}
	return m
}

// A capture that dies while running is half a meeting, and must be reported as
// a failure carrying the reason.
//
// This used to exit zero and be recorded as a clean stop, which meant a meeting
// cut in half by an unplugged headset looked exactly like a meeting somebody
// chose to end there.
func TestMidStreamDeviceFailureIsReportedWithItsReason(t *testing.T) {
	const reason = "GetBuffer failed mid-recording: 0x88890004 " +
		"(the audio device was removed, disabled, or the default endpoint changed)"

	var stream []byte
	stream = append(stream, buildFrame(frame.TypeTrackInfo, frame.TrackSystem, 0, 0, 0,
		trackInfoPayload(48000, 1, 16, frame.FormatPCM, 2, "Speakers"))...)
	stream = append(stream, buildFrame(frame.TypeAudio, frame.TrackSystem, 0, 0, 0, pcm16(4800))...)
	stream = append(stream, buildFrame(frame.TypeLog, frame.TrackSystem, 0, 0, 0, []byte(reason))...)
	stream = append(stream, buildFrame(frame.TypeEnd, frame.TrackSystem, 0, 4800, 0, nil)...)

	m := newManifest(t)
	err := Run(context.Background(), Options{
		Helper:   fakeHelper(t, stream, 1),
		Manifest: m,
	})
	if err == nil {
		t.Fatal("a capture that died mid-recording returned no error; it would be recorded as a clean stop")
	}
	if !strings.Contains(err.Error(), "device was removed") {
		t.Errorf("error does not carry the reason, so the manifest cannot either:\n  %v", err)
	}

	// The caller records that error, and this is what somebody reads later.
	if ferr := m.Finish(err); ferr != nil {
		t.Fatal(ferr)
	}
	got, lerr := manifest.Load(m.Dir())
	if lerr != nil {
		t.Fatal(lerr)
	}
	if got.State != manifest.StateFailed {
		t.Errorf("manifest state is %q, want %q", got.State, manifest.StateFailed)
	}
	if !strings.Contains(got.Error, "device was removed") {
		t.Errorf("manifest does not say why the recording ended: %q", got.Error)
	}
}

// Audio captured before the failure is still on disk and still listed. A
// recording that ended badly keeps what it got.
func TestAudioBeforeAFailureIsKept(t *testing.T) {
	var stream []byte
	stream = append(stream, buildFrame(frame.TypeTrackInfo, frame.TrackMic, 0, 0, 0,
		trackInfoPayload(48000, 1, 16, frame.FormatPCM, 2, "Mic"))...)
	stream = append(stream, buildFrame(frame.TypeAudio, frame.TrackMic, 0, 0, 0, pcm16(48000))...)
	stream = append(stream, buildFrame(frame.TypeLog, frame.TrackMic, 0, 0, 0, []byte("GetBuffer failed mid-recording"))...)

	m := newManifest(t)
	if err := Run(context.Background(), Options{Helper: fakeHelper(t, stream, 1), Manifest: m}); err == nil {
		t.Fatal("expected an error")
	}
	got, err := manifest.Load(m.Dir())
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Tracks) != 1 || len(got.Tracks[0].Segments) != 1 {
		t.Fatalf("the captured second was not recorded: %+v", got.Tracks)
	}
	seg := got.Tracks[0].Segments[0]
	if seg.Frames == 0 {
		t.Error("the segment holds no frames")
	}
	if _, err := os.Stat(filepath.Join(m.Dir(), seg.File)); err != nil {
		t.Errorf("the audio file is not on disk: %v", err)
	}
}

// A helper that exits cleanly is not a failure, and must not be reported as
// one — a recorder that cried wolf on every meeting would be ignored on the one
// that mattered.
func TestCleanCaptureIsNotAFailure(t *testing.T) {
	var stream []byte
	stream = append(stream, buildFrame(frame.TypeTrackInfo, frame.TrackMic, 0, 0, 0,
		trackInfoPayload(48000, 1, 16, frame.FormatPCM, 2, "Mic"))...)
	stream = append(stream, buildFrame(frame.TypeAudio, frame.TrackMic, 0, 0, 0, pcm16(4800))...)
	stream = append(stream, buildFrame(frame.TypeEnd, frame.TrackMic, 0, 4800, 0, nil)...)

	m := newManifest(t)
	if err := Run(context.Background(), Options{Helper: fakeHelper(t, stream, 0), Manifest: m}); err != nil {
		t.Fatalf("a clean capture was reported as a failure: %v", err)
	}
	if err := m.Finish(nil); err != nil {
		t.Fatal(err)
	}
	got, err := manifest.Load(m.Dir())
	if err != nil {
		t.Fatal(err)
	}
	if got.State != manifest.StateStopped {
		t.Errorf("state is %q, want %q", got.State, manifest.StateStopped)
	}
}

// A malformed stream must stop rather than be interpreted. Audio before its
// format description would otherwise be written with a guessed format, which
// produces a file that plays as noise.
func TestAudioBeforeTrackInfoIsRefused(t *testing.T) {
	stream := buildFrame(frame.TypeAudio, frame.TrackMic, 0, 0, 0, pcm16(100))
	m := newManifest(t)
	err := Run(context.Background(), Options{Helper: fakeHelper(t, stream, 0), Manifest: m})
	if err == nil {
		t.Fatal("audio arriving before its track info was accepted")
	}
	if !strings.Contains(err.Error(), "before its track info") {
		t.Errorf("unexpected error: %v", err)
	}
}

// A terminal Ctrl-C signals the whole foreground process group. The helper must
// not be in it.
//
// This is a regression test for a real one: the helper caught by Ctrl-C died
// non-zero, and the orchestrator reported "recording failed" over audio it had
// captured perfectly well. Stopping is supposed to happen by closing the
// helper's stdin, which is the only route that lets it finish the packet in
// hand and emit its END frames.
func TestHelperRunsInItsOwnProcessGroup(t *testing.T) {
	dir := t.TempDir()
	data := filepath.Join(dir, "frames.bin")

	var stream []byte
	stream = append(stream, buildFrame(frame.TypeTrackInfo, frame.TrackMic, 0, 0, 0,
		trackInfoPayload(48000, 1, 16, frame.FormatPCM, 2, "Mic"))...)
	stream = append(stream, buildFrame(frame.TypeAudio, frame.TrackMic, 0, 0, 0, pcm16(4800))...)
	if err := os.WriteFile(data, stream, 0o644); err != nil {
		t.Fatal(err)
	}

	pgidFile := filepath.Join(dir, "pgid")
	script := filepath.Join(dir, "helper")
	body := fmt.Sprintf("#!/bin/sh\nps -o pgid= -p $$ | tr -d ' ' > %q\ncat %q\n", pgidFile, data)
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}

	m := newManifest(t)
	if err := Run(context.Background(), Options{Helper: script, Manifest: m}); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(pgidFile)
	if err != nil {
		t.Fatalf("the helper did not report its process group: %v", err)
	}
	helperPGID, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatalf("unreadable process group %q: %v", raw, err)
	}

	ourPGID := syscall.Getpgrp()
	if helperPGID == ourPGID {
		t.Errorf("the helper shares our process group (%d), so a terminal Ctrl-C would "+
			"kill it mid-write and the recording would be reported as failed", helperPGID)
	}
}

// liveHelper emits the given bytes and then stays alive, so a watcher that
// only matters while a recording is running has something to watch.
func liveHelper(t *testing.T, stdout []byte, hold string) string {
	t.Helper()
	dir := t.TempDir()
	data := filepath.Join(dir, "frames.bin")
	if err := os.WriteFile(data, stdout, 0o644); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(dir, "helper")
	body := fmt.Sprintf("#!/bin/sh\ncat %q\nsleep %s\n", data, hold)
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return script
}

// A track that has been declared and delivers nothing has to be reported while
// the meeting is still happening, because that is the only time anybody can
// still fix it.
//
// On 2026-08-27 a 44-minute standup captured 8 segments of system audio and
// zero microphone frames. The helper wrote "track mic ended after 0 audio
// frames" into a log, the recording was transcribed and delivered with one side
// of the conversation missing, and nobody noticed for two days.
//
// Both tracks are exercised in ONE run, deliberately. A test that only checked
// the silent track would pass just as happily if the watcher reported every
// track, and a watcher that fires on everything is as useless as one that fires
// on nothing — it just fails in the opposite direction. What has to be true is
// that it tells them apart.
func TestATrackDeliveringNothingIsReportedWhileRecording(t *testing.T) {
	var out []byte
	out = append(out, buildFrame(frame.TypeTrackInfo, frame.TrackMic, 0, 0, 0,
		trackInfoPayload(48000, 1, 16, 1, 2, "Some Microphone"))...)
	out = append(out, buildFrame(frame.TypeTrackInfo, frame.TrackSystem, 0, 0, 0,
		trackInfoPayload(48000, 1, 16, 1, 2, "Some Speakers"))...)
	// Only the system track ever delivers audio. The microphone is declared and
	// silent, which is exactly the 2026-08-27 shape.
	out = append(out, buildFrame(frame.TypeAudio, frame.TrackSystem, 10_000_000, 0, 0, pcm16(480))...)

	var mu sync.Mutex
	reported := map[string]time.Duration{}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := Run(ctx, Options{
		Helper:       liveHelper(t, out, "1"),
		Manifest:     newManifest(t),
		NoAudioAfter: 100 * time.Millisecond,
		OnNoAudio: func(track string, since time.Duration) {
			mu.Lock()
			defer mu.Unlock()
			reported[track] = since
		},
	})
	if err != nil {
		t.Fatalf("capture failed: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if _, ok := reported["mic"]; !ok {
		t.Error("a declared track that delivered nothing was never reported")
	}
	if _, ok := reported["system"]; ok {
		t.Error("a track that delivered audio was reported as silent")
	}
	if len(reported) != 1 {
		t.Errorf("reported %v, want exactly the mic", reported)
	}
	if d := reported["mic"]; d < 100*time.Millisecond {
		t.Errorf("reported after %s, before the threshold had elapsed", d)
	}
}

// streamingHelper emits the track info once and then a packet pair every
// `delay` seconds, `times` over.
//
// The difference from liveHelper matters and it is what the first version of
// this test got wrong: `cat` delivers every packet at once and then the stream
// sits idle, so tracks carrying loud audio look quiet a moment later and
// auto-stop fires on a meeting in full flow. A live capture keeps delivering,
// and "quiet" only means anything against a stream that is still arriving.
func streamingHelper(t *testing.T, info, packet []byte, times int, delay string) string {
	t.Helper()
	dir := t.TempDir()
	infoPath := filepath.Join(dir, "info.bin")
	pktPath := filepath.Join(dir, "packet.bin")
	if err := os.WriteFile(infoPath, info, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pktPath, packet, 0o644); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(dir, "streaming-helper")
	body := fmt.Sprintf("#!/bin/sh\ncat %q\ni=0\nwhile [ $i -lt %d ]; do cat %q; sleep %s; i=$((i+1)); done\n",
		infoPath, times, pktPath, delay)
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return script
}

// twoPhaseHelper emits the track info, then `firstN` copies of one packet set,
// then `secondN` of another — a capture where one track starts late.
func twoPhaseHelper(t *testing.T, info, first []byte, firstN int, second []byte, secondN int, delay string) string {
	t.Helper()
	dir := t.TempDir()
	w := func(name string, b []byte) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, b, 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	script := filepath.Join(dir, "two-phase-helper")
	body := fmt.Sprintf("#!/bin/sh\ncat %q\n"+
		"i=0; while [ $i -lt %d ]; do cat %q; sleep %s; i=$((i+1)); done\n"+
		"i=0; while [ $i -lt %d ]; do cat %q; sleep %s; i=$((i+1)); done\n",
		w("info.bin", info), firstN, w("first.bin", first), delay,
		secondN, w("second.bin", second), delay)
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return script
}

// pcm16At fills n samples at one amplitude, so a packet can be made
// deliberately loud or deliberately quiet.
func pcm16At(n int, amp int16) []byte {
	b := make([]byte, n*2)
	for i := 0; i < n; i++ {
		v := amp
		if i%2 == 1 {
			v = -amp
		}
		binary.LittleEndian.PutUint16(b[i*2:], uint16(v))
	}
	return b
}

// Auto-stop needs EVERY track quiet, and this asserts the distinction rather
// than one side of it.
//
// A quiet microphone while the far end talks is somebody listening. A quiet
// system track while the microphone carries speech is an ordinary meeting with
// nothing being played. Only both together mean the room has gone — and a check
// that fired on either alone would stop a meeting that was still happening,
// which is the expensive direction here.
func TestAutoStopNeedsBothTracksQuiet(t *testing.T) {
	const loud, hush = 12000, 20 // ~-8 dBFS and ~-64 dBFS

	run := func(t *testing.T, micAmp, sysAmp int16) bool {
		t.Helper()
		info := buildFrame(frame.TypeTrackInfo, frame.TrackMic, 0, 0, 0,
			trackInfoPayload(48000, 1, 16, 1, 2, "Mic"))
		info = append(info, buildFrame(frame.TypeTrackInfo, frame.TrackSystem, 0, 0, 0,
			trackInfoPayload(48000, 1, 16, 1, 2, "Speakers"))...)
		packet := buildFrame(frame.TypeAudio, frame.TrackMic, 1_000_000, 0, 0, pcm16At(240, micAmp))
		packet = append(packet, buildFrame(frame.TypeAudio, frame.TrackSystem, 1_000_000, 0, 0, pcm16At(240, sysAmp))...)

		var mu sync.Mutex
		fired := false
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		err := Run(ctx, Options{
			Helper:               streamingHelper(t, info, packet, 20, "0.05"),
			Manifest:             newManifest(t),
			SilenceAfter:         150 * time.Millisecond,
			SilenceThresholdDBFS: -45,
			OnSilence: func(time.Duration) {
				mu.Lock()
				defer mu.Unlock()
				fired = true
			},
		})
		if err != nil {
			t.Fatalf("capture failed: %v", err)
		}
		mu.Lock()
		defer mu.Unlock()
		return fired
	}

	for _, tc := range []struct {
		name           string
		mic, sys       int16
		wantAutoStop   bool
		whyNotStopping string
	}{
		{"both quiet", hush, hush, true, ""},
		{"microphone talking", loud, hush, false, "somebody is speaking into it"},
		{"far end talking", hush, loud, false, "the operator is listening to the far end"},
		{"both talking", loud, loud, false, "the meeting is in full flow"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := run(t, tc.mic, tc.sys)
			if got != tc.wantAutoStop {
				if tc.wantAutoStop {
					t.Errorf("auto-stop did not fire with both tracks silent — the feature does nothing")
				} else {
					t.Errorf("auto-stop fired while %s: it would have cut a live meeting", tc.whyNotStopping)
				}
			}
		})
	}
}

// A track that has delivered nothing has established nothing, so it must not be
// counted as quiet — one dead endpoint would otherwise stop a meeting the other
// track is still recording perfectly well.
func TestAutoStopDoesNotFireWhileATrackHasDeliveredNothing(t *testing.T) {
	var out []byte
	out = append(out, buildFrame(frame.TypeTrackInfo, frame.TrackMic, 0, 0, 0,
		trackInfoPayload(48000, 1, 16, 1, 2, "Mic"))...)
	out = append(out, buildFrame(frame.TypeTrackInfo, frame.TrackSystem, 0, 0, 0,
		trackInfoPayload(48000, 1, 16, 1, 2, "Speakers"))...)
	// The system endpoint never opens. Only the microphone delivers, quietly.
	for i := 0; i < 4; i++ {
		out = append(out, buildFrame(frame.TypeAudio, frame.TrackMic,
			uint64(i+1)*1_000_000, 0, 0, pcm16At(240, 20))...)
	}

	var mu sync.Mutex
	fired := false
	var logged []string
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := Run(ctx, Options{
		Helper:       liveHelper(t, out, "1"),
		Manifest:     newManifest(t),
		SilenceAfter: 150 * time.Millisecond,
		Log: func(f string, a ...any) {
			mu.Lock()
			defer mu.Unlock()
			logged = append(logged, fmt.Sprintf(f, a...))
		},
		OnSilence: func(time.Duration) {
			mu.Lock()
			defer mu.Unlock()
			fired = true
		},
	})
	if err != nil {
		t.Fatalf("capture failed: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if fired {
		t.Error("auto-stop fired while a track had delivered nothing at all — " +
			"a dead endpoint would stop a meeting the other track is still recording")
	}
	// And it must SAY it is not armed. An auto-stop that silently never fires
	// is indistinguishable from one that is working and simply has not yet.
	var said bool
	for _, l := range logged {
		if strings.Contains(l, "auto-stop cannot fire yet") {
			said = true
		}
		// And it must not promise anything about the future. A track that
		// starts late re-arms the check, so "the recording will run until it is
		// stopped" was a sentence this loop could not keep — minutes-mac
		// watched one recording print it and then auto-stop anyway.
		if strings.Contains(l, "will run until") {
			t.Errorf("the disarm notice promises the recording will not stop: %q", l)
		}
	}
	if !said {
		t.Errorf("nothing said auto-stop could not fire:\n%s", strings.Join(logged, "\n"))
	}
}

// A track that starts late re-arms the check, and the retraction has to be
// said where the disarm was said.
//
// Observed on the Mac: one recording logged that auto-stop could not fire,
// then the system track began delivering, then it auto-stopped. The behaviour
// was right; the operator had been told otherwise and nothing took it back.
func TestTheDisarmNoticeIsRetractedWhenTheTrackArrives(t *testing.T) {
	info := buildFrame(frame.TypeTrackInfo, frame.TrackMic, 0, 0, 0,
		trackInfoPayload(48000, 1, 16, 1, 2, "Mic"))
	info = append(info, buildFrame(frame.TypeTrackInfo, frame.TrackSystem, 0, 0, 0,
		trackInfoPayload(48000, 1, 16, 1, 2, "Speakers"))...)

	// First the microphone alone — the system endpoint has not opened yet.
	micOnly := buildFrame(frame.TypeAudio, frame.TrackMic, 1_000_000, 0, 0, pcm16At(240, 20))
	// Then both, quietly, which is what should fire the check.
	both := append(append([]byte{}, micOnly...),
		buildFrame(frame.TypeAudio, frame.TrackSystem, 1_000_000, 0, 0, pcm16At(240, 20))...)

	var mu sync.Mutex
	var logged []string
	fired := false
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	err := Run(ctx, Options{
		Helper:       twoPhaseHelper(t, info, micOnly, 6, both, 12, "0.05"),
		Manifest:     newManifest(t),
		SilenceAfter: 150 * time.Millisecond,
		Log: func(f string, a ...any) {
			mu.Lock()
			defer mu.Unlock()
			logged = append(logged, fmt.Sprintf(f, a...))
		},
		OnSilence: func(time.Duration) {
			mu.Lock()
			defer mu.Unlock()
			fired = true
		},
	})
	if err != nil {
		t.Fatalf("capture failed: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	all := strings.Join(logged, "\n")
	if !strings.Contains(all, "cannot fire yet") {
		t.Fatalf("never said it could not fire while the system track was absent:\n%s", all)
	}
	if !strings.Contains(all, "armed now") {
		t.Errorf("the disarm notice was never retracted once the track arrived — "+
			"the operator has been told the recording will not auto-stop, and it does:\n%s", all)
	}
	if !fired {
		t.Errorf("auto-stop never fired after both tracks were delivering quietly:\n%s", all)
	}
}
