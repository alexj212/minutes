package preflight

import (
	"context"
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeHelper writes an executable that prints the given report, so the refusal
// logic can be exercised against a platform that says no without needing a
// machine whose audio is actually broken.
func fakeHelper(t *testing.T, report string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "fake-helper")
	script := "#!/bin/sh\ncat <<'JSON'\n" + report + "\nJSON\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func runWithHelper(t *testing.T, report string) *Result {
	t.Helper()
	if !IsWSL() || !InteropEnabled() {
		t.Skip("refusal path under test only applies on a WSL host with interop")
	}
	t.Setenv("MINUTES_HELPER", fakeHelper(t, report))
	res, err := Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return res
}

// The failure this project exists to prevent: the microphone works, the system
// endpoint does not, and the recording would contain your voice and silence.
// Preflight must refuse rather than let that be discovered after the meeting.
func TestRefusesWhenSystemTrackUnavailable(t *testing.T) {
	res := runWithHelper(t, `{
  "platform": "windows",
  "tracks": {
    "microphone": {"ok": true, "mode": "wasapi-capture", "device": "Mic", "sampleRate": 48000, "channels": 2, "bitsPerSample": 32, "formatTag": 3},
    "system": {"ok": false, "mode": "wasapi-loopback", "error": "no default render endpoint", "hresult": "0x80070490"}
  },
  "ok": false
}`)

	if res.CanRecord {
		t.Fatal("CanRecord is true with no system track — this would record half a meeting")
	}
	if res.Refusal == "" {
		t.Fatal("refused without saying why")
	}
	// The explanation has to name the consequence, because the person reading
	// it is deciding whether to start a meeting.
	if !strings.Contains(res.Refusal, "silence") {
		t.Errorf("refusal does not explain the consequence:\n%s", res.Refusal)
	}
	if !strings.Contains(res.Refusal, "no default render endpoint") {
		t.Errorf("refusal drops the platform's own reason:\n%s", res.Refusal)
	}
}

func TestRefusesWhenMicrophoneUnavailable(t *testing.T) {
	res := runWithHelper(t, `{
  "platform": "windows",
  "tracks": {
    "microphone": {"ok": false, "mode": "wasapi-capture", "error": "no default capture endpoint", "hresult": "0x80070490"},
    "system": {"ok": true, "mode": "wasapi-loopback", "device": "Speakers", "sampleRate": 44100, "channels": 2, "bitsPerSample": 32, "formatTag": 3}
  },
  "ok": false
}`)
	if res.CanRecord {
		t.Fatal("CanRecord is true with no microphone")
	}
	if !strings.Contains(res.Refusal, "Nothing you say") {
		t.Errorf("refusal does not explain the consequence:\n%s", res.Refusal)
	}
}

func TestAllowsWhenBothTracksAvailable(t *testing.T) {
	res := runWithHelper(t, `{
  "platform": "windows",
  "tracks": {
    "microphone": {"ok": true, "mode": "wasapi-capture", "device": "Mic", "sampleRate": 48000, "channels": 2, "bitsPerSample": 32, "formatTag": 3},
    "system": {"ok": true, "mode": "wasapi-loopback", "device": "Speakers", "sampleRate": 44100, "channels": 2, "bitsPerSample": 32, "formatTag": 3}
  },
  "ok": true
}`)
	if !res.CanRecord {
		t.Fatalf("refused a working machine: %s", res.Refusal)
	}
	if res.Mic.Device != "Mic" || res.System.Device != "Speakers" {
		t.Errorf("device names did not survive: mic=%q system=%q", res.Mic.Device, res.System.Device)
	}
	if res.System.SampleRate != 44100 {
		t.Errorf("system sample rate = %d, want 44100", res.System.SampleRate)
	}
}

// A helper that produces nothing must not be read as approval.
func TestRefusesWhenHelperSaysNothing(t *testing.T) {
	res := runWithHelper(t, ``)
	if res.CanRecord {
		t.Fatal("CanRecord is true on an empty report")
	}
}

func TestRefusesWhenHelperIsMissing(t *testing.T) {
	if !IsWSL() || !InteropEnabled() {
		t.Skip("only applies on a WSL host with interop")
	}
	t.Setenv("MINUTES_HELPER", filepath.Join(t.TempDir(), "does-not-exist"))
	res, err := Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.CanRecord {
		t.Fatal("CanRecord is true with no helper")
	}
}

// Capture is in the endpoint's mix format but storage is 16-bit PCM, so the
// disk rate is two bytes per sample per channel whatever the device hands over.
// Using the capture width would overestimate by double on this machine, where
// both endpoints report 32-bit float.
func TestStorageRateUsesStorageWidthNotCaptureWidth(t *testing.T) {
	r := &Result{
		Mic:    TrackStatus{OK: true, SampleRate: 48000, Channels: 2, BitsPerSample: 32},
		System: TrackStatus{OK: true, SampleRate: 44100, Channels: 2, BitsPerSample: 32},
	}
	if got, want := r.StorageBytesPerSecond(), 48000*2*2+44100*2*2; got != want {
		t.Errorf("StorageBytesPerSecond = %d, want %d", got, want)
	}
}

