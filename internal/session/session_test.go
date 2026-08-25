package session

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/alexj/minutes/internal/manifest"
)

func TestNewIDSortsChronologicallyAndSlugsNames(t *testing.T) {
	at := time.Date(2026, 8, 25, 10, 15, 30, 0, time.UTC)
	if got, want := NewID(at, ""), "2026-08-25-101530"; got != want {
		t.Errorf("NewID = %q, want %q", got, want)
	}
	if got, want := NewID(at, "Weekly Standup!"), "2026-08-25-101530-weekly-standup"; got != want {
		t.Errorf("NewID = %q, want %q", got, want)
	}
	earlier := NewID(at.Add(-time.Hour), "b")
	later := NewID(at, "a")
	if !(earlier < later) {
		t.Errorf("ids do not sort chronologically: %q should precede %q", earlier, later)
	}
}

// write a recording directory with a given state and pid.
func fixture(t *testing.T, root, id string, state manifest.State, pid string) string {
	t.Helper()
	dir := filepath.Join(root, id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	m := manifest.New(dir, id, "", 300)
	m.State = state
	if err := m.Save(); err != nil {
		t.Fatal(err)
	}
	if pid != "" {
		if err := os.WriteFile(filepath.Join(dir, PIDFile), []byte(pid), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// A manifest that says "recording" with no live supervisor was interrupted.
// Trusting the file alone would report a meeting as still being captured hours
// after the machine that was capturing it went away.
func TestInterruptedRecordingIsDetected(t *testing.T) {
	root := t.TempDir()
	// PID 1 exists but is not our supervisor; use an unlikely-to-exist one.
	dir := fixture(t, root, "2026-08-25-101530", manifest.StateRecording, "4194304")

	st, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if st.Live {
		t.Fatal("a dead pid was reported live")
	}
	if !st.Interrupted() {
		t.Error("a recording state with no supervisor was not reported interrupted")
	}
	if got := st.StateLabel(); got != "interrupted" {
		t.Errorf("StateLabel = %q, want \"interrupted\"", got)
	}
}

func TestStoppedRecordingIsNotInterrupted(t *testing.T) {
	root := t.TempDir()
	dir := fixture(t, root, "2026-08-25-101530", manifest.StateStopped, "")
	st, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if st.Interrupted() {
		t.Error("a cleanly stopped recording was reported interrupted")
	}
	if got := st.StateLabel(); got != "stopped" {
		t.Errorf("StateLabel = %q, want \"stopped\"", got)
	}
}

// A live supervisor is one whose process actually exists. This process does.
func TestLiveSupervisorIsDetected(t *testing.T) {
	root := t.TempDir()
	dir := fixture(t, root, "2026-08-25-101530", manifest.StateRecording, "")
	if err := os.WriteFile(filepath.Join(dir, PIDFile), []byte(itoa(os.Getpid())), 0o644); err != nil {
		t.Fatal(err)
	}
	st, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !st.Live {
		t.Fatal("a running process was not reported live")
	}
	if st.Interrupted() {
		t.Error("a live recording was reported interrupted")
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}

func TestListIsNewestFirstAndSkipsRubbish(t *testing.T) {
	root := t.TempDir()
	older := fixture(t, root, "2026-08-25-100000", manifest.StateStopped, "")
	newer := fixture(t, root, "2026-08-25-110000", manifest.StateStopped, "")

	// Bump the newer one's start time; New() stamps time.Now() for both.
	m, _ := manifest.Load(newer)
	m.StartedAt = time.Now().Add(time.Hour)
	if err := m.Save(); err != nil {
		t.Fatal(err)
	}
	// A directory with no manifest must not break the listing.
	if err := os.MkdirAll(filepath.Join(root, "not-a-recording"), 0o755); err != nil {
		t.Fatal(err)
	}

	all, err := List(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Fatalf("listed %d recordings, want 2 (the directory without a manifest must be skipped)", len(all))
	}
	if all[0].Dir() != newer || all[1].Dir() != older {
		t.Errorf("listing is not newest first: %s then %s", all[0].ID, all[1].ID)
	}
}

func TestListOfMissingRootIsEmptyNotAnError(t *testing.T) {
	all, err := List(filepath.Join(t.TempDir(), "nope"))
	if err != nil {
		t.Fatalf("listing a missing root errored: %v", err)
	}
	if len(all) != 0 {
		t.Errorf("got %d recordings from a missing root", len(all))
	}
}

// "stop" almost always means "stop what is recording", so a bare reference must
// prefer a live recording over a more recent finished one.
func TestResolvePrefersTheLiveRecording(t *testing.T) {
	root := t.TempDir()
	live := fixture(t, root, "2026-08-25-100000", manifest.StateRecording, itoa(os.Getpid()))
	newer := fixture(t, root, "2026-08-25-110000", manifest.StateStopped, "")
	m, _ := manifest.Load(newer)
	m.StartedAt = time.Now().Add(time.Hour)
	if err := m.Save(); err != nil {
		t.Fatal(err)
	}

	got, err := Resolve(root, "")
	if err != nil {
		t.Fatal(err)
	}
	if got != live {
		t.Errorf("Resolve picked %s, want the live recording %s", got, live)
	}
}

func TestResolveNamesAndFailures(t *testing.T) {
	root := t.TempDir()
	dir := fixture(t, root, "2026-08-25-100000", manifest.StateStopped, "")
	got, err := Resolve(root, "2026-08-25-100000")
	if err != nil {
		t.Fatal(err)
	}
	if got != dir {
		t.Errorf("Resolve by id gave %s, want %s", got, dir)
	}
	if _, err := Resolve(root, "no-such-recording"); err == nil {
		t.Error("resolving an unknown id succeeded")
	}
	if _, err := Resolve(t.TempDir(), ""); err == nil {
		t.Error("resolving in an empty root succeeded")
	}
}

func TestStopWithoutASupervisorRefuses(t *testing.T) {
	root := t.TempDir()
	dir := fixture(t, root, "2026-08-25-100000", manifest.StateStopped, "")
	if _, err := Stop(dir, time.Second); err != ErrNotRecording {
		t.Errorf("Stop returned %v, want ErrNotRecording", err)
	}
}
