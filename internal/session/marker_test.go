package session

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func markerAt(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "recording")
	t.Setenv("MINUTES_MARKER", p)
	return p
}

// The marker exists so a shell prompt, a status bar or a person with `cat` can
// tell that this machine is recording without running a command to ask.
func TestMarkerRoundTrip(t *testing.T) {
	markerAt(t)
	if _, ok := ReadMarker(); ok {
		t.Fatal("reported a recording before one started")
	}
	want := Marker{ID: "2026-08-25-100000-standup", Name: "standup",
		Dir: "/somewhere", PID: os.Getpid(), StartedAt: time.Now()}
	if err := SetMarker(want); err != nil {
		t.Fatal(err)
	}
	got, ok := ReadMarker()
	if !ok {
		t.Fatal("no marker after setting one")
	}
	if got.ID != want.ID || got.Name != want.Name || got.Dir != want.Dir {
		t.Errorf("marker round trip lost data: %+v", got)
	}
	if err := ClearMarker(); err != nil {
		t.Fatal(err)
	}
	if _, ok := ReadMarker(); ok {
		t.Error("marker survived being cleared; the machine would claim to be recording forever")
	}
}

// A recorder killed without cleaning up must not leave the machine permanently
// claiming to record. A warning that is always on is a warning nobody reads.
func TestMarkerFromADeadProcessIsIgnoredAndRemoved(t *testing.T) {
	path := markerAt(t)

	child := exec.Command("sh", "-c", "exit 0")
	if err := child.Run(); err != nil {
		t.Fatal(err)
	}
	dead := child.Process.Pid

	if err := SetMarker(Marker{ID: "x", Dir: "/somewhere", PID: dead, StartedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if _, ok := ReadMarker(); ok {
		t.Error("a marker from a dead process was reported as a live recording")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("the stale marker was left on disk")
	}
}

// A half-written marker must not read as "not recording", which is why it is
// renamed into place rather than written directly.
func TestMarkerIsWrittenAtomically(t *testing.T) {
	path := markerAt(t)
	if err := SetMarker(Marker{ID: "x", PID: os.Getpid(), StartedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path + ".tmp"); err == nil {
		t.Error("the temporary file was left behind, so it was not renamed into place")
	}
}

func TestCorruptMarkerIsDiscarded(t *testing.T) {
	path := markerAt(t)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, ok := ReadMarker(); ok {
		t.Error("a corrupt marker was reported as a live recording")
	}
}

func TestClearingAnAbsentMarkerIsNotAnError(t *testing.T) {
	markerAt(t)
	if err := ClearMarker(); err != nil {
		t.Errorf("clearing an absent marker errored: %v", err)
	}
}
