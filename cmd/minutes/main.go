// Command minutes records a meeting from this desktop — both sides of it.
//
// R1 built the capture path; R2 adds segments, a manifest, and a lifecycle that
// outlives the command that starts it. Transcription, summary and delivery are
// R3 and R4 and are deliberately absent.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"syscall"
	"time"

	"errors"

	"github.com/alexj212/minutes/internal/capture"
	"github.com/alexj212/minutes/internal/config"
	"github.com/alexj212/minutes/internal/deliver"
	"github.com/alexj212/minutes/internal/indicator"
	"github.com/alexj212/minutes/internal/manifest"
	"github.com/alexj212/minutes/internal/preflight"
	"github.com/alexj212/minutes/internal/session"
	"github.com/alexj212/minutes/internal/transcribe"
	"github.com/alexj212/minutes/internal/transcript"
)

// defaultRoot is where recordings go when nobody says otherwise.
//
// Not a relative path. Once `minutes` is on PATH it is run from wherever you
// happen to be standing, and a relative default would scatter meetings across
// whichever directories you were in when they started — then `minutes list`
// would show none of them, because it looks beside the current directory too.
func defaultRoot() string {
	if v := os.Getenv("MINUTES_ROOT"); v != "" {
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "recordings"
	}
	return filepath.Join(home, "minutes")
}

// parseFlags parses args that mix flags and positionals in any order, and
// returns the positionals.
//
// Go's flag package stops at the first non-flag argument, so
// `minutes transcribe <id> --model small` would take "--model" and "small" as
// two more recording ids and silently transcribe with the default model. Every
// command here takes an optional id followed by flags, which is exactly the
// shape that breaks.
func parseFlags(fs *flag.FlagSet, args []string) []string {
	var positional []string
	rest := args
	for {
		if err := fs.Parse(rest); err != nil {
			os.Exit(2)
		}
		rest = fs.Args()
		if len(rest) == 0 {
			return positional
		}
		positional = append(positional, rest[0])
		rest = rest[1:]
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `minutes — record both sides of a meeting

  minutes version [--json]
        What this build is, and whether its capture helper is installed
        beside it. --json is the shape a publisher verifies against.

  minutes preflight
        Report whether a recording started now would capture both tracks.
        Exits non-zero if it would not.

  minutes apps
        List processes producing audio, so one can be named with --app.

  minutes start [--name NAME] [--to PROJECT] [--app APP] [--segment 5m]
        Begin recording and return. The recording outlives this command.
        --to names where the notes should go; it defaults to this machine's
        own session, and is delivered there automatically once transcribed.

  minutes stop [ID] [--root DIR]
        Stop a recording and report what it captured. Defaults to the one
        that is running.

  minutes status [ID] [--root DIR]
        Show a recording's manifest.

  minutes list [--root DIR]
        List recordings, newest first, with size, transcript and delivery.

  minutes rm [ID...] [--older-than D] [--undelivered] [--force]
        Remove recordings. Refuses to remove ones whose notes were never
        delivered unless told otherwise.

  minutes prune [--dry-run] [--force] [--root DIR]
        Apply the retention policy from the config. Off unless configured.

  minutes config [--json]
        Show the settings actually in effect, and where they are read from.

  minutes config set KEY VALUE
        Change one setting, validating it first. "set --help" lists the keys.

  minutes record [--duration D] [--name NAME] [--segment 5m] [--root DIR]
        Record in the foreground until the duration elapses or Ctrl-C.

  minutes transcribe [ID] [--backend B] [--model M] [--root DIR]
        Transcribe a recording, both tracks, merged on the shared clock.
        Runs locally by default; audio leaves this machine only if a hosted
        backend is named.

  minutes deliver [ID] --to PROJECT [--notes FILE] [--include-flagged]
        Hand a meeting to a session. By default it sends the transcript so the
        session can write the notes; --notes sends notes you have already
        written and nothing else. Refuses to send a transcript containing
        stretches where the far end was silent, since those may be the room
        rather than the meeting.

Environment:
  MINUTES_ROOT     where recordings are kept (default: ~/minutes)
  MINUTES_HELPER   path to the capture helper (default: beside this binary)
`)
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	args := os.Args[2:]
	switch os.Args[1] {
	case "preflight":
		os.Exit(cmdPreflight())
	case "version", "--version", "-v":
		os.Exit(cmdVersion(args))
	case "start":
		os.Exit(cmdStart(args))
	case "stop":
		os.Exit(cmdStop(args))
	case "status":
		os.Exit(cmdStatus(args))
	case "list":
		os.Exit(cmdList(args))
	case "record":
		os.Exit(cmdRecord(args))
	case "transcribe":
		os.Exit(cmdTranscribe(args))
	case "deliver":
		os.Exit(cmdDeliver(args))
	case "rm":
		os.Exit(cmdRemove(args))
	case "prune":
		os.Exit(cmdPrune(args))
	case "config":
		os.Exit(cmdConfig(args))
	case "apps":
		os.Exit(cmdApps(args))
	case "supervise":
		os.Exit(cmdSupervise(args))
	case "-h", "--help", "help":
		usage()
		os.Exit(0)
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n\n", os.Args[1])
		usage()
		os.Exit(2)
	}
}

// readyToRecord runs the checks that must pass before a recording begins, and
// returns the helper path — or "" if it must not start.
//
// Preflight answers "can both sides be captured". These answer the two
// questions that are just as capable of losing a meeting and are not about
// audio at all: is something already recording, and is there room to write.
func readyToRecord(ctx context.Context, root string, force bool) string {
	helper := checkedHelper(ctx)
	if helper == "" {
		return ""
	}

	// Two supervisors each hold their own microphone and loopback client and
	// write the same meeting to two directories. It is almost always somebody
	// forgetting to stop the last one, and it makes a bare `stop` ambiguous.
	if live, err := session.Live(root); err == nil && len(live) > 0 {
		fmt.Fprintln(os.Stderr, "Something is already recording:")
		for _, st := range live {
			fmt.Fprintf(os.Stderr, "  ● %s (pid %d), started %s\n",
				st.ID, st.PID, st.StartedAt.Format("15:04:05"))
		}
		if !force {
			fmt.Fprintln(os.Stderr, "\nStarting another would capture the same meeting twice and make")
			fmt.Fprintln(os.Stderr, "`minutes stop` ambiguous. Stop that one first, or pass --force.")
			return ""
		}
		fmt.Fprintln(os.Stderr, "  --force given; starting anyway.")
	}

	res, err := preflight.Run(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "preflight failed: %v\n", err)
		return ""
	}
	head, err := session.EstimateHeadroom(root, res.StorageBytesPerSecond())
	if err != nil {
		// Not knowing the free space is not a reason to refuse a meeting.
		fmt.Fprintf(os.Stderr, "  (could not check free disk: %v)\n", err)
		return helper
	}
	switch {
	case head.Refuse():
		fmt.Fprintf(os.Stderr, "Refusing to record: not enough disk.\n  %s\n", head)
		fmt.Fprintln(os.Stderr, "\nNo real meeting fits in that, so this would fill the disk partway")
		fmt.Fprintln(os.Stderr, "through — costing the recording and possibly whatever else is running.")
		return ""
	case head.Warn():
		fmt.Printf("  %s\n", head)
	}
	return helper
}

