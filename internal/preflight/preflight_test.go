package preflight

import (
	"context"
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