// A track that cannot be captured consumes nothing.
func TestStorageRateIgnoresUnavailableTracks(t *testing.T) {
	r := &Result{
		Mic:    TrackStatus{OK: true, SampleRate: 48000, Channels: 2},
		System: TrackStatus{OK: false, SampleRate: 44100, Channels: 2},
	}
	if got, want := r.StorageBytesPerSecond(), 48000*2*2; got != want {
		t.Errorf("StorageBytesPerSecond = %d, want %d", got, want)
	}
}

// Waiting for a person is not a fault. An error means fix the machine; a wait
// means look at the screen and answer something. Collapsing them tells an
// operator "the capture helper produced no report", which is true and useless —
// the helper is sitting there waiting to be allowed to work.
func TestWaitingForConsentReadsAsAnInstructionNotAFault(t *testing.T) {
	res := runWithHelper(t, `{
  "platform": "macos",
  "tracks": {
    "microphone": {"ok": true, "mode": "input", "device": "MacBook Air Microphone", "sampleRate": 48000, "channels": 1, "bitsPerSample": 32, "formatTag": 3},
    "system": {"ok": false, "mode": "global tap", "waiting": "system audio capture is waiting for permission — look for a dialog"}
  },
  "ok": false
}`)

	if res.CanRecord {
		t.Fatal("CanRecord is true while a track is waiting for consent")
	}
	if !res.System.BlockedOnConsent() {
		t.Fatal("a waiting track was not recognised as blocked on consent")
	}
	for _, want := range []string{"waiting for your permission", "Answer the dialog", "nothing to fix"} {
		if !strings.Contains(res.Refusal, want) {
			t.Errorf("the refusal does not read as an instruction; missing %q:\n%s", want, res.Refusal)
		}
	}
	// It must not read as a broken machine.
	for _, wrong := range []string{"cannot be captured", "Check that the endpoint"} {
		if strings.Contains(res.Refusal, wrong) {
			t.Errorf("a consent wait was reported as a fault: %q appears in:\n%s", wrong, res.Refusal)
		}
	}
	if !strings.Contains(res.Describe(), "WAIT") {
		t.Errorf("the rendered status does not distinguish a wait:\n%s", res.Describe())
	}
}

// A genuine fault must still read as one, or the distinction is decorative.
func TestGenuineFaultStillReadsAsAFault(t *testing.T) {
	res := runWithHelper(t, `{
  "platform": "macos",
  "tracks": {
    "microphone": {"ok": true, "mode": "input", "device": "Mic", "sampleRate": 48000, "channels": 1},
    "system": {"ok": false, "mode": "global tap", "error": "no such device", "hresult": "0x80070490"}
  },
  "ok": false
}`)
	if res.System.BlockedOnConsent() {
		t.Error("a fault was treated as a consent wait")
	}
	if !strings.Contains(res.Refusal, "cannot be captured") {
		t.Errorf("a genuine fault no longer reads as one:\n%s", res.Refusal)
	}
	if strings.Contains(res.Refusal, "Answer the dialog") {
		t.Error("a fault was reported as something a person can answer")
	}
}

// A track that is merely not ok, with nothing said about why, is a fault and not
// a wait — absent is not the same as waiting.
func TestSilenceAboutTheReasonIsNotAWait(t *testing.T) {
	res := runWithHelper(t, `{
  "platform": "macos",
  "tracks": {
    "microphone": {"ok": true, "mode": "input", "device": "Mic"},
    "system": {"ok": false, "mode": "global tap"}
  },
  "ok": false
}`)
	if res.System.BlockedOnConsent() {
		t.Error("a track with no stated reason was treated as waiting for a person")
	}
}

// frameBytes builds one frame in the layout docs/protocol.md specifies.
func frameBytes(typ uint16, track uint16, payload []byte) []byte {
	h := make([]byte, 32)
	binary.LittleEndian.PutUint32(h[0:], 0x314E494D)
	binary.LittleEndian.PutUint16(h[4:], typ)
	binary.LittleEndian.PutUint16(h[6:], track)
	binary.LittleEndian.PutUint32(h[24:], uint32(len(payload)))
	return append(h, payload...)
}