// checkedHelper runs preflight and returns the helper path, or prints the
// refusal and returns "".
func checkedHelper(ctx context.Context) string {
	res, err := preflight.Run(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "preflight failed: %v\n", err)
		return ""
	}
	if !res.CanRecord {
		fmt.Fprint(os.Stderr, res.Describe())
		return ""
	}
	return res.HelperPath
}

// Build stamps, set by the linker. Empty in a plain `go build`, where the
// version falls back to what the toolchain recorded about the commit.
var (
	version string
	built   string
)

// Component is one file a release has to land.
//
// minutes is not one binary. The orchestrator is a Linux process and the
// capture helper is built for whatever OS owns the audio hardware — on this
// machine a Windows PE executed over WSL interop. They must arrive together:
// an orchestrator without its helper refuses to record and blames the helper,
// which is a working installation of nothing.
type Component struct {
	Name     string `json:"name"`
	Platform string `json:"platform"`
	Path     string `json:"path,omitempty"`
	// Present is whether it is actually installed. A release that landed
	// half-way is the failure this field exists to make visible.
	Present bool `json:"present"`
	// Degraded says, in words meant for whoever installs this, why the
	// component works here but will not work as well elsewhere. Empty when
	// there is nothing wrong with it.
	//
	// Present and Degraded are different questions. A component can be present,
	// correct, checksummed and complete, and still be useless to the machine
	// that receives it — a macOS helper signed ad-hoc carries a designated
	// requirement that is a bare hash of its own bytes, so the consent grant it
	// is given does not survive the trip. That set installs cleanly, reports
	// the right version, and refuses to record, which is the failure shape this
	// project keeps meeting: it does not fail, it just does not work, and
	// nobody is told why.
	//
	// A string rather than a boolean because the reason is the part a recipient
	// needs, and a boolean throws it away.
	//
	// It must be MEASURED from the artifact, never remembered from the build.
	// The helper on disk at publish time is not necessarily the one the build
	// produced — somebody rebuilds, copies one in, restores a backup — and a
	// field recording what a script intended is the same mistake as a manifest
	// naming the machine a recording did not come from. Ask the binary.
	Degraded string `json:"degraded,omitempty"`
}

// Version is what this build is, and what it needs beside it.
//
// The first three fields match what shabadoo emits so one publisher can verify
// any tool the same way. Components are additive: a reader that only knows the
// three fields is unaffected, and a reader that needs the set can have it.
type Version struct {
	Version    string      `json:"version"`
	Built      string      `json:"built"`
	Platform   string      `json:"platform"`
	Components []Component `json:"components,omitempty"`
}

// describeVersion assembles it.
func describeVersion() Version {
	v := Version{
		Version:  version,
		Built:    built,
		Platform: runtime.GOOS + "/" + runtime.GOARCH,
	}

	// Without ldflags, report what the toolchain recorded rather than
	// "unknown". A development build should still be able to say which commit
	// it is and whether the tree was dirty — that distinction has already cost
	// this project a stale deployment nobody noticed.
	if info, ok := debug.ReadBuildInfo(); ok {
		var revision, vcsTime string
		var dirty bool
		for _, s := range info.Settings {
			switch s.Key {
			case "vcs.revision":
				revision = s.Value
			case "vcs.time":
				vcsTime = s.Value
			case "vcs.modified":
				dirty = s.Value == "true"
			}
		}
		if v.Version == "" && revision != "" {
			short := revision
			if len(short) > 12 {
				short = short[:12]
			}
			v.Version = short
			if dirty {
				v.Version += "-dirty"
			}
		}
		if v.Built == "" {
			v.Built = vcsTime
		}
	}
	if v.Version == "" {
		v.Version = "unknown"
	}

	// The orchestrator is always this binary; the helper is whatever it would
	// actually use, found the same way a recording would find it.
	self, _ := os.Executable()
	v.Components = append(v.Components, Component{
		Name: "minutes", Platform: v.Platform, Path: self, Present: self != "",
	})

	helper, err := preflight.FindHelper()
	hc := Component{Name: "minutes-capture", Platform: helperPlatform(helper)}
	if err == nil {
		hc.Name, hc.Path, hc.Present = filepath.Base(helper), helper, true
		hc.Degraded = helperDegraded(helper)
	}
	v.Components = append(v.Components, hc)

	// The tray is part of the set but is not required to record, so its absence
	// is reported without making the set incomplete. `present: false` means
	// "this release landed half-way" and would refuse a publish; a machine that
	// simply has no tray helper is not a broken release.
	if tray := indicator.FindHelper(helper); tray != "" {
		v.Components = append(v.Components, Component{
			Name: filepath.Base(tray), Platform: helperPlatform(tray),
			Path: tray, Present: true,
		})
	}
	return v
}

// helperPlatform names what the helper was built for, which is not this
// machine's platform when the audio hardware belongs to another OS.
func helperPlatform(path string) string {
	if strings.HasSuffix(path, ".exe") {
		return "windows/amd64"
	}
	if runtime.GOOS == "linux" {
		// A Linux orchestrator with no helper found: on WSL the helper it wants
		// is the Windows one, which is the only reason this process can reach
		// audio at all.
		if preflight.IsWSL() {
			return "windows/amd64"
		}
	}
	return runtime.GOOS + "/" + runtime.GOARCH
}

func cmdVersion(args []string) int {
	fs := flag.NewFlagSet("version", flag.ExitOnError)
	asJSON := fs.Bool("json", false, "machine-readable, for a publisher verifying a build")
	parseFlags(fs, args)

	v := describeVersion()
	if *asJSON {
		body, err := json.MarshalIndent(v, "", "  ")
		if err != nil {
			fmt.Fprintf(os.Stderr, "%v\n", err)
			return 1
		}
		fmt.Println(string(body))
		return 0
	}

	fmt.Printf("minutes %s\n", v.Version)
	if v.Built != "" {
		fmt.Printf("  built    %s\n", v.Built)
	}
	fmt.Printf("  platform %s\n", v.Platform)
	missing := false
	for _, c := range v.Components {
		state := "missing"
		if c.Present {
			state = c.Path
		} else {
			missing = true
		}
		fmt.Printf("  %-18s %-14s %s\n", c.Name, c.Platform, state)
	}
	if missing {
		// Not an error: `version` should answer even when the installation is
		// broken, and saying so is the whole point.
		fmt.Println("\n  A component is missing. `minutes` needs its capture helper beside it;")
		fmt.Println("  without it a recording refuses to start and blames the helper.")
	}
	return 0
}

func cmdPreflight() int {
	res, err := preflight.Run(context.Background())
	if err != nil {
		fmt.Fprintf(os.Stderr, "preflight failed: %v\n", err)
		return 1
	}
	fmt.Print(res.Describe())
	if !res.CanRecord {
		return 1
	}
	return 0
}

