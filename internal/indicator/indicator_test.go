package indicator

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// fakeTray writes the given lines and then waits for stdin to close, standing
// in for the Windows tray helper.
func fakeTray(t *testing.T, lines string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "minutes-tray.exe")
	body := "#!/bin/sh\nprintf '%b' \"" + lines + "\"\ncat > /dev/null\n"
	if err := os.WriteFile(p, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// Stopping from the tray is a request to whoever owns the recording, never an
// action the tray takes. A tray that killed the capture helper itself would
// produce the half-written recording the rest of this program exists to avoid.
func TestStopFromTheTrayReachesTheOwner(t *testing.T) {
	var mu sync.Mutex
	stopped := false
	done := make(chan struct{})

	_, err := Start(context.Background(), Options{
		Helper: fakeTray(t, "READY\\nSTOP\\n"),
		Name:   "standup",
		OnStop: func() {
			mu.Lock()
			stopped = true
			mu.Unlock()
			close(done)
		},
	})
	if err != nil {
		t.Fatalf("indicator did not start: %v", err)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the tray asked to stop and nobody was told")
	}
	mu.Lock()
	defer mu.Unlock()
	if !stopped {
		t.Error("OnStop was not called")
	}
}

// The pair: a tray that reports itself ready and asks for nothing must not stop
// the recording. A handler wired to fire on any output would pass the test
// above and silently end every meeting the moment the icon appeared.
func TestATrayThatOnlyReportsReadyStopsNothing(t *testing.T) {
	var mu sync.Mutex
	calls := 0

	ind, err := Start(context.Background(), Options{
		Helper: fakeTray(t, "READY\\n"),
		OnStop: func() { mu.Lock(); calls++; mu.Unlock() },
	})
	if err != nil {
		t.Fatalf("indicator did not start: %v", err)
	}
	time.Sleep(300 * time.Millisecond)
	ind.Stop()

	mu.Lock()
	defer mu.Unlock()
	if calls != 0 {
		t.Errorf("OnStop fired %d time(s) on a tray that only said READY", calls)
	}
}

// A platform with no tray helper records without one. Refusing to record
// because an icon would not draw protects the wrong thing.
func TestNoTrayHelperIsNotAnError(t *testing.T) {
	ind, err := Start(context.Background(), Options{Helper: ""})
	if err != nil {
		t.Fatalf("no helper was treated as an error: %v", err)
	}
	// A nil indicator must be safe to stop, so a caller never has to ask
	// whether it came up.
	ind.Stop()
}

func TestFindHelperLooksBesideTheCaptureHelper(t *testing.T) {
	dir := t.TempDir()
	capture := filepath.Join(dir, "minutes-capture.exe")
	if err := os.WriteFile(capture, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := FindHelper(capture); got != "" {
		t.Errorf("FindHelper = %q with no tray beside it", got)
	}
	tray := filepath.Join(dir, "minutes-tray.exe")
	if err := os.WriteFile(tray, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := FindHelper(capture); got != tray {
		t.Errorf("FindHelper = %q, want %q", got, tray)
	}
}
