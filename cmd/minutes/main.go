// Command minutes records a meeting from this desktop — both sides of it.
//
// R1 built the capture path; R2 adds segments, a manifest, and a lifecycle that
// outlives the command that starts it. Transcription, summary and delivery are
// R3 and R4 and are deliberately absent.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"errors"

	"github.com/alexj/minutes/internal/capture"
	"github.com/alexj/minutes/internal/config"
	"github.com/alexj/minutes/internal/deliver"
	"github.com/alexj/minutes/internal/manifest"
	"github.com/alexj/minutes/internal/preflight"
	"github.com/alexj/minutes/internal/session"
	"github.com/alexj/minutes/internal/transcribe"
	"github.com/alexj/minutes/internal/transcript"
)

const defaultRoot = "recordings"

func usage() {
	fmt.Fprint(os.Stderr, `minutes — record both sides of a meeting

  minutes preflight
        Report whether a recording started now would capture both tracks.
        Exits non-zero if it would not.

  minutes start [--name NAME] [--segment 5m] [--root DIR] [--force]
        Begin recording and return. The recording outlives this command.
        Refuses if something is already recording, or if the disk is too full.

  minutes stop [ID] [--root DIR]
        Stop a recording and report what it captured. Defaults to the one
        that is running.

  minutes status [ID] [--root DIR]
        Show a recording's manifest.

  minutes list [--root DIR]
        List recordings, newest first.

  minutes record [--duration D] [--name NAME] [--segment 5m] [--root DIR]
        Record in the foreground until the duration elapses or Ctrl-C.

  minutes transcribe [ID] [--backend B] [--model M] [--root DIR]
        Transcribe a recording, both tracks, merged on the shared clock.
        Runs locally by default; audio leaves this machine only if a hosted
        backend is named.

  minutes deliver [ID] --to PROJECT [--root DIR] [--no-notify]
        Hand the transcript to a session so it can write and file the notes,
        and tell the human. Falls back to writing the brief to disk when the
        agent is unreachable.

Environment:
  MINUTES_HELPER   path to the capture helper (default: ./dist/minutes-capture.exe)
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
	root := fs.String("root", defaultRoot, "where recordings are kept")
	seg := fs.Duration("segment", session.DefaultSegment, "segment length")
	force := fs.Bool("force", false, "start even if another recording is running")
	fs.Parse(args)

	helper := readyToRecord(context.Background(), *root, *force)
	if helper == "" {
		return 1
	}

	m, err := session.Start(session.StartOptions{
		Root: *root, Name: *name, Segment: *seg, Helper: helper,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "could not start recording: %v\n", err)
		return 1
	}

	// Recording is a trust matter, and in some places a legal one. A recording
	// that runs detached is exactly the one that could become quiet, so this
	// says so plainly.
	banner()
	fmt.Printf("  id:       %s\n", m.ID)
	fmt.Printf("  files:    %s\n", m.Dir())
	fmt.Printf("  segments: %s\n\n", seg.String())
	fmt.Printf("  stop with:  minutes stop %s\n", m.ID)
	return 0
}

func banner() {
	fmt.Println("┌──────────────────────────────────────────────┐")
	fmt.Println("│  ● RECORDING — microphone and system audio   │")
	fmt.Println("└──────────────────────────────────────────────┘")
}

func cmdStop(args []string) int {
	fs := flag.NewFlagSet("stop", flag.ExitOnError)
	root := fs.String("root", defaultRoot, "where recordings are kept")
	fs.Parse(args)

	dir, err := session.Resolve(*root, fs.Arg(0))
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
	return report(m, true)
}

func cmdStatus(args []string) int {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	root := fs.String("root", defaultRoot, "where recordings are kept")
	fs.Parse(args)

	dir, err := session.Resolve(*root, fs.Arg(0))
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
	root := fs.String("root", defaultRoot, "where recordings are kept")
	fs.Parse(args)

	all, err := session.List(*root)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		return 1
	}
	if len(all) == 0 {
		fmt.Printf("no recordings under %s\n", *root)
		return 0
	}
	for _, st := range all {
		mark := " "
		if st.Live {
			mark = "●" // an active recording is visible in a plain listing
		}
		fmt.Printf("%s %-28s %-12s %7.1fs  %s\n",
			mark, st.ID, st.StateLabel(), st.Duration(), st.Name)
	}
	return 0
}

func cmdRecord(args []string) int {
	fs := flag.NewFlagSet("record", flag.ExitOnError)
	dur := fs.Duration("duration", 0, "how long to record; 0 means until Ctrl-C")
	name := fs.String("name", "", "what the meeting is")
	root := fs.String("root", defaultRoot, "where recordings are kept")
	seg := fs.Duration("segment", session.DefaultSegment, "segment length")
	force := fs.Bool("force", false, "record even if another recording is running")
	fs.Parse(args)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	helper := readyToRecord(ctx, *root, *force)
	if helper == "" {
		return 1
	}

	id := session.NewID(time.Now(), *name)
	dir := filepath.Join(*root, id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		return 1
	}
	m := manifest.New(dir, id, *name, seg.Seconds())
	if err := m.Save(); err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		return 1
	}

	banner()
	if *dur > 0 {
		fmt.Printf("  for %s, or Ctrl-C to stop early\n", *dur)
	} else {
		fmt.Print("  until Ctrl-C\n")
	}
	fmt.Printf("  files: %s\n\n", dir)

	runErr := capture.Run(ctx, capture.Options{
		Helper: helper, Manifest: m, Duration: *dur,
		Log: func(f string, a ...any) { fmt.Printf("  "+f+"\n", a...) },
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
	return report(m, true)
}

func cmdTranscribe(args []string) int {
	fs := flag.NewFlagSet("transcribe", flag.ExitOnError)
	root := fs.String("root", defaultRoot, "where recordings are kept")
	backend := fs.String("backend", "", "override the configured backend")
	model := fs.String("model", "", "override the configured model")
	fs.Parse(args)

	dir, err := session.Resolve(*root, fs.Arg(0))
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
	opt := cfg.TranscribeOptions(log)
	if *backend != "" {
		opt.Backend = *backend
	}
	if *model != "" {
		opt.Model = *model
	}

	tr, err := transcribe.New(opt)
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

	started := time.Now()
	t, err := transcript.Build(ctx, st.Manifest, tr, log)
	if err != nil {
		fmt.Fprintf(os.Stderr, "transcription failed: %v\n", err)
		return 1
	}
	if err := t.Write(dir); err != nil {
		fmt.Fprintf(os.Stderr, "writing transcript: %v\n", err)
		return 1
	}
	if err := st.Manifest.SetTranscript(manifest.TranscriptRecord{
		Backend:          t.Backend,
		AudioLeftMachine: t.AudioLeftMachine,
		CreatedAt:        t.CreatedAt,
		Lines:            len(t.Lines),
		File:             transcript.JSONName,
	}); err != nil {
		fmt.Fprintf(os.Stderr, "recording the transcript in the manifest: %v\n", err)
		return 1
	}

	var you, others int
	for _, l := range t.Lines {
		if l.Speaker == transcript.SpeakerYou {
			you++
		} else {
			others++
		}
	}
	fmt.Printf("\n  %d lines in %s — %d you, %d everyone else\n",
		len(t.Lines), time.Since(started).Round(time.Second), you, others)
	fmt.Printf("  %s\n", filepath.Join(dir, transcript.TextName))
	return 0
}

func cmdDeliver(args []string) int {
	fs := flag.NewFlagSet("deliver", flag.ExitOnError)
	root := fs.String("root", defaultRoot, "where recordings are kept")
	to := fs.String("to", "", "the project whose session should write the notes")
	from := fs.String("from", "minutes", "who the message is from")
	noNotify := fs.Bool("no-notify", false, "do not send a human notification")
	fs.Parse(args)

	if *to == "" {
		fmt.Fprintln(os.Stderr, "deliver needs --to: which project the notes belong to is a judgment call,")
		fmt.Fprintln(os.Stderr, "and this program deliberately does not make it. Name the project.")
		return 2
	}

	dir, err := session.Resolve(*root, fs.Arg(0))
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		return 1
	}
	m, err := manifest.Load(dir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		return 1
	}
	t, err := transcript.Load(dir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "no transcript in %s: run `minutes transcribe` first (%v)\n", dir, err)
		return 1
	}

	brief := deliver.Brief{Recording: m, Transcript: t}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	client := deliver.New()
	err = client.Send(ctx, deliver.Message{
		To: *to, From: *from, Tag: "minutes",
		Title: brief.Title(), Body: brief.Body(),
	})

	if errors.Is(err, deliver.ErrUnreachable) {
		// Degrade rather than fail. A recorder that loses a meeting because a
		// coordinator blipped would be worse than one that never integrated at
		// all; the notes are on disk either way.
		path := filepath.Join(dir, "delivery.md")
		body := "# " + brief.Title() + "\n\n" + brief.Body()
		if werr := os.WriteFile(path, []byte(body), 0o644); werr != nil {
			fmt.Fprintf(os.Stderr, "agent unreachable, and writing the brief failed too: %v\n", werr)
			return 1
		}
		fmt.Printf("  the shabadoo agent is not reachable, so nothing was delivered.\n")
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
	fmt.Printf("  notes requested from %s\n", *to)

	if !*noNotify {
		if err := client.Notify(ctx, "Meeting recorded", brief.NotifyBody(), "minutes"); err != nil {
			// The message is what matters; the notification is a courtesy.
			fmt.Printf("  (the human notification did not go out: %v)\n", err)
		}
	}
	return 0
}

// cmdSupervise is the detached half of `start`. It is not in the usage text
// because nobody should run it directly.
func cmdSupervise(args []string) int {
	fs := flag.NewFlagSet("supervise", flag.ExitOnError)
	dir := fs.String("dir", "", "recording directory")
	helper := fs.String("helper", "", "capture helper path")
	fs.Parse(args)
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
	runErr := capture.Run(ctx, capture.Options{
		Helper: *helper, Manifest: m,
		Log: func(f string, a ...any) { fmt.Printf(f+"\n", a...) },
	})
	if err := m.Finish(runErr); err != nil {
		fmt.Fprintf(os.Stderr, "writing manifest: %v\n", err)
	}
	// The pid file is removed last: while it exists and names a live process,
	// `stop` has something to signal.
	os.Remove(filepath.Join(*dir, session.PIDFile))
	if runErr != nil {
		fmt.Fprintf(os.Stderr, "recording failed: %v\n", runErr)
		return 1
	}
	fmt.Println("stopped cleanly")
	return 0
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
	for _, t := range m.Tracks {
		peak := fmt.Sprintf("%.1f dBFS", t.PeakDBFS())
		switch {
		case !t.Started():
			peak = "starting"
		case t.Silent():
			peak, silent = "SILENT", true
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
	if silent && judge {
		fmt.Fprintln(os.Stderr, "\n  WARNING: a track is silent. Half a meeting was recorded.")
		fmt.Fprintln(os.Stderr, "  For the system track this usually means nothing was playing,")
		fmt.Fprintln(os.Stderr, "  or the meeting is on an output that is not the default endpoint.")
		return 1
	}
	return 0
}
