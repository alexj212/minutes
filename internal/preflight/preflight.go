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
	// Signal is what a live probe established about audio actually coming out
	// of this track, as distinct from the device agreeing to open.
	//
	// Never `omitempty`. An absent field here would be read as a measured
	// "nothing", and the whole point of it is that not-measured and
	// measured-nothing are different answers.
	Signal Signal `json:"signal"`
}

// Signal is what a short live capture established about a track.
//
// `ok` was being asked to carry two meanings — the device opened, and audio
// comes out of it — and for the system track only the first was ever checked.
// Found by minutes-mac, who ran the probe twice with only a tone playing
// between the runs: both exited zero, both produced `system ok`, and one of
// them had captured nothing at all.
//
// A track that opened and delivered nothing is not a track that was quiet.
// Which of those it was is a question the operator can settle in two seconds,
// so the word says which one was established and what would settle it.
// An integer rather than a string, so that the ZERO VALUE IS "unknown". A
// string Signal makes the zero value `""`, and a TrackStatus nobody probed then
// serialises an empty field — which is the very ambiguity this type exists to
// remove, reintroduced by the choice of underlying type. Caught by the test
// below, which failed on the first version of this.
type Signal int

const (
	// SignalUnknown means nothing was established: the track was not probed,
	// or the probe could not answer. Deliberately the zero value, so a status
	// nobody measured never claims to have been measured.
	SignalUnknown Signal = iota
	// SignalCarrying means audio arrived and varied. The only value that
	// establishes a working capture path end to end.
	SignalCarrying
	// SignalNone means the track declared its format and nothing was ever
	// written to it.
	//
	// A refusal on the microphone and *expected* on the system track, which is
	// idle before every meeting. Same measurement, opposite policy — see
	// probeTrack.
	SignalNone
	// SignalConstant means audio arrived and every sample was identical:
	// denied, muted, or a dead cable. Not a quiet room, which has a noise
	// floor.
	SignalConstant
)

// String is the wire form, and it names the unknown case rather than leaving it
// blank.
func (s Signal) String() string {
	switch s {
	case SignalCarrying:
		return "carrying"
	case SignalNone:
		return "none"
	case SignalConstant:
		return "constant"
	}
	return "unknown"
}

// MarshalJSON writes the name. A reader of the JSON gets the same three-way
// distinction the code has, rather than an integer they have to look up.
func (s Signal) MarshalJSON() ([]byte, error) { return json.Marshal(s.String()) }

// UnmarshalJSON accepts the name, and anything it does not recognise — an older
// file, a helper that predates this — becomes unknown rather than an error or,
// worse, a confident wrong value.
func (s *Signal) UnmarshalJSON(b []byte) error {
	var name string
	if err := json.Unmarshal(b, &name); err != nil {
		return err
	}
	switch name {
	case "carrying":
		*s = SignalCarrying
	case "none":
		*s = SignalNone
	case "constant":
		*s = SignalConstant
	default:
		*s = SignalUnknown
	}
	return nil
}

