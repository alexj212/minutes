// Package preflight answers one question before a recording starts: would a
// recording started now capture both sides of the meeting?
//
// It exists because the alternative failure is the expensive one. A recorder
// that captures your microphone and silence produces a file of the right
// length, with a waveform in it, that plays — and the missing half is not
// discovered until someone tries to read the notes, by which time the meeting
// is over and unrepeatable. Refusing up front costs a moment; recording half a
// conversation costs the conversation.
package preflight

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// TrackStatus is what the platform reported about one track.
type TrackStatus struct {
	OK            bool   `json:"ok"`
	Mode          string `json:"mode"`
	Device        string `json:"device,omitempty"`
	SampleRate    int    `json:"sampleRate,omitempty"`
	Channels      int    `json:"channels,omitempty"`
	BitsPerSample int    `json:"bitsPerSample,omitempty"`
	FormatTag     int    `json:"formatTag,omitempty"`
	Error         string `json:"error,omitempty"`
	HResult       string `json:"hresult,omitempty"`
}

// Result is the verdict.
type Result struct {
	Platform   string      `json:"platform"`
	HelperPath string      `json:"helperPath,omitempty"`
	Mic        TrackStatus `json:"microphone"`
	System     TrackStatus `json:"system"`
	CanRecord  bool        `json:"canRecord"`
	// Refusal explains why recording must not start. It is written for whoever
	// is about to lose a meeting, so it says what is wrong and what to do.
	Refusal string `json:"refusal,omitempty"`
}

// helperReport is the shape the Windows helper prints for --preflight.
type helperReport struct {
	Platform string `json:"platform"`
	Tracks   struct {
		Microphone TrackStatus `json:"microphone"`
		System     TrackStatus `json:"system"`
	} `json:"tracks"`
	OK bool `json:"ok"`
}

// IsWSL reports whether this is a WSL kernel.
//
// WSL_DISTRO_NAME and WSL_INTEROP are not reliable: they are absent from
// environments that did not inherit a login shell, which includes every process
// a daemon starts. The kernel release string is always there.
func IsWSL() bool {
	b, err := os.ReadFile("/proc/sys/kernel/osrelease")
	if err != nil {
		return false
	}
	s := strings.ToLower(string(b))
	return strings.Contains(s, "microsoft") || strings.Contains(s, "wsl")
}

// InteropEnabled reports whether this kernel will execute Windows binaries.
func InteropEnabled() bool {
	b, err := os.ReadFile("/proc/sys/fs/binfmt_misc/WSLInterop")
	if err != nil {
		// Newer builds register the handler under a different name; fall back
		// to asking whether anything can run a PE.
		if _, err := os.Stat("/proc/sys/fs/binfmt_misc/WSLInterop-late"); err == nil {
			return true
		}
		return false
	}
	return strings.Contains(string(b), "enabled")
}

// FindHelper locates the Windows capture helper.
func FindHelper() (string, error) {
	if p := os.Getenv("MINUTES_HELPER"); p != "" {
		if _, err := os.Stat(p); err != nil {
			return "", fmt.Errorf("MINUTES_HELPER is set to %s but it is not there: %w", p, err)
		}
		return p, nil
	}

	var candidates []string
	if exe, err := os.Executable(); err == nil {
		dir := filepath.Dir(exe)
		candidates = append(candidates,
			filepath.Join(dir, "minutes-capture.exe"),
			filepath.Join(dir, "dist", "minutes-capture.exe"),
			filepath.Join(dir, "..", "dist", "minutes-capture.exe"),
		)
	}
	if wd, err := os.Getwd(); err == nil {
		candidates = append(candidates, filepath.Join(wd, "dist", "minutes-capture.exe"))
	}
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			return filepath.Abs(c)
		}
	}
	return "", errors.New("capture helper not found; build it with `make helper`")
}

// pulseSourcesLookLikeWSL reports whether the only PulseAudio devices present
// are the WSL RDP pair, and returns what it saw.
//
// This is the trap the refusal is about: RDPSink.monitor exists, opens, and
// records — but it carries only audio from Linux applications running inside
// WSL. A meeting in Teams, Zoom or a browser is a Windows process and never
// touches it.
func pulseSourcesLookLikeWSL(ctx context.Context) (bool, string) {
	cmd := exec.CommandContext(ctx, "pactl", "list", "short", "sources")
	out, err := cmd.Output()
	if err != nil {
		return false, ""
	}
	text := strings.TrimSpace(string(out))
	if text == "" {
		return false, ""
	}
	onlyRDP := true
	for _, line := range strings.Split(text, "\n") {
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		if !strings.HasPrefix(f[1], "RDPSink") && !strings.HasPrefix(f[1], "RDPSource") {
			onlyRDP = false
		}
	}
	return onlyRDP, text
}