func cmdStart(args []string) int {
	fs := flag.NewFlagSet("start", flag.ExitOnError)
	name := fs.String("name", "", "what the meeting is")
	root := fs.String("root", defaultRoot(), "where recordings are kept")
	seg := fs.Duration("segment", session.DefaultSegment, "segment length")
	force := fs.Bool("force", false, "start even if another recording is running")
	to := fs.String("to", "", "where the notes should go (default: this machine's own session)")
	app := fs.String("app", "", "capture only this application, rather than everything the machine plays")
	parseFlags(fs, args)

	helper := readyToRecord(context.Background(), *root, *force)
	if helper == "" {
		return 1
	}
	target, ok := resolveApp(context.Background(), helper, *app)
	if !ok {
		return 1
	}

	m, err := session.Start(session.StartOptions{
		Root: *root, Name: *name, Segment: *seg, Helper: helper,
		AppPID: target.PID, App: target.Name,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "could not start recording: %v\n", err)
		return 1
	}
	dest := destination(*to)
	if dest != "" {
		if err := m.SetIntendedFor(dest); err != nil {
			fmt.Fprintf(os.Stderr, "recording the destination: %v\n", err)
		}
	}

	// Recording is a trust matter, and in some places a legal one. A recording
	// that runs detached is exactly the one that could become quiet, so this
	// says so plainly.
	banner()
	fmt.Printf("  id:       %s\n", m.ID)
	fmt.Printf("  files:    %s\n", m.Dir())
	fmt.Printf("  segments: %s\n", seg.String())
	if dest != "" {
		fmt.Printf("  notes to: %s\n", dest)
	}
	if target.PID > 0 {
		fmt.Printf("  capturing: %s (pid %d) only\n", target.Name, target.PID)
	} else {
		fmt.Printf("  capturing: everything this machine plays\n")
	}
	fmt.Println()
	fmt.Printf("  stop with:  minutes stop %s\n", m.ID)
	return 0
}

// announce says out loud that a recording has started, beyond the terminal that
// started it: a marker file anything can read, and a notification if the agent
// is there. Both are best effort — neither is a reason not to record.
// noAudio reports a track that has been declared and has delivered nothing.
//
// It notifies rather than logs. A supervised recording's log is a file, and
// that is where the helper's own "track mic ended after 0 audio frames" sat
// unread while a 44-minute meeting was transcribed and delivered with one side
// of the conversation missing. The whole value of noticing this during the
// meeting is that somebody can still fix it, which requires telling somebody.
//
// The wording states what was measured rather than diagnosing it. Nothing on a
// track can mean the meeting has not started yet — a loopback stream is silent
// until something plays — and a false alarm that says "nothing has arrived" is
// cheap, where one that says "your microphone is broken" is not.
func noAudio(m *manifest.Manifest, track string, since time.Duration) {
	body := fmt.Sprintf("Nothing has been captured from the %s track in %s of %q. "+
		"If the meeting has started, that track is not recording — everything on it "+
		"will be missing from the transcript.", track, since.Round(time.Second), m.Name)
	fmt.Printf("  ⚠ no audio on the %s track after %s — if the meeting has started, "+
		"it is not being recorded\n", track, since.Round(time.Second))
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = deliver.New().Notify(ctx, "Recording is missing a track", body, "minutes")
}

// showIndicator puts the recording in the tray, and says so if it cannot.
//
// Failure is reported and not fatal. A recording whose indicator did not draw
// is still a recording, and refusing to record because a tray icon would not
// appear would be protecting the wrong thing.
func showIndicator(ctx context.Context, m *manifest.Manifest, helper string,
	log func(string, ...any), onStop func()) *indicator.Indicator {
	tray := indicator.FindHelper(helper)
	if tray == "" {
		return nil
	}
	name := m.Name
	if name == "" {
		name = m.ID
	}
	ind, err := indicator.Start(ctx, indicator.Options{
		Helper: tray, Name: name, Dir: m.Dir(), OnStop: onStop, Log: log,
	})
	if err != nil {
		log("the tray indicator did not start (%v) — recording anyway, "+
			"but nothing on screen will show that this is happening", err)
		return nil
	}
	return ind
}

func announce(m *manifest.Manifest) {
	if err := session.SetMarker(session.Marker{
		ID: m.ID, Name: m.Name, Dir: m.Dir(), PID: os.Getpid(), StartedAt: m.StartedAt,
	}); err != nil {
		fmt.Fprintf(os.Stderr, "  (could not write the recording marker: %v)\n", err)
	}
	// And in the recording directory, which is where `list` looks.
	//
	// The reasoning was already written down 500 lines below, about the
	// transcription phase: "a 'transcribing' directory with no live process is
	// how an *interrupted* transcription is detected, and without this a
	// perfectly healthy one would be reported as interrupted the moment anybody
	// looked." Exactly the same is true of the recording phase and only the
	// second half of the run ever got the file.
	//
	// `minutes start` was unaffected — session.Start writes the pid of the
	// supervisor it spawns — so this only ever bit `minutes record`, whose
	// supervisor is this process and which wrote nothing.
	//
	// The cost of getting it wrong is the bad direction: a LIVE recording
	// listed as `interrupted` invites recovery. Observed rather than reasoned
	// about — a meeting in progress was reported interrupted and transcribed
	// mid-flight on the strength of it, while the audio was still being
	// written.
	if err := os.WriteFile(filepath.Join(m.Dir(), session.PIDFile),
		[]byte(strconv.Itoa(os.Getpid())), 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "  (could not write the recorder pid file: %v)\n", err)
	}
	what := m.Name
	if what == "" {
		what = m.ID
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = deliver.New().Notify(ctx, "Recording started", "minutes is recording: "+what, "minutes")
}

// unannounce clears the marker and says the recording has ended.
func unannounce(m *manifest.Manifest) {
	if err := session.ClearMarker(); err != nil {
		fmt.Fprintf(os.Stderr, "  (could not clear the recording marker: %v)\n", err)
	}
	// Paired with the write in announce. The supervise path removes it again
	// later for its own reasons; os.Remove on a missing file is not an error
	// worth reporting.
	_ = os.Remove(filepath.Join(m.Dir(), session.PIDFile))
	what := m.Name
	if what == "" {
		what = m.ID
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = deliver.New().Notify(ctx, "Recording stopped",
		fmt.Sprintf("minutes stopped recording %s (%s)", what, duration(m.Duration())), "minutes")
}

// duration renders a length for a person.
func duration(seconds float64) string {
	d := time.Duration(seconds * float64(time.Second))
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
}

func banner() {
	fmt.Println("┌──────────────────────────────────────────────┐")
	fmt.Println("│  ● RECORDING — microphone and system audio   │")
	fmt.Println("└──────────────────────────────────────────────┘")
}

func cmdStop(args []string) int {
	fs := flag.NewFlagSet("stop", flag.ExitOnError)
	root := fs.String("root", defaultRoot(), "where recordings are kept")
	ids := parseFlags(fs, args)

	dir, err := session.Resolve(*root, first(ids))
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		return 1
	}
	m, err := session.Stop(dir, 30*time.Second)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		return 1
	}
	fmt.Println("  ○ stopped")
	fmt.Println()
	rc := report(m, true)
	if m.State == manifest.StateTranscribing {
		fmt.Printf("\n  transcribing in the background — `minutes list` shows when it is done.\n")
		fmt.Printf("  expect about %s.\n", roughly(transcribeSeconds(m.Duration())))
	}
	return rc
}

// transcribeSeconds estimates how long transcribing a recording will take.
//
// Both tracks are transcribed, so a meeting is twice its own length in audio,
// and whisper runs at about 7.4x real time — measured on a two-hour call, not
// on a short clip where loading the model dominates and makes it look like 1x.
func transcribeSeconds(recordingSeconds float64) float64 {
	const throughput = 7.4
	return recordingSeconds * 2 / throughput
}

