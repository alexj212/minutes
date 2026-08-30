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

	"github.com/alexj212/minutes/internal/frame"
	"github.com/alexj212/minutes/internal/wav"
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
		// The two failure modes are kept apart. The operator's action is the
		// same — a permission or a cable — but the evidence differs, and
		// merging them loses the ability to say which was seen.
		const advice = "\n\n  On macOS check System Settings > Privacy & Security > Microphone.\n" +
			"  A denied microphone there is not an error the recorder can see: it opens,\n" +
			"  it starts, and every call returns success.\n\n" +
			"  Recording now would capture the far end and none of you."
		switch probeMicrophone(ctx, helper) {
		case micConstant:
			res.CanRecord = false
			res.Mic.OK = false
			res.Mic.Error = "delivered an unvarying signal"
			res.Refusal = "Refusing to record: the microphone opens and delivers a constant signal.\n\n" +
				"  Every sample is identical, which a working microphone never produces — a\n" +
				"  quiet room still has a noise floor. This is denied permission, a mute\n" +
				"  switch, or a dead cable. It is not a quiet room." + advice
			return res, nil
		case micNoPackets:
			res.CanRecord = false
			res.Mic.OK = false
			res.Mic.Error = "declared a track and delivered no audio at all"
			res.Refusal = "Refusing to record: the microphone declared its format and then\n" +
				"  delivered nothing.\n\n" +
				"  Not silence — no packets at all. A track declared and never written to is\n" +
				"  the same thing as no track, and a working microphone here delivers its\n" +
				"  first packet well inside the probe window, so this is not one that was\n" +
				"  slow to start." + advice
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
// thousands of consecutive identical ones.
//
// It also has to be long enough that "no packets" means broken rather than
// slow. Measured on the target Mac: a working microphone produced its first
// packet, 75 audio frames and 153 KB, well inside this window. So a device that
// delivers nothing here is not one that was still warming up.
const probeMillis = 700

// micVerdict is what a short live capture says about the microphone.
//
// Three states and not two, which is the distinction this project has now got
// wrong in five places. "Delivered nothing" is not a milder "delivered
// silence": a track that declares its format and then never writes to it is
// arguably the stronger signal, because a constant signal at least requires
// arguing that rooms are not constant.
//
// The sentence that settles it is already in this codebase, about the far end:
// *a track declared and never written to is the same thing as no track*. It was
// only ever asked about the far end.
type micVerdict int

const (
	// micUnknown means the probe could not answer. Never a refusal: this check
	// can establish "definitely broken" and never "definitely fine".
	micUnknown micVerdict = iota
	// micLive means the samples vary, which is what a capture path does.
	micLive
	// micConstant means every sample is identical — denied, muted, or a dead
	// cable. Not a quiet room: a real room is not constant.
	micConstant
	// micNoPackets means the track was declared and nothing was ever written to
	// it.
	//
	// **Real, and currently unwitnessed on darwin — which is not the same as
	// impossible.** It was briefly believed to be a second failure mode of a
	// denied macOS microphone; that was two people independently tripping the
	// stdin contract from different directions and reading the symptom as a
	// platform behaviour. A denied microphone delivers zeros.
	//
	// The confirmed instance is on Windows and is the reason this verdict
	// exists at all: a real 44-minute standup where the helper wrote "track mic
	// ended after 0 audio frames" and nothing consumed it. A track that
	// declares a format and never writes to it is a different thing from one
	// that writes zeros, whether or not the machine in front of you has managed
	// it yet.
	micNoPackets
)

// probeMicrophone runs a short capture and reports what came out.
//
// Every check before this one asks the device whether it will open. On macOS a
// *denied* microphone opens: the audio unit starts, the stream format reads
// back, every call returns success. Preflight passed, a real meeting recorded,
// and the operator's side was empty — found by minutes-mac with the operator sitting at
// the machine, with "access to kTCCServiceMicrophone denied" in the system log
// and nothing in the capture path able to see it.
//
// Microphone only, deliberately. A loopback track legitimately delivers nothing
// while the render endpoint is idle, which it is before every meeting — there,
// silence is the ordinary case rather than the alarming one.
func probeMicrophone(ctx context.Context, helper string) micVerdict {
	probeCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()

	cmd := exec.CommandContext(probeCtx, helper,
		"--duration-ms", strconv.Itoa(probeMillis), "--mic-only")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return micUnknown
	}
	// stdin must stay open for the whole probe, and forgetting that made this
	// check inert on both platforms.
	//
	// The helper stops on stdin EOF *or* the duration, whichever comes first —
	// that is the contract that lets a recording end cleanly. os/exec gives a
	// command with no Stdin an already-closed /dev/null, so the helper saw EOF
	// immediately, emitted TRACK_INFO, and exited before capturing anything.
	// Measured: 117 bytes with no stdin, 182101 bytes with it held open.
	//
	// It shipped that way and passed, because "no packets" was being read as
	// "nothing to report". A check that cannot fire looks exactly like a check
	// that passes.
	r, w, err := os.Pipe()
	if err != nil {
		return micUnknown
	}
	cmd.Stdin = r
	if err := cmd.Start(); err != nil {
		r.Close()
		w.Close()
		return micUnknown
	}
	r.Close()
	defer func() {
		w.Close()
		_ = cmd.Wait()
	}()

	reader := frame.NewReader(bufio.NewReaderSize(stdout, 1<<16))
	var info frame.TrackInfo
	var lo, hi int16
	var haveInfo, seen, sawAudio bool
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
		// Any variation at all means a live capture path, and there is no
		// reason to keep recording once that is established.
		if seen && lo != hi {
			return micLive
		}
	}

	switch {
	case sawAudio:
		return micConstant
	case haveInfo:
		// Declared a track and never wrote to it.
		return micNoPackets
	}
	// The helper said the microphone was fine and then did not even declare it.
	// That is a fault, but it is indistinguishable from the probe failing to
	// run, so it is not grounds to refuse.
	return micUnknown
}