// Text is the phrase shown beside a track, and it says what was established
// rather than how it scored.
func (s Signal) Text() string {
	switch s {
	case SignalCarrying:
		return "carrying signal"
	case SignalNone:
		return "opened; no audio observed"
	case SignalConstant:
		return "opened; every sample identical"
	}
	return "opened; signal not checked"
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
		// BOTH probes run before anything decides, and the order is the point.
		//
		// The first version returned on a microphone refusal before the system
		// track was ever probed, so `carrying signal` was unreachable in
		// exactly the situation that makes it valuable: an operator whose
		// microphone is broken learned nothing at all about the far end, at the
		// moment they most want to know what still works. Found by minutes-mac,
		// who could see it because their microphone is denied and mine is not —
		// from here the branch looked covered.
		//
		// It costs about two seconds on a path that is already refusing. That
		// is the right trade: a refusal naming what does still work is a
		// different message from one that only says no.
		micV := probeTrack(ctx, helper, frame.TrackMic)
		res.Mic.Signal = signalOf(micV)
		// The same measurement on the far end, reported and never enforced.
		// `system ok` meant "the device opened" and was silent about whether
		// anything came out of it — and the two are told apart only by whether
		// something happened to be playing at the time.
		res.System.Signal = signalOf(probeTrack(ctx, helper, frame.TrackSystem))

		// The two failure modes are kept apart. The operator's action is the
		// same — a permission or a cable — but the evidence differs, and
		// merging them loses the ability to say which was seen.
		advice := micAdvice(res.Platform, res.System.Signal)
		switch micV {
		case probeConstant:
			res.CanRecord = false
			res.Mic.OK = false
			res.Mic.Error = "delivered an unvarying signal"
			res.Refusal = "Refusing to record: the microphone opens and delivers a constant signal.\n\n" +
				"  Every sample is identical, which a working microphone never produces — a\n" +
				"  quiet room still has a noise floor. This is denied permission, a mute\n" +
				"  switch, or a dead cable. It is not a quiet room." + advice
			return res, nil
		case probeNoPackets:
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
			fmt.Fprintf(&b, "  %-7s ok   %-16s %s  %d Hz, %d ch, %d-bit  — %s\n",
				label, t.Mode, t.Device, t.SampleRate, t.Channels, t.BitsPerSample,
				t.Signal.Text())
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
		// Not a warning, and phrased so it does not read as one. It is the
		// ordinary state before a meeting, and the operator can settle it in
		// two seconds — so say which way it is unresolved and what resolves it,
		// rather than either alarming them or implying it was checked.
		if r.System.Signal == SignalNone || r.System.Signal == SignalUnknown {
			b.WriteString("\nNo audio was observed on the system track. That is expected when\n" +
				"nothing is playing, and it is also what a dead capture path looks like —\n" +
				"this cannot tell them apart. Play something for a second and run this\n" +
				"again to settle it.\n")
		}
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

// probeVerdict is what a short live capture says about one track.
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
type probeVerdict int

const (
	// probeUnknown means the probe could not answer. Never a refusal: this check
	// can establish "definitely broken" and never "definitely fine".
	probeUnknown probeVerdict = iota
	// probeLive means the samples vary, which is what a capture path does.
	probeLive
	// probeConstant means every sample is identical — denied, muted, or a dead
	// cable. Not a quiet room: a real room is not constant.
	probeConstant
	// probeNoPackets means the track was declared and nothing was ever written to
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
	probeNoPackets
)

// micAdvice is what to do about a microphone that opens and captures nothing.
//
// It used to assert one remedy — open the Microphone pane and enable it — and
// on at least one machine that sends the operator to a pane where the fix does
// not exist. macOS can refuse to *ask*: when the responsible process is built
// with the hardened runtime and without `com.apple.security.device.audio-input`,
// no dialog is ever raised, so there is nothing to enable and toggling the entry
// that is there changes nothing. Measured by minutes-mac in TCC's own log.
//
// And it is reached by the fix for the *previous* problem. The log's words are
// "failed to match **existing** code requirement": a grant was already there,
// the launcher's first real signature invalidated the requirement it had been
// recorded against, so re-consent became necessary — which is precisely the
// moment a missing entitlement forbids the prompt. Signing was adopted to make
// grants durable and it is what made this one unrecoverable.
//
// Nothing in the capture path can tell the two causes apart: an answered "no"
// and a forbidden prompt both arrive as a constant signal. So this enumerates
// what it cannot distinguish and hands over the command that does distinguish
// them, rather than naming one confidently. A wrong remedy stated with
// confidence costs more than an honest list, because it is followed.
func micAdvice(platform string, farEnd Signal) string {
	var b strings.Builder
	b.WriteString("\n\n  Recording now would capture ")
	switch farEnd {
	case SignalCarrying:
		// Worth saying plainly. It is the difference between "the machine is
		// broken" and "one device is", and it is what the operator would
		// otherwise go and find out for themselves.
		b.WriteString("the far end — which was probed and is\n  carrying audio — and none of you.")
	case SignalNone:
		b.WriteString("the far end and none of you. Nothing was\n" +
			"  playing during the probe, so whether the far end works is not established.")
	default:
		b.WriteString("the far end and none of you.")
	}
	if platform != "macos" {
		return b.String()
	}
	b.WriteString("\n\n  Start at System Settings > Privacy & Security > Microphone, and enable\n" +
		"  whatever launched this — the grant belongs to the launcher, not to\n" +
		"  minutes-capture, which appears nowhere in that pane.\n\n" +
		"  If it is listed there and enabling it changes nothing, macOS is refusing\n" +
		"  to ask: a launcher with the hardened runtime and no\n" +
		"  com.apple.security.device.audio-input entitlement gets no dialog, so the\n" +
		"  toggle has nothing behind it. That is a defect in the launcher, not here.\n" +
		"  TCC says which it is:\n\n" +
		"    log show --last 5m --predicate 'subsystem == \"com.apple.TCC\"' \\\n" +
		"      | grep -i microphone")
	return b.String()
}

// signalOf translates a probe verdict into what it established.
//
// probeUnknown maps to SignalUnknown rather than to anything reassuring: a
// probe that could not answer has established nothing, and this check can show
// "definitely broken" but never "definitely fine".
func signalOf(v probeVerdict) Signal {
	switch v {
	case probeLive:
		return SignalCarrying
	case probeConstant:
		return SignalConstant
	case probeNoPackets:
		return SignalNone
	}
	return SignalUnknown
}

// probeTrack runs a short capture and reports what came out.
//
// Every check before this one asks the device whether it will open. On macOS a
// *denied* microphone opens: the audio unit starts, the stream format reads
// back, every call returns success. Preflight passed, a real meeting recorded,
// and the operator's side was empty — found by minutes-mac with the operator sitting at
// the machine, with "access to kTCCServiceMicrophone denied" in the system log
// and nothing in the capture path able to see it.
//
// Run on both tracks, and the verdict is read differently on each. That is a
// policy difference, not a measurement one, and it is the whole reason this
// takes a track rather than assuming one:
//
//   - On the microphone, "no audio" and "constant" are refusals. Recording
//     anyway produces a meeting with the operator missing from it, discovered
//     afterwards.
//   - On the system track neither is, because a loopback track legitimately
//     delivers nothing while the render endpoint is idle — which it is before
//     every meeting. A refusal there would block valid recordings to prevent a
//     problem that usually is not one, and the far end arriving late is normal.
//
// So the system track's verdict is reported and never enforced. Saying "ok"
// for both "it opened" and "audio comes out of it" is what hid the difference;
// measuring it and refusing to act on it is the honest position.
func probeTrack(ctx context.Context, helper string, track frame.Track) probeVerdict {
	probeCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()

	only := "--mic-only"
	if track == frame.TrackSystem {
		only = "--system-only"
	}
	cmd := exec.CommandContext(probeCtx, helper,
		"--duration-ms", strconv.Itoa(probeMillis), only)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return probeUnknown
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
		return probeUnknown
	}
	cmd.Stdin = r
	if err := cmd.Start(); err != nil {
		r.Close()
		w.Close()
		return probeUnknown
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
		if f.Type == frame.TypeTrackInfo && f.Track == track {
			if ti, err := frame.ParseTrackInfo(f.Payload); err == nil {
				info, haveInfo = ti, true
			}
			continue
		}
		if f.Type != frame.TypeAudio || f.Track != track || !haveInfo {
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
			return probeLive
		}
	}

	switch {
	case sawAudio:
		return probeConstant
	case haveInfo:
		// Declared a track and never wrote to it.
		return probeNoPackets
	}
	// The helper said the track was fine and then did not even declare it.
	// That is a fault, but it is indistinguishable from the probe failing to
	// run, so it is not grounds to refuse.
	return probeUnknown
}
