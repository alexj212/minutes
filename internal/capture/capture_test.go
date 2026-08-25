package capture

import (
	"context"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alexj/minutes/internal/frame"
	"github.com/alexj/minutes/internal/manifest"
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
