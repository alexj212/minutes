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
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/alexj/minutes/internal/frame"
	"github.com/alexj/minutes/internal/wav"
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
	// Waiting is set when the platform is blocked on a human rather than
	// broken: a consent dialog is open and nothing will proceed until somebody
	// answers it.
	//
	// A third state, not a kind of error. An error means fix the machine; a
	// wait means look at the screen. Collapsing them tells an operator "the
	// capture helper produced no report", which is true and useless — the
	// helper is sitting there waiting to be allowed to work.
	//
	// Rare rather than routine: on macOS the grant is per-machine once the
	// helper is signed. But the first one on any new machine still has to be
	// given by somebody looking at a screen, and nothing else can give it.
	Waiting string `json:"waiting,omitempty"`
}

// BlockedOnConsent reports a track that is waiting for a person.
func (t TrackStatus) BlockedOnConsent() bool { return !t.OK && t.Waiting != "" }

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

	// The Windows helper is a PE and keeps its extension even when launched
	// from Linux over interop; every other platform's is an ordinary binary.
	// Both names are tried everywhere rather than switched on the host, because
	// the host running the orchestrator is not always the host the helper was
	// built for — that is the whole point of the WSL arrangement.
	names := []string{"minutes-capture", "minutes-capture.exe"}

	var dirs []string
	if exe, err := os.Executable(); err == nil {
		dir := filepath.Dir(exe)
		dirs = append(dirs, dir, filepath.Join(dir, "dist"), filepath.Join(dir, "..", "dist"))
	}
	if wd, err := os.Getwd(); err == nil {
		dirs = append(dirs, filepath.Join(wd, "dist"))
	}
	var candidates []string
	for _, d := range dirs {
		for _, n := range names {
			candidates = append(candidates, filepath.Join(d, n))
		}
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
	// macOS runs its own helper directly — there is no interop layer to cross,
	// and no WSL trap to refuse. The helper answers the same --preflight
	// contract, so everything below it is shared.
	if runtime.GOOS == "darwin" {
		return runHelperPreflight(ctx, &Result{Platform: "macos"})
	}

	if !IsWSL() {
		return &Result{
			Platform:  "linux",
			CanRecord: false,
			Refusal: "This is a native Linux host, and capture is implemented for Windows\n" +
				"(through WSL) and macOS only. Capturing here needs the PulseAudio path —\n" +
				"a source for the microphone, the sink's .monitor for system audio — which\n" +
				"is not built.",
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

	if _, err := FindHelper(); err != nil {
		res.CanRecord = false
		res.Refusal = "Refusing to record: " + err.Error() + ".\n" +
			"Windows interop works, so capture is possible here once the helper is built."
		return res, nil
	}
	return runHelperPreflight(ctx, res)
}

// runHelperPreflight asks the platform's helper whether it could capture, and
// turns its answer into a verdict.
//
// Shared by every platform that has a helper, because the question and the
// refusal are the same everywhere: a device that enumerates but refuses to
// start is what this exists to catch, and half a meeting is half a meeting
// whatever recorded it.
func runHelperPreflight(ctx context.Context, res *Result) (*Result, error) {
	helper, err := FindHelper()
	if err != nil {
		res.CanRecord = false
		res.Refusal = "Refusing to record: " + err.Error() + "."
		return res, nil
	}
	res.HelperPath = helper

	// The helper bounds each of its own setup calls well inside this, so that a
	// call blocked on consent returns a "waiting" report rather than being
	// killed here and surfacing as "produced no report". The two numbers are
	// coupled: shortening this without telling the helper authors turns a
	// legible wait back into a silent timeout.
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

	// Waiting for a person is not a fault, and telling somebody to fix their
	// machine when the machine is asking them a question wastes the one action
	// that would actually work.
	if !res.CanRecord && (res.Mic.BlockedOnConsent() || res.System.BlockedOnConsent()) {
		var b strings.Builder
		b.WriteString("Not recording yet: this machine is waiting for your permission.\n\n")
		for _, t := range []struct {
			label string
			st    TrackStatus
		}{{"microphone", res.Mic}, {"system audio", res.System}} {
			if t.st.BlockedOnConsent() {
				fmt.Fprintf(&b, "  %s: %s\n", t.label, t.st.Waiting)
			}
		}
		b.WriteString("\nAnswer the dialog, then run this again. Nothing here is broken and\n")
		b.WriteString("there is nothing to fix — the helper is waiting to be allowed to work.\n")
		b.WriteString("Worth doing before the meeting rather than at it.")
		res.Refusal = b.String()
		return res, nil
	}

	// Everything above asked the device whether it would open. This asks
	// whether anything comes out of it.
	if res.CanRecord {
		if silent, err := microphoneIsDigitallySilent(ctx, helper); err == nil && silent {
			res.CanRecord = false
			res.Mic.OK = false
			res.Mic.Error = "delivered nothing but exact digital silence"
			res.Refusal = "Refusing to record: the microphone opens and delivers silence.\n\n" +
				"  Every sample is exactly zero, which a working microphone never produces —\n" +
				"  a quiet room still has a noise floor. This is denied permission, a hardware\n" +
				"  mute switch, or a disconnected device, and it is not a quiet room.\n\n" +
				"  On macOS check System Settings > Privacy & Security > Microphone. A denied\n" +
				"  microphone there is not an error the recorder can see: it opens, it starts,\n" +
				"  and it hands over zeros.\n\n" +
				"  Recording now would capture the far end and none of you."
			return res, nil
		}
	}

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

// StorageBytesPerSecond is how fast a recording will consume disk.
//
// Capture is in the endpoint's mix format but storage is 16-bit PCM, so the
// rate on disk is two bytes per sample per channel regardless of what the
// device hands over.
func (r *Result) StorageBytesPerSecond() int {
	total := 0
	for _, t := range []TrackStatus{r.Mic, r.System} {
		if t.OK {
			total += t.SampleRate * t.Channels * 2
		}
	}
	return total
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
		switch {
		case t.OK:
			fmt.Fprintf(&b, "  %-7s ok   %-16s %s  %d Hz, %d ch, %d-bit\n",
				label, t.Mode, t.Device, t.SampleRate, t.Channels, t.BitsPerSample)
		case t.BlockedOnConsent():
			fmt.Fprintf(&b, "  %-7s WAIT %-16s %s\n", label, t.Mode, t.Waiting)
		default:
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

// probeMillis is how long the live-audio probe records for.
//
// Long enough for a noise floor to appear and short enough that preflight stays
// something somebody runs before a meeting rather than instead of one. At
// 48 kHz it is thousands of samples, and a working capture path cannot produce
// thousands of consecutive exact zeros.
const probeMillis = 700

// microphoneIsDigitallySilent reports whether the microphone delivers an
// unvarying signal.
//
// Every check before this one asks the device whether it will open. On macOS a
// *denied* microphone opens: the audio unit starts, the stream format reads
// back, every call returns success, and the samples are all zero. So preflight
// passed, a real meeting recorded, and the operator's side was empty — found by
// minutes-mac with Alex sitting at the machine. The system log said
// "access to kTCCServiceMicrophone denied" and nothing in the capture path
// could see it.
//
// The discriminator is categorical rather than a threshold, which is what makes
// it safe to refuse on. A real microphone in a silent room gives a mean far
// below its max, because a room is not constant — preamp noise, thermal noise,
// room tone. A loudness cutoff would refuse a genuinely quiet room and put us
// back to guessing; "every sample is identical" needs no guess.
//
// Constant rather than zero, which is the mac core session's refinement of this
// and strictly better: it also catches a device pinned at a non-zero DC offset,
// a dead cable and an interface that vanished, none of which are quiet rooms
// and all of which a zero-check would pass.
//
// Microphone only, deliberately. A loopback track legitimately delivers nothing
// while the render endpoint is idle, which it is before every meeting — there,
// silence is the ordinary case rather than the alarming one.
//
// An error is not a refusal. If the probe cannot run, preflight reports what it
// already knows rather than inventing a fault: this check can say "definitely
// broken", never "definitely fine".
func microphoneIsDigitallySilent(ctx context.Context, helper string) (bool, error) {
	probeCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()

	cmd := exec.CommandContext(probeCtx, helper,
		"--duration-ms", strconv.Itoa(probeMillis), "--mic-only")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return false, err
	}
	if err := cmd.Start(); err != nil {
		return false, err
	}
	defer func() {
		_ = cmd.Wait()
	}()

	reader := frame.NewReader(bufio.NewReaderSize(stdout, 1<<16))
	var info frame.TrackInfo
	var haveInfo bool
	var lo, hi int16
	var seen bool
	sawAudio := false
	for {
		f, err := reader.Next()
		if err != nil {
			break
		}
		if f.Type == frame.TypeTrackInfo && f.Track == frame.TrackMic {
			if ti, err := frame.ParseTrackInfo(f.Payload); err == nil {
				info, haveInfo = ti, true
			}
			continue
		}
		if f.Type != frame.TypeAudio || f.Track != frame.TrackMic || !haveInfo {
			continue
		}
		samples, err := wav.ToInt16(f.Payload, info.FormatTag, info.BitsPerSample)
		if err != nil || len(samples) == 0 {
			continue
		}
		sawAudio = true
		for _, v := range samples {
			if !seen {
				lo, hi, seen = v, v, true
				continue
			}
			if v < lo {
				lo = v
			}
			if v > hi {
				hi = v
			}
		}
		// Any variation at all means a live capture path. Checked as the frames
		// arrive rather than at the end so a working microphone stops the probe
		// early.
		if seen && lo != hi {
			return false, nil
		}
	}
	// No audio at all is not this check's finding. The helper reported the
	// track as OK and delivered no packets, which is a different fault and one
	// the recording-time watcher is built to catch.
	return sawAudio, nil
}