// Run performs the checks and returns a verdict.
func Run(ctx context.Context) (*Result, error) {
	if !IsWSL() {
		return &Result{
			Platform:  "linux",
			CanRecord: false,
			Refusal: "This is a native Linux host, and R1 implements Windows capture only.\n" +
				"Capturing here needs the PulseAudio path (a source for the microphone,\n" +
				"the sink's .monitor for system audio), which is not built yet.",
		}, nil
	}

	res := &Result{Platform: "wsl"}

	if !InteropEnabled() {
		onlyRDP, sources := pulseSourcesLookLikeWSL(ctx)
		res.CanRecord = false
		res.Refusal = "Refusing to record: this is WSL, and Windows interop is disabled, so\n" +
			"the Windows capture helper cannot be started.\n\n" +
			"There is a PulseAudio device here that looks like it would work, and it\n" +
			"would not. RDPSink.monitor carries audio only from Linux applications\n" +
			"running inside WSL. A meeting in Teams, Zoom or a browser is a Windows\n" +
			"process, so it never reaches that monitor: the recording would contain\n" +
			"your microphone and silence, and look entirely successful.\n\n" +
			"Enable interop (WSL_INTEROP / binfmt_misc) so the helper can run."
		if onlyRDP {
			res.Refusal += "\n\nPulseAudio sources visible here:\n  " +
				strings.ReplaceAll(sources, "\n", "\n  ")
		}
		return res, nil
	}

	helper, err := FindHelper()
	if err != nil {
		res.CanRecord = false
		res.Refusal = "Refusing to record: " + err.Error() + ".\n" +
			"Windows interop works, so capture is possible here once the helper is built."
		return res, nil
	}
	res.HelperPath = helper

	// Ask the platform rather than assuming it. A device that enumerates but
	// refuses to start is exactly what this is here to catch.
	probeCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	out, runErr := exec.CommandContext(probeCtx, helper, "--preflight").Output()
	if len(out) == 0 {
		res.CanRecord = false
		res.Refusal = fmt.Sprintf("Refusing to record: the capture helper produced no report (%v).", runErr)
		return res, nil
	}

	var rep helperReport
	if err := json.Unmarshal(out, &rep); err != nil {
		res.CanRecord = false
		res.Refusal = fmt.Sprintf("Refusing to record: could not read the helper's report: %v", err)
		return res, nil
	}

	res.Mic = rep.Tracks.Microphone
	res.System = rep.Tracks.System
	res.CanRecord = rep.Tracks.Microphone.OK && rep.Tracks.System.OK

	if !res.CanRecord {
		var b strings.Builder
		b.WriteString("Refusing to record: one side of the meeting cannot be captured.\n\n")
		if !rep.Tracks.Microphone.OK {
			fmt.Fprintf(&b, "  microphone: %s (%s)\n", rep.Tracks.Microphone.Error, rep.Tracks.Microphone.HResult)
			b.WriteString("    Nothing you say would be recorded.\n")
		}
		if !rep.Tracks.System.OK {
			fmt.Fprintf(&b, "  system: %s (%s)\n", rep.Tracks.System.Error, rep.Tracks.System.HResult)
			b.WriteString("    Nothing anyone else says would be recorded — the file would\n" +
				"    contain your voice and silence, and would look successful.\n")
		}
		b.WriteString("\nCheck that the endpoint exists and is enabled in Windows sound settings.")
		res.Refusal = b.String()
	}
	return res, nil
}

// Describe renders a result for a person.
func (r *Result) Describe() string {
	var b strings.Builder
	fmt.Fprintf(&b, "platform: %s\n", r.Platform)
	if r.HelperPath != "" {
		fmt.Fprintf(&b, "helper:   %s\n", r.HelperPath)
	}
	describe := func(label string, t TrackStatus) {
		if t.Mode == "" && !t.OK {
			return
		}
		if t.OK {
			fmt.Fprintf(&b, "  %-7s ok   %-16s %s  %d Hz, %d ch, %d-bit\n",
				label, t.Mode, t.Device, t.SampleRate, t.Channels, t.BitsPerSample)
		} else {
			fmt.Fprintf(&b, "  %-7s FAIL %-16s %s %s\n", label, t.Mode, t.Error, t.HResult)
		}
	}
	describe("mic", r.Mic)
	describe("system", r.System)
	if r.CanRecord {
		b.WriteString("\nBoth tracks can be captured.\n")
	} else {
		b.WriteString("\n" + r.Refusal + "\n")
	}
	return b.String()
}