// roughly renders a duration for a person waiting on it.
func roughly(seconds float64) string {
	switch {
	case seconds < 90:
		return "a minute"
	case seconds < 3600:
		return fmt.Sprintf("%.0f minutes", seconds/60)
	default:
		return fmt.Sprintf("%.1f hours", seconds/3600)
	}
}

func cmdStatus(args []string) int {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	root := fs.String("root", defaultRoot(), "where recordings are kept")
	ids := parseFlags(fs, args)

	dir, err := session.Resolve(*root, first(ids))
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		return 1
	}
	st, err := session.Open(dir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		return 1
	}
	if st.Live {
		banner()
	}
	fmt.Printf("  id:    %s\n", st.ID)
	if st.Name != "" {
		fmt.Printf("  name:  %s\n", st.Name)
	}
	fmt.Printf("  state: %s", st.StateLabel())
	if st.Live {
		fmt.Printf(" (supervisor pid %d)", st.PID)
	}
	fmt.Println()
	fmt.Printf("  since: %s\n", st.StartedAt.Format(time.RFC3339))
	fmt.Printf("  files: %s\n\n", st.Dir())
	if st.Interrupted() {
		fmt.Println("  This recording was interrupted: the manifest says it is running,")
		fmt.Println("  but no supervisor is. Completed segments below are intact.")
		fmt.Println()
	}
	return report(st.Manifest, !st.Live)
}

func cmdList(args []string) int {
	fs := flag.NewFlagSet("list", flag.ExitOnError)
	root := fs.String("root", defaultRoot(), "where recordings are kept")
	parseFlags(fs, args)

	all, err := session.List(*root)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		return 1
	}
	if len(all) == 0 {
		fmt.Printf("no recordings under %s\n", *root)
		return 0
	}

	// Column width from the data. A fixed one looked fine until a meeting was
	// named after what it was about, and then every row was ragged.
	idWidth := len("ID")
	for _, st := range all {
		if n := len(st.ID); n > idWidth {
			idWidth = n
		}
	}
	fmt.Printf("  %-*s %-12s %8s %8s  %-12s %s\n",
		idWidth, "ID", "STATE", "LENGTH", "SIZE", "TRANSCRIPT", "DELIVERED")

	var total int64
	for _, st := range all {
		mark := " "
		if st.Live {
			mark = "●" // an active recording is visible in a plain listing
		}
		size, _ := session.DirSize(st.Dir())
		total += size

		transcribed := "—"
		if t := st.Transcript; t != nil {
			transcribed = fmt.Sprintf("%d lines", t.Lines)
			if t.AudioLeftMachine {
				// Worth seeing at a glance which meetings left the machine.
				transcribed += " ↑"
			}
		}
		delivered := "—"
		if d := st.Delivery; d != nil {
			delivered = d.To
			if d.Degraded {
				delivered += " (on disk only)"
			}
		}

		fmt.Printf("%s %-*s %-12s %7.1fs %8s  %-12s %s\n",
			mark, idWidth, st.ID, st.StateLabel(), st.Duration(),
			session.HumanBytes(size), transcribed, delivered)
		// The id usually already ends in a slug of the name; repeating it
		// underneath every row is noise.
		if st.Name != "" && !strings.HasSuffix(st.ID, slug(st.Name)) {
			fmt.Printf("  %-*s %s\n", idWidth, "", st.Name)
		}
	}
	fmt.Printf("\n  %d recording(s), %s in %s\n", len(all), session.HumanBytes(total), *root)
	for _, st := range all {
		if st.Transcript != nil && st.Transcript.AudioLeftMachine {
			fmt.Printf("  ↑ marks a meeting whose audio was sent off this machine.\n")
			break
		}
	}
	return 0
}

func cmdApps(args []string) int {
	fs := flag.NewFlagSet("apps", flag.ExitOnError)
	parseFlags(fs, args)

	helper, err := preflight.FindHelper()
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		return 1
	}
	apps, err := preflight.ListApps(context.Background(), helper)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		return 1
	}
	if len(apps) == 0 {
		fmt.Println("nothing has an audio session right now.")
		fmt.Println("Start the meeting first — a process the audio engine does not know about")
		fmt.Println("cannot be captured.")
		return 0
	}
	fmt.Printf("  %-26s %-8s %s\n", "APPLICATION", "PID", "")
	for _, a := range apps {
		mark := " "
		state := ""
		if a.Active {
			mark, state = "♪", "playing"
		}
		fmt.Printf("%s %-26s %-8d %s\n", mark, a.Name, a.PID, state)
	}
	fmt.Println()
	fmt.Println("  Record only one of these with:  minutes start --app <name>")
	fmt.Println("  Without --app the recording takes everything the machine plays,")
	fmt.Println("  including whatever else is open.")
	return 0
}

// resolveApp turns --app into a process id, or explains why it cannot.
func resolveApp(ctx context.Context, helper, want string) (preflight.App, bool) {
	if want == "" {
		return preflight.App{}, true
	}
	apps, err := preflight.ListApps(ctx, helper)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		return preflight.App{}, false
	}
	app, err := preflight.FindApp(apps, want)
	if err != nil {
		// Never fall back to recording everything. Silently widening the
		// capture is how a meeting ends up with a film in it.
		fmt.Fprintf(os.Stderr, "%v\n", err)
		return preflight.App{}, false
	}
	return app, true
}

func cmdPrune(args []string) int {
	fs := flag.NewFlagSet("prune", flag.ExitOnError)
	root := fs.String("root", defaultRoot(), "where recordings are kept")
	force := fs.Bool("force", false, "do not ask")
	dryRun := fs.Bool("dry-run", false, "say what would go, and remove nothing")
	parseFlags(fs, args)

	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		return 1
	}
	if !cfg.Retention.Enabled() {
		fmt.Println("No retention policy is configured, so nothing is removed automatically.")
		fmt.Printf("Set one in %s, for example:\n\n", config.Path())
		fmt.Println(`  "retention": { "keepDays": 90, "keepUndelivered": true }`)
		fmt.Println("\nDeleting recordings without being asked is worse than using disk,")
		fmt.Println("which is why this is off until you say otherwise.")
		fmt.Println("\n`minutes rm` removes things by hand in the meantime.")
		return 0
	}

	all, err := session.List(*root)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		return 1
	}
	doomed, spared := cfg.Retention.Doomed(all, time.Now())

	for _, c := range spared {
		fmt.Printf("  keeping %-28s %8s  — %s\n", c.ID, session.HumanBytes(c.Size), c.Reason)
	}
	if len(doomed) == 0 {
		fmt.Println("nothing to remove")
		return 0
	}

	var total int64
	fmt.Println("Would remove:")
	for _, c := range doomed {
		total += c.Size
		fmt.Printf("  %-28s %8s  — %s\n", c.ID, session.HumanBytes(c.Size), c.Reason)
	}
	fmt.Printf("  %s total\n", session.HumanBytes(total))

	if *dryRun {
		return 0
	}
	if !*force {
		fmt.Print("\nRemove these? [y/N] ")
		var answer string
		fmt.Scanln(&answer)
		if answer != "y" && answer != "Y" {
			fmt.Println("nothing removed")
			return 0
		}
	}
	for _, c := range doomed {
		if err := os.RemoveAll(c.Dir()); err != nil {
			fmt.Fprintf(os.Stderr, "removing %s: %v\n", c.ID, err)
			return 1
		}
		fmt.Printf("  removed %s\n", c.ID)
	}
	fmt.Printf("  %s freed\n", session.HumanBytes(total))
	return 0
}

