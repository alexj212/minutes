// Command minutes records a meeting from this desktop — both sides of it.
//
// R1 covers Windows capture only. What is here is the capture path and the
// preflight that guards it; segmentation, transcription, summary and delivery
// are later phases and are deliberately absent.
package main

import (
	"context"
	"fmt"
	"math"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/alexj/minutes/internal/capture"
	"github.com/alexj/minutes/internal/preflight"
)

func usage() {
	fmt.Fprint(os.Stderr, `minutes — record both sides of a meeting

  minutes preflight
        Report whether a recording started now would capture both tracks.
        Exits non-zero if it would not.

  minutes record [--duration D] [--out DIR] [--prefix NAME]
        Record until the duration elapses or Ctrl-C. Refuses to start if
        preflight refuses.

Environment:
  MINUTES_HELPER   path to the capture helper (default: ./dist/minutes-capture.exe)
`)
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}

	switch os.Args[1] {
	case "preflight":
		os.Exit(cmdPreflight())
	case "record":
		os.Exit(cmdRecord(os.Args[2:]))
	case "-h", "--help", "help":
		usage()
		os.Exit(0)
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n\n", os.Args[1])
		usage()
		os.Exit(2)
	}
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

func cmdRecord(args []string) int {
	duration := 0 * time.Second
	outDir := "recordings"
	prefix := ""

	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--duration":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "--duration needs a value, e.g. 30s")
				return 2
			}
			d, err := time.ParseDuration(args[i+1])
			if err != nil {
				fmt.Fprintf(os.Stderr, "bad --duration: %v\n", err)
				return 2
			}
			duration = d
			i++
		case "--out":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "--out needs a directory")
				return 2
			}
			outDir = args[i+1]
			i++
		case "--prefix":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "--prefix needs a value")
				return 2
			}
			prefix = args[i+1]
			i++
		default:
			fmt.Fprintf(os.Stderr, "unknown flag: %s\n", args[i])
			return 2
		}
	}
	if prefix == "" {
		prefix = time.Now().Format("2006-01-02-150405")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	res, err := preflight.Run(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "preflight failed: %v\n", err)
		return 1
	}
	if !res.CanRecord {
		fmt.Fprint(os.Stderr, res.Describe())
		return 1
	}

	// Recording is a trust matter, and in some places a legal one. An active
	// recording should be obvious rather than quiet.
	fmt.Println("┌──────────────────────────────────────────────┐")
	fmt.Println("│  ● RECORDING — microphone and system audio   │")
	fmt.Println("└──────────────────────────────────────────────┘")
	if duration > 0 {
		fmt.Printf("  for %s, or Ctrl-C to stop early\n\n", duration)
	} else {
		fmt.Print("  until Ctrl-C\n\n")
	}

	sum, err := capture.Run(ctx, capture.Options{
		Helper:   res.HelperPath,
		OutDir:   outDir,
		Prefix:   prefix,
		Duration: duration,
		Log:      func(f string, a ...any) { fmt.Printf("  "+f+"\n", a...) },
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "\nrecording failed: %v\n", err)
		return 1
	}

	fmt.Println("\n  ○ stopped")
	fmt.Println()

	silent := false
	for _, t := range sum.Tracks {
		peak := fmt.Sprintf("%.1f dBFS", t.PeakDBFS)
		if math.IsInf(t.PeakDBFS, -1) || t.Silent() {
			peak = "SILENT"
		}
		fmt.Printf("  %-7s %7.3fs  %5d packets  peak %-11s %s\n",
			t.Track, t.Duration, t.Packets, peak, t.Path)
		if t.PaddedSeconds > 0.05 {
			fmt.Printf("          %.3fs of that is gap-fill: nothing was playing then\n", t.PaddedSeconds)
		}
		if t.Discontinuity > 0 {
			fmt.Printf("          %d discontinuities reported by the device\n", t.Discontinuity)
		}
		if t.Silent() {
			silent = true
		}
	}

	if silent {
		fmt.Fprintln(os.Stderr, "\n  WARNING: a track is silent. Half a meeting was recorded.")
		fmt.Fprintln(os.Stderr, "  For the system track this usually means nothing was playing,")
		fmt.Fprintln(os.Stderr, "  or the meeting is on an output that is not the default endpoint.")
		return 1
	}
	return 0
}
