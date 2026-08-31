package main

import (
	"flag"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/alexj212/minutes/internal/manifest"
	"github.com/alexj212/minutes/internal/session"
)

// Go's flag package stops parsing at the first non-flag argument, so
// `minutes transcribe <id> --model small` would take "--model" and "small" as
// two more recording ids and quietly transcribe with the default model. Every
// command here is that shape: an optional id followed by flags.
func TestFlagsAfterPositionalsAreParsed(t *testing.T) {
	cases := []struct {
		name      string
		args      []string
		wantModel string
		wantForce bool
		wantIDs   []string
	}{
		{
			name:      "flags before the id, which always worked",
			args:      []string{"--model", "small", "--force", "rec-1"},
			wantModel: "small", wantForce: true, wantIDs: []string{"rec-1"},
		},
		{
			name:      "flags after the id, which silently did not",
			args:      []string{"rec-1", "--model", "small", "--force"},
			wantModel: "small", wantForce: true, wantIDs: []string{"rec-1"},
		},
		{
			name:      "interleaved",
			args:      []string{"--model", "small", "rec-1", "--force", "rec-2"},
			wantModel: "small", wantForce: true, wantIDs: []string{"rec-1", "rec-2"},
		},
		{
			name:      "no positionals",
			args:      []string{"--model", "medium"},
			wantModel: "medium", wantIDs: nil,
		},
		{
			name:    "no flags",
			args:    []string{"rec-1", "rec-2"},
			wantIDs: []string{"rec-1", "rec-2"},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fs := flag.NewFlagSet("test", flag.ContinueOnError)
			model := fs.String("model", "", "")
			force := fs.Bool("force", false, "")

			ids := parseFlags(fs, c.args)

			if *model != c.wantModel {
				t.Errorf("model = %q, want %q", *model, c.wantModel)
			}
			if *force != c.wantForce {
				t.Errorf("force = %v, want %v", *force, c.wantForce)
			}
			if !reflect.DeepEqual(ids, c.wantIDs) {
				t.Errorf("ids = %v, want %v", ids, c.wantIDs)
			}
		})
	}
}

func TestFirstReturnsEmptyForNoIDs(t *testing.T) {
	if got := first(nil); got != "" {
		t.Errorf("first(nil) = %q, want empty", got)
	}
	if got := first([]string{"a", "b"}); got != "a" {
		t.Errorf("first = %q, want \"a\"", got)
	}
}

// announce must write the pid file, and the session-level test cannot see that.
//
// The first version of this fix shipped with a passing test that would also
// have passed with the bug still in place: it asserted what PID() and
// Interrupted() do given a pid file, not that anything creates one. The defect
// was never in those functions.
//
// So this asserts the write itself. MINUTES_MARKER is redirected because
// announce writes the machine-wide recording marker, and clobbering that during
// a real meeting is a worse bug than the one being fixed.
func TestAnnounceWritesThePidFileListLooksFor(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("MINUTES_MARKER", filepath.Join(t.TempDir(), "marker.json"))
	t.Setenv("SHABADOO_SOCKET", filepath.Join(t.TempDir(), "absent.sock"))

	m := manifest.New(dir, "2026-01-01-000000-test", "test", 300)
	announce(m)
	t.Cleanup(func() { unannounce(m) })

	body, err := os.ReadFile(filepath.Join(m.Dir(), session.PIDFile))
	if err != nil {
		t.Fatalf("announce wrote no pid file: %v\n"+
			"`minutes list` reads this to tell a live recording from an interrupted "+
			"one, and without it a meeting in progress reports as interrupted", err)
	}
	if got := strings.TrimSpace(string(body)); got != strconv.Itoa(os.Getpid()) {
		t.Errorf("pid file says %q, want %d", got, os.Getpid())
	}

	// And it is cleaned up, or the next look at a finished recording finds a
	// pid file naming a process that has gone.
	unannounce(m)
	if _, err := os.Stat(filepath.Join(m.Dir(), session.PIDFile)); !os.IsNotExist(err) {
		t.Errorf("unannounce left the pid file behind: %v", err)
	}
}