func cmdRemove(args []string) int {
	fs := flag.NewFlagSet("rm", flag.ExitOnError)
	root := fs.String("root", defaultRoot(), "where recordings are kept")
	olderThan := fs.Duration("older-than", 0, "also remove anything older than this")
	force := fs.Bool("force", false, "do not ask")
	undelivered := fs.Bool("undelivered", false, "allow removing recordings whose notes were never delivered")
	ids := parseFlags(fs, args)

	all, err := session.List(*root)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		return 1
	}

	wanted := map[string]bool{}
	for _, id := range ids {
		wanted[id] = true
	}

	var doomed []session.Status
	for _, st := range all {
		switch {
		case wanted[st.ID]:
		case *olderThan > 0 && time.Since(st.StartedAt) > *olderThan:
		default:
			continue
		}
		if st.Live {
			// Deleting the directory out from under a running supervisor would
			// leave it writing into nothing.
			fmt.Fprintf(os.Stderr, "%s is still recording; stop it first.\n", st.ID)
			return 1
		}
		doomed = append(doomed, st)
	}

	if len(doomed) == 0 {
		fmt.Println("nothing matched")
		return 0
	}

	var total int64
	fmt.Println("Would remove:")
	blocked := 0
	for _, st := range doomed {
		size, _ := session.DirSize(st.Dir())
		total += size
		note := ""
		if st.Delivery == nil {
			// The notes were never sent anywhere, so deleting this loses the
			// only copy of a meeting nobody has read.
			note = "  ← never delivered"
			blocked++
		} else {
			// A message in somebody's inbox names these paths. Removing them
			// leaves it pointing at nothing, which reads as a mistake rather
			// than as a deliberate cleanup.
			note = fmt.Sprintf("  ← delivered to %s; a message points at these files", st.Delivery.To)
		}
		fmt.Printf("  %-28s %8s%s\n", st.ID, session.HumanBytes(size), note)
	}
	fmt.Printf("  %s total\n", session.HumanBytes(total))

	if blocked > 0 && !*undelivered {
		fmt.Fprintf(os.Stderr, "\n%d of these were never delivered, so deleting them loses the only\n", blocked)
		fmt.Fprintln(os.Stderr, "copy of a meeting nobody has read. Pass --undelivered if that is fine.")
		return 1
	}

	if !*force {
		fmt.Print("\nRemove these? [y/N] ")
		var answer string
		fmt.Scanln(&answer)
		if answer != "y" && answer != "Y" {
			fmt.Println("nothing removed")
			return 0
		}
	}
	for _, st := range doomed {
		if err := os.RemoveAll(st.Dir()); err != nil {
			fmt.Fprintf(os.Stderr, "removing %s: %v\n", st.ID, err)
			return 1
		}
		fmt.Printf("  removed %s\n", st.ID)
	}
	fmt.Printf("  %s freed\n", session.HumanBytes(total))
	return 0
}

func cmdRecord(args []string) int {
	fs := flag.NewFlagSet("record", flag.ExitOnError)
	dur := fs.Duration("duration", 0, "how long to record; 0 means until Ctrl-C")
	name := fs.String("name", "", "what the meeting is")
	root := fs.String("root", defaultRoot(), "where recordings are kept")
	seg := fs.Duration("segment", session.DefaultSegment, "segment length")
	force := fs.Bool("force", false, "record even if another recording is running")
	to := fs.String("to", "", "where the notes should go (default: this machine's own session)")
	app := fs.String("app", "", "capture only this application, rather than everything the machine plays")
	parseFlags(fs, args)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	helper := readyToRecord(ctx, *root, *force)
	if helper == "" {
		return 1
	}
	target, ok := resolveApp(ctx, helper, *app)
	if !ok {
		return 1
	}

	id := session.NewID(time.Now(), *name)
	dir := filepath.Join(*root, id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		return 1
	}
	m := manifest.New(dir, id, *name, seg.Seconds())
	if dest := destination(*to); dest != "" {
		m.IntendedFor = dest
	}
	m.App = target.Name
	if err := m.Save(); err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		return 1
	}

	banner()
	announce(m)
	defer unannounce(m)
	if *dur > 0 {
		fmt.Printf("  for %s, or Ctrl-C to stop early\n", *dur)
	} else {
		fmt.Print("  until Ctrl-C\n")
	}
	fmt.Printf("  files: %s\n\n", dir)

	log := func(f string, a ...any) { fmt.Printf("  "+f+"\n", a...) }
	recCtx, endRecording := context.WithCancel(ctx)
	defer endRecording()
	ind := showIndicator(recCtx, m, helper, log, endRecording)
	defer ind.Stop()

	runErr := capture.Run(recCtx, capture.Options{
		Helper: helper, Manifest: m, Duration: *dur, AppPID: target.PID,
		Log:       log,
		OnNoAudio: func(track string, since time.Duration) { noAudio(m, track, since) },
	})
	if err := m.Finish(runErr); err != nil {
		fmt.Fprintf(os.Stderr, "writing manifest: %v\n", err)
	}
	if runErr != nil {
		fmt.Fprintf(os.Stderr, "\nrecording failed: %v\n", runErr)
		report(m, false)
		return 1
	}
	fmt.Println("\n  ○ stopped")
	fmt.Println()
	rc := report(m, true)

	// The same continuation the detached supervisor does. Without this, whether
	// a meeting gets transcribed depends on which command started it, which is
	// not a distinction anybody should have to remember at the end of a call.
	if cfg, err := config.Load(); err != nil {
		fmt.Fprintf(os.Stderr, "  config unreadable, not transcribing: %v\n", err)
	} else if cfg.Transcription.AfterStop {
		fmt.Printf("\n  transcribing — about %s. Ctrl-C to leave it for `minutes transcribe` later.\n\n",
			roughly(transcribeSeconds(m.Duration())))
		log := func(f string, a ...any) { fmt.Printf("  "+f+"\n", a...) }
		if err := transcribeInto(ctx, m, cfg, log); err != nil {
			fmt.Fprintf(os.Stderr, "  transcription failed (the recording is intact): %v\n", err)
		} else {
			if t, lerr := transcript.Load(m.Dir()); lerr == nil {
				fmt.Printf("\n  %d lines -> %s\n", len(t.Lines), filepath.Join(m.Dir(), transcript.TextName))
			}
			autoDeliver(ctx, m, cfg, log)
		}
	}
	return rc
}