func micTrackInfo() []byte {
	p := make([]byte, 24+3)
	binary.LittleEndian.PutUint32(p[0:], 48000) // rate
	binary.LittleEndian.PutUint16(p[4:], 1)     // channels
	binary.LittleEndian.PutUint16(p[6:], 16)    // bits
	binary.LittleEndian.PutUint16(p[8:], 1)     // formatTag: PCM
	binary.LittleEndian.PutUint16(p[10:], 2)    // blockAlign
	binary.LittleEndian.PutUint64(p[12:], 10_000_000)
	binary.LittleEndian.PutUint32(p[20:], 3)
	copy(p[24:], "mic")
	return p
}

func pcm(values ...int16) []byte {
	b := make([]byte, len(values)*2)
	for i, v := range values {
		binary.LittleEndian.PutUint16(b[i*2:], uint16(v))
	}
	return b
}

// probeHelper writes an executable that emits the given frames on stdout.
func probeHelper(t *testing.T, out []byte) string {
	t.Helper()
	dir := t.TempDir()
	data := filepath.Join(dir, "frames.bin")
	if err := os.WriteFile(data, out, 0o644); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "probe-helper")
	body := "#!/bin/sh\ncat " + data + "\n"
	if err := os.WriteFile(p, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// A denied microphone has two failure modes and they must not be merged.
//
// Asserted as a set of three, because that is the distinction this project has
// now got wrong in five places. Any two of these alone would pass a check that
// collapsed the third into "fine" — which is exactly what happened: the
// constant test shipped, and the machine it was written for had already moved
// to the mode it does not cover.
func TestTheThreeThingsAMicrophoneCanDo(t *testing.T) {
	const (
		typTrackInfo = 1
		typAudio     = 2
	)
	info := frameBytes(typTrackInfo, 0, micTrackInfo())

	live := append(append([]byte{}, info...), frameBytes(typAudio, 0, pcm(3, -2, 5, -1, 4))...)
	constant := append(append([]byte{}, info...), frameBytes(typAudio, 0, pcm(0, 0, 0, 0, 0))...)
	pinned := append(append([]byte{}, info...), frameBytes(typAudio, 0, pcm(1200, 1200, 1200))...)
	nothing := append([]byte{}, info...)

	for _, tc := range []struct {
		name string
		out  []byte
		want micVerdict
	}{
		{"varying audio is a working device", live, micLive},
		{"all zeros is denied or muted", constant, micConstant},
		{"pinned at a non-zero value is a dead cable", pinned, micConstant},
		{"declared and never written to is not silence", nothing, micNoPackets},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := probeMicrophone(context.Background(), probeHelper(t, tc.out))
			if got != tc.want {
				t.Errorf("probeMicrophone = %v, want %v", got, tc.want)
			}
		})
	}

	// A helper that says nothing at all cannot be distinguished from a probe
	// that failed to run, so it must not refuse. This check establishes
	// "definitely broken" and never "definitely fine".
	if got := probeMicrophone(context.Background(), probeHelper(t, nil)); got != micUnknown {
		t.Errorf("a silent helper gave %v, want micUnknown — refusing on this would refuse "+
			"every machine where the probe cannot run", got)
	}
}

// The probe must hold the helper's stdin open, and this is the test that would
// have caught it not doing so.
//
// The helper stops on stdin EOF *or* its duration, whichever comes first — that
// is the contract that lets a recording end cleanly. os/exec gives a command
// with no Stdin an already-closed /dev/null, so the helper saw EOF immediately,
// emitted TRACK_INFO and exited before capturing anything. Measured on the real
// Windows helper: 117 bytes with no stdin, 182101 with it held open.
//
// It shipped and published in that state, and preflight kept passing, because
// "no packets" was being read as "nothing to report". A check that cannot fire
// is indistinguishable from a check that passes — which is the whole reason
// this file's other tests assert three outcomes rather than one.
//
// The fake helper here emits audio ONLY if its stdin is still open, so the
// probe cannot pass by accident.
func TestTheProbeHoldsTheHelpersStdinOpen(t *testing.T) {
	const (
		typTrackInfo = 1
		typAudio     = 2
	)
	dir := t.TempDir()
	info := filepath.Join(dir, "info.bin")
	audio := filepath.Join(dir, "audio.bin")
	if err := os.WriteFile(info, frameBytes(typTrackInfo, 0, micTrackInfo()), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(audio, frameBytes(typAudio, 0, pcm(3, -2, 5, -1)), 0o644); err != nil {
		t.Fatal(err)
	}

	// timeout returns 124 only when cat was still waiting for input, which
	// means stdin was held open rather than already at EOF.
	script := filepath.Join(dir, "needs-stdin")
	body := "#!/bin/sh\ncat " + info + "\ntimeout 0.4 cat > /dev/null\n" +
		"if [ $? -eq 124 ]; then cat " + audio + "; fi\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}

	if got := probeMicrophone(context.Background(), script); got != micLive {
		t.Errorf("probeMicrophone = %v, want micLive — the helper only emits audio while "+
			"its stdin is open, so this means the probe closed it and the check is inert", got)
	}
}
