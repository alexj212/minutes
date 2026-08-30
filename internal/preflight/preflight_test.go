package preflight

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alexj212/minutes/internal/frame"
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
		want probeVerdict
	}{
		{"varying audio is a working device", live, probeLive},
		{"all zeros is denied or muted", constant, probeConstant},
		{"pinned at a non-zero value is a dead cable", pinned, probeConstant},
		{"declared and never written to is not silence", nothing, probeNoPackets},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := probeTrack(context.Background(), probeHelper(t, tc.out), frame.TrackMic)
			if got != tc.want {
				t.Errorf("probeTrack = %v, want %v", got, tc.want)
			}
		})
	}

	// A helper that says nothing at all cannot be distinguished from a probe
	// that failed to run, so it must not refuse. This check establishes
	// "definitely broken" and never "definitely fine".
	if got := probeTrack(context.Background(), probeHelper(t, nil), frame.TrackMic); got != probeUnknown {
		t.Errorf("a silent helper gave %v, want probeUnknown — refusing on this would refuse "+
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

	if got := probeTrack(context.Background(), script, frame.TrackMic); got != probeLive {
		t.Errorf("probeTrack = %v, want probeLive — the helper only emits audio while "+
			"its stdin is open, so this means the probe closed it and the check is inert", got)
	}
}

// `ok` was being asked to carry two meanings on the system track and only one
// of them was ever checked: the device opened. Whether audio came out of it was
// decided entirely by whether something happened to be playing.
//
// Found by minutes-mac running the same probe twice with only a tone playing
// between the runs. Both exited zero, both produced `system ok`, and one had
// captured nothing at all.
//
// Asserted as a pair rather than an example, because a single case passes a
// renderer that has stopped telling the two apart.
func TestOkSaysWhichOfItsTwoMeaningsWasEstablished(t *testing.T) {
	base := TrackStatus{OK: true, Mode: "wasapi-loopback", Device: "Speakers",
		SampleRate: 44100, Channels: 2, BitsPerSample: 32}

	carrying := base
	carrying.Signal = SignalCarrying
	none := base
	none.Signal = SignalNone

	a := (&Result{Platform: "windows", CanRecord: true, Mic: carrying, System: carrying}).Describe()
	b := (&Result{Platform: "windows", CanRecord: true, Mic: carrying, System: none}).Describe()

	if a == b {
		t.Fatal("a system track carrying audio and one that delivered none render " +
			"identically — this is the defect, not a cosmetic difference")
	}
	if !strings.Contains(b, "no audio was observed") && !strings.Contains(b, "no audio observed") {
		t.Errorf("a track that delivered nothing does not say so:\n%s", b)
	}
	// It must not read as a fault. The endpoint is idle before every meeting,
	// so an operator told to go and fix this would be sent after nothing.
	if strings.Contains(b, "FAIL") || !strings.Contains(b, "Both tracks can be captured.") {
		t.Errorf("an idle system track is being reported as a failure:\n%s", b)
	}
	// And it must say what settles it, or the honest label is just a shrug.
	if !strings.Contains(b, "Play something") {
		t.Errorf("says the state is unresolved without saying what resolves it:\n%s", b)
	}
}

// A probe that could not answer must not claim a measured "nothing".
//
// This is the empty-versus-unknown rule at the point of measurement: the zero
// value of Signal is "unknown", so a status nobody probed never reads as one
// that was probed and came back empty.
func TestAnUnprobedTrackDoesNotClaimToHaveBeenProbed(t *testing.T) {
	var zero Signal
	if zero != SignalUnknown {
		t.Fatalf("the zero value is %q, want %q — an unprobed track would inherit "+
			"whatever this is and assert it", zero, SignalUnknown)
	}
	for _, tc := range []struct {
		v    probeVerdict
		want Signal
	}{
		{probeLive, SignalCarrying},
		{probeConstant, SignalConstant},
		{probeNoPackets, SignalNone},
		{probeUnknown, SignalUnknown},
	} {
		if got := signalOf(tc.v); got != tc.want {
			t.Errorf("signalOf(%v) = %q, want %q", tc.v, got, tc.want)
		}
	}
	// Never omitempty: an absent field would be read as a measured nothing.
	b, err := json.Marshal(TrackStatus{OK: true})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"signal"`) {
		t.Errorf("signal is omitted when unknown: %s\n"+
			"an absent field is indistinguishable from a measured result", b)
	}
}

// dispatchHelper answers --preflight with a report and each probe with frames,
// so a whole Run() can be exercised against a machine whose two tracks behave
// differently from each other.
func dispatchHelper(t *testing.T, report string, mic, system []byte) string {
	t.Helper()
	dir := t.TempDir()
	write := func(name string, b []byte) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, b, 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	body := "#!/bin/sh\ncase \"$*\" in\n" +
		"  *--mic-only*)    cat " + write("mic.bin", mic) + " ;;\n" +
		"  *--system-only*) cat " + write("sys.bin", system) + " ;;\n" +
		"  *)               cat " + write("report.json", []byte(report)) + " ;;\n" +
		"esac\n"
	p := filepath.Join(dir, "dispatch-helper")
	if err := os.WriteFile(p, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// The same measurement, the opposite policy, and the difference is deliberate.
//
// A microphone that delivers nothing is a refusal: recording anyway produces a
// meeting with the operator missing from it. A system track that delivers
// nothing is NOT, because the render endpoint is idle before every meeting —
// this repo documents that — so refusing there would block valid recordings to
// prevent a problem that usually is not one.
//
// Worth a test rather than a comment: the obvious next change to this file is
// to notice the microphone has a probe, give the system track the same one, and
// wire it to the same refusal.
func TestAnIdleSystemTrackIsReportedAndNeverRefused(t *testing.T) {
	if !IsWSL() || !InteropEnabled() {
		t.Skip("Run()'s helper path under test only applies on a WSL host with interop")
	}
	const report = `{
  "platform": "windows",
  "tracks": {
    "microphone": {"ok": true, "mode": "wasapi-capture", "device": "Mic", "sampleRate": 48000, "channels": 1, "bitsPerSample": 16, "formatTag": 1},
    "system": {"ok": true, "mode": "wasapi-loopback", "device": "Speakers", "sampleRate": 44100, "channels": 2, "bitsPerSample": 32, "formatTag": 3}
  },
  "ok": true
}`
	const (
		typTrackInfo = 1
		typAudio     = 2
	)
	micInfo := frameBytes(typTrackInfo, 0, micTrackInfo())
	liveMic := append(append([]byte{}, micInfo...), frameBytes(typAudio, 0, pcm(3, -2, 5, -1, 4))...)
	sysInfo := frameBytes(typTrackInfo, 1, micTrackInfo())

	// The system track declares itself and never writes: nothing is playing.
	t.Setenv("MINUTES_HELPER", dispatchHelper(t, report, liveMic, sysInfo))
	res, err := Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !res.CanRecord {
		t.Fatalf("refused a recording because nothing was playing yet:\n%s", res.Refusal)
	}
	if res.System.Signal != SignalNone {
		t.Errorf("System.Signal = %q, want %q — the probe ran and saw no audio, "+
			"and that has to be recorded even though it is not a fault",
			res.System.Signal, SignalNone)
	}
	if res.Mic.Signal != SignalCarrying {
		t.Errorf("Mic.Signal = %q, want %q", res.Mic.Signal, SignalCarrying)
	}

	// And with something playing, the same call establishes the stronger claim.
	liveSys := append(append([]byte{}, sysInfo...), frameBytes(typAudio, 1, pcm(7, -3, 9, 2))...)
	t.Setenv("MINUTES_HELPER", dispatchHelper(t, report, liveMic, liveSys))
	res, err = Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.System.Signal != SignalCarrying {
		t.Errorf("System.Signal = %q with audio flowing, want %q", res.System.Signal, SignalCarrying)
	}
}

// A refusal has to say what still works, and the first version of this could
// not: both microphone refusals returned before the system track was ever
// probed, so `carrying signal` was unreachable in exactly the case that makes
// it valuable.
//
// Found by minutes-mac, whose microphone is denied. From a machine with a
// working one the branch looks covered, which is the whole reason it shipped.
func TestARefusedMicrophoneStillReportsTheFarEnd(t *testing.T) {
	if !IsWSL() || !InteropEnabled() {
		t.Skip("Run()'s helper path under test only applies on a WSL host with interop")
	}
	const report = `{
  "platform": "windows",
  "tracks": {
    "microphone": {"ok": true, "mode": "wasapi-capture", "device": "Mic", "sampleRate": 48000, "channels": 1, "bitsPerSample": 16, "formatTag": 1},
    "system": {"ok": true, "mode": "wasapi-loopback", "device": "Speakers", "sampleRate": 44100, "channels": 2, "bitsPerSample": 32, "formatTag": 3}
  },
  "ok": true
}`
	const (
		typTrackInfo = 1
		typAudio     = 2
	)
	deadMic := append(append([]byte{}, frameBytes(typTrackInfo, 0, micTrackInfo())...),
		frameBytes(typAudio, 0, pcm(0, 0, 0, 0, 0))...)
	liveSys := append(append([]byte{}, frameBytes(typTrackInfo, 1, micTrackInfo())...),
		frameBytes(typAudio, 1, pcm(7, -3, 9, 2))...)

	t.Setenv("MINUTES_HELPER", dispatchHelper(t, report, deadMic, liveSys))
	res, err := Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.CanRecord {
		t.Fatal("a constant microphone was allowed to record")
	}
	// The assertion that fails if the probes go back the other way round.
	if res.System.Signal != SignalCarrying {
		t.Errorf("System.Signal = %q on a mic refusal, want %q — the far end was "+
			"never probed, so the operator learns nothing about what still works "+
			"at the moment they most need to know", res.System.Signal, SignalCarrying)
	}
	if !strings.Contains(res.Refusal, "carrying audio") {
		t.Errorf("the refusal does not say the far end works:\n%s", res.Refusal)
	}
}

// The remedy used to be asserted, and on at least one machine it was wrong.
//
// macOS can refuse to *ask* for the microphone — a responsible process with the
// hardened runtime and no com.apple.security.device.audio-input entitlement
// raises no dialog, so the Microphone pane has nothing behind its toggle.
// Nothing in the capture path separates that from an answered "no": both arrive
// as a constant signal. So the text must enumerate rather than pick.
func TestMicAdviceDoesNotAssertARemedyItCannotVerify(t *testing.T) {
	got := micAdvice("macos", SignalCarrying)
	for _, want := range []string{
		"Privacy & Security > Microphone", // the common case, still first
		"changes nothing",                 // and the case where that pane is a dead end
		"hardened runtime",
		"com.apple.TCC", // the command that actually tells them apart
	} {
		if !strings.Contains(got, want) {
			t.Errorf("advice is missing %q — it names one cause confidently:\n%s", want, got)
		}
	}
	// Not on Windows, where none of it applies.
	if win := micAdvice("windows", SignalCarrying); strings.Contains(win, "System Settings") {
		t.Errorf("macOS advice is printed on Windows:\n%s", win)
	}
	// And the far end's state is carried into it, differently for each answer.
	a, b := micAdvice("windows", SignalCarrying), micAdvice("windows", SignalNone)
	if a == b {
		t.Error("a far end that was probed and carries audio reads the same as one " +
			"where nothing was playing — the operator cannot tell what still works")
	}
}