func cmdTranscribe(args []string) int {
	fs := flag.NewFlagSet("transcribe", flag.ExitOnError)
	root := fs.String("root", defaultRoot(), "where recordings are kept")
	backend := fs.String("backend", "", "override the configured backend")
	model := fs.String("model", "", "override the configured model")
	ids := parseFlags(fs, args)

	dir, err := session.Resolve(*root, first(ids))
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		return 1
	}
	st, err := session.Open(dir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		return 1
	}
	if st.Live {
		fmt.Fprintf(os.Stderr, "%s is still recording. Stop it first.\n", st.ID)
		return 1
	}

	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		return 1
	}
	log := func(f string, a ...any) { fmt.Printf("  "+f+"\n", a...) }
	if *backend != "" {
		cfg.Transcription.Backend = *backend
	}
	if *model != "" {
		cfg.Transcription.Model = *model
	}

	tr, err := transcribe.New(cfg.TranscribeOptions(log))
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		return 1
	}

	// Where the audio goes is stated before it goes there, every time.
	if tr.SendsAudioOffMachine() {
		fmt.Printf("  ⚠ %s uploads this meeting's audio to a third party.\n", tr.Name())
		fmt.Printf("    Configured in %s.\n\n", config.Path())
	} else {
		fmt.Printf("  %s — audio stays on this machine.\n\n", tr.Name())
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Say so in the manifest for as long as it runs, so `minutes list` shows a
	// transcription in progress. Without this a job that takes an hour is
	// invisible to everything except the terminal that started it, and looks
	// like nothing is happening.
	wasState := st.State
	if err := st.Manifest.SetState(manifest.StateTranscribing); err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		return 1
	}
	// A pid file alongside it, because the state alone is not enough: a
	// "transcribing" directory with no live process is how an *interrupted*
	// transcription is detected, and without this a perfectly healthy one would
	// be reported as interrupted the moment anybody looked.
	pidPath := filepath.Join(dir, session.PIDFile)
	if err := os.WriteFile(pidPath, []byte(strconv.Itoa(os.Getpid())), 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		return 1
	}
	restore := func() {
		os.Remove(pidPath)
		if err := st.Manifest.SetState(wasState); err != nil {
			fmt.Fprintf(os.Stderr, "restoring state: %v\n", err)
		}
	}

	started := time.Now()
	if err := transcribeInto(ctx, st.Manifest, cfg, log); err != nil {
		restore()
		fmt.Fprintf(os.Stderr, "transcription failed: %v\n", err)
		return 1
	}
	restore()
	t, err := transcript.Load(dir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		return 1
	}

	// Counted explicitly rather than by an else-branch. "Anything that is not
	// you is everyone else" is the same inference that made a recording with
	// one source report "0 you, 3 everyone else" directly beneath a warning
	// saying it could not say who spoke — the tally is the line a person
	// actually reads, so it must not contradict the disclosure above it.
	c := transcript.Count(t.Lines)
	took := time.Since(started).Round(time.Second)
	if t.Unattributed {
		fmt.Printf("\n  %d lines in %s — unattributed, this recording cannot say who spoke\n",
			len(t.Lines), took)
	} else {
		fmt.Printf("\n  %d lines in %s — %d you, %d everyone else%s\n",
			len(t.Lines), took, c.You, c.Others,
			map[bool]string{true: fmt.Sprintf(", %d unattributed", c.Unattributed)}[c.Unattributed > 0])
	}
	fmt.Printf("  %s\n", filepath.Join(dir, transcript.TextName))
	return 0
}

func cmdDeliver(args []string) int {
	fs := flag.NewFlagSet("deliver", flag.ExitOnError)
	root := fs.String("root", defaultRoot(), "where recordings are kept")
	to := fs.String("to", "", "the project whose session should write the notes")
	from := fs.String("from", "minutes", "who the message is from")
	notesFile := fs.String("notes", "", "send these notes instead of the transcript")
	includeFlagged := fs.Bool("include-flagged", false,
		"send the transcript even though stretches of it may be the room rather than the meeting")
	noNotify := fs.Bool("no-notify", false, "do not send a human notification")
	ids := parseFlags(fs, args)

	dir, err := session.Resolve(*root, first(ids))
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		return 1
	}
	m, err := manifest.Load(dir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		return 1
	}

	// A delivered message names these paths and is read later — sometimes much
	// later, since mail waits for a session to start. Sending from somewhere
	// that gets cleared produces a message pointing at nothing.
	if session.IsVolatile(dir) {
		fmt.Fprintf(os.Stderr, "Refusing to deliver from %s.\n\n", dir)
		fmt.Fprintln(os.Stderr, "  That directory is temporary, and the message would name paths that")
		fmt.Fprintln(os.Stderr, "  will not be there when somebody reads it — which looks exactly like")
		fmt.Fprintln(os.Stderr, "  a typo. Move the recording somewhere durable, or record with")
		fmt.Fprintln(os.Stderr, "  --root pointing at one.")
		return 1
	}

	// Where it goes: what was asked for, then what the recording was started
	// with, then this machine's own session. Nothing is guessed — every one of
	// those was named by a person at some point.
	target := *to
	if target == "" {
		target = m.IntendedFor
	}
	if target == "" {
		target = destination("")
	}
	if target == "" {
		fmt.Fprintln(os.Stderr, "deliver needs --to: which project the notes belong to is a judgment call,")
		fmt.Fprintln(os.Stderr, "and this program deliberately does not make it. Name the project,")
		fmt.Fprintln(os.Stderr, "or set delivery.to in the config, or pass --to when starting a recording.")
		return 2
	}

	var title, body, notifyBody string

	if *notesFile != "" {
		// Notes somebody has already written. Nothing about the transcript
		// goes with them — not the text, not a path to it.
		raw, err := os.ReadFile(*notesFile)
		if err != nil {
			fmt.Fprintf(os.Stderr, "reading notes: %v\n", err)
			return 1
		}
		if len(strings.TrimSpace(string(raw))) == 0 {
			fmt.Fprintf(os.Stderr, "%s is empty; there is nothing to deliver\n", *notesFile)
			return 1
		}
		n := deliver.Notes{Recording: m, Text: string(raw)}
		title, body, notifyBody = n.Title(), n.Body(), n.NotifyBody()
	} else {
		t, err := transcript.Load(dir)
		if err != nil {
			fmt.Fprintf(os.Stderr, "no transcript in %s: run `minutes transcribe` first, or pass --notes (%v)\n", dir, err)
			return 1
		}
		// A transcript with flagged stretches is one where the call had dropped
		// and the room had not. Sending it whole is exactly what should not
		// happen by default: on the meeting that prompted this, those stretches
		// held thirteen minutes of somebody's family.
		if len(t.FarEndSilent) > 0 && !*includeFlagged {
			var flagged float64
			for _, g := range t.FarEndSilent {
				flagged += g.Duration()
			}
			fmt.Fprintf(os.Stderr, "Refusing to send this transcript whole.\n\n")
			fmt.Fprintf(os.Stderr, "  %d stretch(es), %.0f minutes in total, are marked: the other side was\n",
				len(t.FarEndSilent), flagged/60)
			fmt.Fprintf(os.Stderr, "  silent, so what the microphone recorded there may be the room rather\n")
			fmt.Fprintf(os.Stderr, "  than the meeting.\n\n")
			for _, g := range t.FarEndSilent {
				fmt.Fprintf(os.Stderr, "    %s - %s   %.0f min\n",
					clockOf(g.Start), clockOf(g.End), g.Duration()/60)
			}
			fmt.Fprintf(os.Stderr, "\n  Read %s, then either\n", filepath.Join(dir, transcript.TextName))
			fmt.Fprintf(os.Stderr, "  write it up and send that with --notes, or pass --include-flagged\n")
			fmt.Fprintf(os.Stderr, "  if the whole transcript is genuinely fine to hand over.\n")
			return 1
		}
		b := deliver.Brief{Recording: m, Transcript: t}
		title, body, notifyBody = b.Title(), b.Body(), b.NotifyBody()
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	client := deliver.New()
	err = client.Send(ctx, deliver.Message{
		To: target, From: *from, Tag: "minutes", Title: title, Body: body,
	})

	// Two different failures, one behaviour: keep the brief on disk and say so.
	//
	// An unreachable agent is transient — a coordinator blipped, and a recorder
	// that lost a meeting over that would be worse than one that never
	// integrated. An unknown destination is the opposite: it is final. The
	// message is not queued and will not arrive when somebody opens that
	// project, because the coordinator only routes to destinations it has
	// already seen. Either way the notes must not evaporate, and either way the
	// operator must not be told they were sent.
	if errors.Is(err, deliver.ErrUnreachable) || errors.Is(err, deliver.ErrUnknownDestination) {
		path := filepath.Join(dir, "delivery.md")
		if werr := os.WriteFile(path, []byte("# "+title+"\n\n"+body), 0o644); werr != nil {
			fmt.Fprintf(os.Stderr, "delivery failed, and writing the brief failed too: %v\n", werr)
			return 1
		}
		if serr := m.SetDelivery(manifest.DeliveryRecord{To: target, At: time.Now(), Degraded: true}); serr != nil {
			fmt.Fprintf(os.Stderr, "recording the delivery: %v\n", serr)
		}
		if errors.Is(err, deliver.ErrUnknownDestination) {
			fmt.Printf("  %q is not a destination this coordinator knows, so nothing was sent.\n", target)
			fmt.Printf("  this is final rather than queued: a project it has never seen is refused\n")
			fmt.Printf("  at send time and nothing is kept for it. %v\n", err)
			fmt.Printf("  `shaba folders` lists what is addressable; adding the folder to the boot\n")
			fmt.Printf("  list makes it addressable before it has ever been opened.\n")
		} else {
			fmt.Printf("  the shabadoo agent is not reachable, so nothing was delivered.\n")
		}
		fmt.Printf("  the brief is on disk and nothing was lost:\n    %s\n", path)
		return 0
	}
	if errors.Is(err, deliver.ErrThrottled) {
		fmt.Fprintf(os.Stderr, "the coordinator is throttling this sender: %v\n", err)
		fmt.Fprintln(os.Stderr, "that is the loop guard, and a recorder should never reach it — "+
			"notes go out once per meeting. Something is sending in a loop.")
		return 1
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "delivery failed: %v\n", err)
		return 1
	}
	if serr := m.SetDelivery(manifest.DeliveryRecord{To: target, At: time.Now()}); serr != nil {
		fmt.Fprintf(os.Stderr, "recording the delivery: %v\n", serr)
	}
	if *notesFile != "" {
		fmt.Printf("  notes delivered to %s (no transcript sent)\n", target)
	} else {
		fmt.Printf("  notes requested from %s\n", target)
	}

	if !*noNotify {
		if err := client.Notify(ctx, "Meeting recorded", notifyBody, "minutes"); err != nil {
			// The message is what matters; the notification is a courtesy.
			fmt.Printf("  (the human notification did not go out: %v)\n", err)
		}
	}
	return 0
}

// clockOf renders seconds as h:mm:ss for a person reading a refusal.
func clockOf(seconds float64) string {
	t := int(seconds)
	return fmt.Sprintf("%d:%02d:%02d", t/3600, (t/60)%60, t%60)
}

// cmdSupervise is the detached half of `start`. It is not in the usage text
// because nobody should run it directly.
func cmdSupervise(args []string) int {
	fs := flag.NewFlagSet("supervise", flag.ExitOnError)
	dir := fs.String("dir", "", "recording directory")
	helper := fs.String("helper", "", "capture helper path")
	appPID := fs.Int("app-pid", 0, "capture only this process")
	parseFlags(fs, args)
	if *dir == "" || *helper == "" {
		fmt.Fprintln(os.Stderr, "supervise needs --dir and --helper")
		return 2
	}

	m, err := manifest.Load(*dir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "loading manifest: %v\n", err)
		return 1
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	fmt.Printf("supervising %s\n", m.ID)
	announce(m)
	log := func(f string, a ...any) { fmt.Printf(f+"\n", a...) }
	recCtx, endRecording := context.WithCancel(ctx)
	defer endRecording()
	// Stopping from the tray cancels the recording context, which is exactly
	// what `minutes stop` and a Ctrl-C already do — one path to ending a
	// recording rather than three, so the manifest is finished and the
	// transcript queued the same way however it was asked for.
	ind := showIndicator(recCtx, m, *helper, log, endRecording)
	defer ind.Stop()

	runErr := capture.Run(recCtx, capture.Options{
		Helper: *helper, Manifest: m, AppPID: *appPID,
		Log:       log,
		OnNoAudio: func(track string, since time.Duration) { noAudio(m, track, since) },
	})
	// Capture has ended, whatever happens next. The marker must go now rather
	// than after transcription, or the machine keeps claiming to be recording
	// for the length of the transcript.
	unannounce(m)

	if runErr != nil {
		if err := m.Finish(runErr); err != nil {
			fmt.Fprintf(os.Stderr, "writing manifest: %v\n", err)
		}
		os.Remove(filepath.Join(*dir, session.PIDFile))
		fmt.Fprintf(os.Stderr, "recording failed: %v\n", runErr)
		return 1
	}

	// Capture is done and the audio is safe. Anything after this point can fail
	// without costing the meeting.
	cfg, cfgErr := config.Load()
	if cfgErr != nil {
		fmt.Fprintf(os.Stderr, "config unreadable, not transcribing: %v\n", cfgErr)
	} else if cfg.Transcription.AfterStop {
		// The state moves before the work starts, because `stop` is waiting for
		// it: it returns as soon as capture is complete rather than waiting out
		// a transcript that runs at about real time.
		if err := m.SetState(manifest.StateTranscribing); err != nil {
			fmt.Fprintf(os.Stderr, "writing manifest: %v\n", err)
		}
		log := func(f string, a ...any) { fmt.Printf(f+"\n", a...) }
		if err := transcribeInto(context.Background(), m, cfg, log); err != nil {
			// A failed transcript is not a failed recording. The audio is on
			// disk and `minutes transcribe` can be run again.
			fmt.Fprintf(os.Stderr, "transcription failed (the recording is intact): %v\n", err)
		} else {
			autoDeliver(context.Background(), m, cfg, log)
		}
	}

	if err := m.Finish(nil); err != nil {
		fmt.Fprintf(os.Stderr, "writing manifest: %v\n", err)
	}
	// The pid file is removed last: while it exists and names a live process,
	// `stop` has something to signal.
	os.Remove(filepath.Join(*dir, session.PIDFile))
	fmt.Println("stopped cleanly")
	return 0
}

// autoDeliver hands a finished recording to this node's own session, if that is
// safe to do without asking.
//
// Safe means three things, and all of them have to hold. There has to be a
// destination; it has to be the core session, because delivering there keeps
// the transcript on the machine that made it while sending to another project
// is publishing; and the transcript must carry no stretches where the far end
// was silent, since those may be the room rather than the meeting.
//
// Anything else waits for `minutes deliver`, which will pick up the stored
// destination so nobody has to remember it.
func autoDeliver(ctx context.Context, m *manifest.Manifest, cfg *config.Config, log func(string, ...any)) {
	if !cfg.Delivery.Auto {
		return
	}
	to := m.IntendedFor
	if to == "" {
		to = cfg.Delivery.To
	}
	if to == "" {
		return
	}
	// The same refusal the manual path makes. A delivered message names these
	// paths and is read later; sending from a directory that will not survive
	// produces a message pointing at nothing.
	//
	// This guard was on `minutes deliver` and not here, so every automatic
	// delivery from a temporary directory went out unchecked — which cost a
	// reviewing session the verification on two separate runs.
	if session.IsVolatile(m.Dir()) {
		log("not delivering automatically: %s is temporary, and the message would name "+
			"paths that will not be there when somebody reads it", m.Dir())
		return
	}
	// A recording the banner has just called half a meeting does not go out by
	// itself.
	//
	// Automatic delivery is for the ordinary case, and its safety rests on the
	// destination being this machine's own session — a person is in the loop.
	// That argument does not stretch to a recording already known to be broken:
	// it arrives as a normal brief, is read as a normal meeting, and the one
	// thing worth knowing about it is in a banner the reader has no reason to
	// expect. minutes-mac's half-empty test recording reached a session inbox
	// this way while they were still reading the failure.
	//
	// Not deleted, not hidden — stored, with the reason. Deciding what a broken
	// recording is worth is a judgment, which is the same argument that keeps
	// the destination out of this program's hands.
	for _, t := range m.Tracks {
		if t.Constant() {
			log("not delivering automatically: the %s track carries no signal at all — "+
				"every sample identical, which is a denied, muted or disconnected device "+
				"rather than a quiet one. Half a meeting was recorded. It is on disk; "+
				"use `minutes deliver` if you want it sent anyway.", t.Name)
			return
		}
	}

	if cfg.Delivery.CoreSession == "" || to != cfg.Delivery.CoreSession {
		log("not delivering automatically: %q is not this machine's own session, and sending "+
			"a meeting to another project is publishing rather than filing. Use `minutes deliver`.", to)
		return
	}

	t, err := transcript.Load(m.Dir())
	if err != nil {
		return
	}
	if len(t.FarEndSilent) > 0 {
		log("not delivering automatically: %d stretch(es) are marked where the other side was "+
			"silent, so what was recorded there may be the room rather than the meeting. "+
			"Read it, then deliver by hand.", len(t.FarEndSilent))
		return
	}

	brief := deliver.Brief{Recording: m, Transcript: t}
	client := deliver.New()
	if err := client.Send(ctx, deliver.Message{
		To: to, From: "minutes", Tag: "minutes",
		Title: brief.Title(), Body: brief.Body(),
	}); err != nil {
		// Never fatal. The recording is on disk and `minutes deliver` can send
		// it later; a recorder that failed because a coordinator blipped would
		// be worse than one that never integrated.
		log("could not deliver to %s (%v); the recording is on disk and `minutes deliver` will send it", to, err)
		return
	}
	if err := m.SetDelivery(manifest.DeliveryRecord{To: to, At: time.Now()}); err != nil {
		log("recording the delivery: %v", err)
	}
	log("delivered to %s", to)
}

// transcribeInto transcribes a recording and records the result. Shared by the
// supervisor and the `transcribe` command so the two cannot drift.
func transcribeInto(ctx context.Context, m *manifest.Manifest, cfg *config.Config, log func(string, ...any)) error {
	tr, err := transcribe.New(cfg.TranscribeOptions(log))
	if err != nil {
		return err
	}
	t, err := transcript.Build(ctx, m, tr, log)
	if err != nil {
		return err
	}
	if err := t.Write(m.Dir()); err != nil {
		return err
	}
	return m.SetTranscript(manifest.TranscriptRecord{
		Backend:          t.Backend,
		AudioLeftMachine: t.AudioLeftMachine,
		CreatedAt:        t.CreatedAt,
		Lines:            len(t.Lines),
		File:             transcript.JSONName,
	})
}

// slug mirrors the id-building in session.NewID, so a listing does not repeat a
// meeting name underneath an id that already contains it.
func slug(name string) string {
	return strings.ToLower(strings.Trim(nonAlnum.ReplaceAllString(name, "-"), "-"))
}

var nonAlnum = regexp.MustCompile(`[^a-zA-Z0-9]+`)

// destination resolves where a recording's notes should go: what was asked for,
// or this machine's own session.
func destination(asked string) string {
	if asked != "" {
		return asked
	}
	cfg, err := config.Load()
	if err != nil {
		return ""
	}
	return cfg.Delivery.To
}

// first returns the first element, or "" — the commands take an optional id.
func first(ids []string) string {
	if len(ids) == 0 {
		return ""
	}
	return ids[0]
}

// report prints what a recording captured. When judge is set it also fails on a
// silent track; a recording that is still running cannot be judged, because a
// track that has not been written to yet is not the same as a track with
// nothing on it.
func report(m *manifest.Manifest, judge bool) int {
	if len(m.Tracks) == 0 {
		fmt.Println("  nothing was captured")
		return 1
	}
	silent := false
	var mute []manifest.Track
	for _, t := range m.Tracks {
		peak := t.LevelText()
		switch {
		case !t.Started():
			peak = "starting"
		case t.Silent():
			peak, silent = "SILENT", true
		case !t.CarriesSpeech():
			// Above digital silence, far below anybody talking. This is the
			// case that used to sail through and get transcribed into
			// invention.
			peak += "  NO SPEECH"
			mute = append(mute, t)
		}
		fmt.Printf("  %-7s %7.1fs  %2d segment(s)  peak %-11s %s\n",
			t.Name, t.Duration(), len(t.Segments), peak, t.Device)
		for _, s := range t.Segments {
			note := ""
			// Gap-fill on the system track is how long nothing was playing,
			// which is information rather than a fault.
			if t.SampleRate > 0 {
				if pad := float64(s.PaddedFrames) / float64(t.SampleRate); pad > 0.05 {
					note += fmt.Sprintf("  %.1fs gap-fill", pad)
				}
			}
			if !s.Complete {
				note += "  incomplete"
			}
			fmt.Printf("      [%02d] %-16s %6.1fs at %6.1fs%s\n",
				s.Index, s.File, s.DurationSeconds, s.StartSeconds, note)
		}
	}
	if len(mute) > 0 && judge {
		fmt.Fprintln(os.Stderr, "")
		for _, t := range mute {
			fmt.Fprintf(os.Stderr, "  WARNING: the %s track peaked at %.1f dBFS, and speech peaks\n", t.Name, t.PeakDBFS())
			fmt.Fprintf(os.Stderr, "  around -6 to -20. Nothing was captured on it. Check that %s is\n", t.Device)
			fmt.Fprintln(os.Stderr, "  unmuted, at usable gain, and the device actually in use.")
		}
	}
	if silent && judge {
		fmt.Fprintln(os.Stderr, "\n  WARNING: a track is silent. Half a meeting was recorded.")
		fmt.Fprintln(os.Stderr, "  For the system track this usually means nothing was playing,")
		fmt.Fprintln(os.Stderr, "  or the meeting is on an output that is not the default endpoint.")
		return 1
	}
	return 0
}
