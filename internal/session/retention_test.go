package session

import (
	"os"
	"testing"
	"time"

	"github.com/alexj212/minutes/internal/manifest"
)

func statusAt(t *testing.T, root, id string, age time.Duration, delivered bool, pid string) Status {
	t.Helper()
	dir := fixture(t, root, id, manifest.StateStopped, pid)
	m, err := manifest.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	m.StartedAt = time.Now().Add(-age)
	if delivered {
		m.Delivery = &manifest.DeliveryRecord{To: "homelab", At: time.Now().Add(-age)}
	}
	if err := m.Save(); err != nil {
		t.Fatal(err)
	}
	st, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

// Off unless configured. Deleting somebody's meetings without being asked is
// worse than using their disk.
func TestNoPolicyRemovesNothing(t *testing.T) {
	root := t.TempDir()
	all := []Status{statusAt(t, root, "2020-01-01-000000", 5*365*24*time.Hour, true, "")}
	var r Retention
	if r.Enabled() {
		t.Error("an empty policy reports itself enabled")
	}
	if doomed, _ := r.Doomed(all, time.Now()); len(doomed) != 0 {
		t.Errorf("an empty policy selected %d recordings", len(doomed))
	}
}

func TestKeepDaysSelectsByAge(t *testing.T) {
	root := t.TempDir()
	all := []Status{
		statusAt(t, root, "2026-08-25-000000", 1*24*time.Hour, true, ""),
		statusAt(t, root, "2026-01-01-000000", 100*24*time.Hour, true, ""),
	}
	doomed, _ := Retention{KeepDays: 90}.Doomed(all, time.Now())
	if len(doomed) != 1 {
		t.Fatalf("selected %d, want 1", len(doomed))
	}
	if doomed[0].ID != "2026-01-01-000000" {
		t.Errorf("selected %s, want the old one", doomed[0].ID)
	}
}

func TestKeepCountKeepsTheNewest(t *testing.T) {
	root := t.TempDir()
	// Doomed takes them newest first, as List returns them.
	all := []Status{
		statusAt(t, root, "2026-08-25-000000", 1*time.Hour, true, ""),
		statusAt(t, root, "2026-08-24-000000", 25*time.Hour, true, ""),
		statusAt(t, root, "2026-08-23-000000", 49*time.Hour, true, ""),
	}
	doomed, _ := Retention{KeepCount: 2}.Doomed(all, time.Now())
	if len(doomed) != 1 || doomed[0].ID != "2026-08-23-000000" {
		t.Fatalf("selected %+v, want only the oldest", doomed)
	}
}

// An undelivered recording is the only record of a meeting nobody has read.
func TestUndeliveredIsSparedByDefault(t *testing.T) {
	root := t.TempDir()
	all := []Status{statusAt(t, root, "2026-01-01-000000", 100*24*time.Hour, false, "")}

	doomed, spared := Retention{KeepDays: 90, KeepUndelivered: true}.Doomed(all, time.Now())
	if len(doomed) != 0 {
		t.Error("removed a recording whose notes were never delivered")
	}
	if len(spared) != 1 || spared[0].Reason == "" {
		t.Fatalf("spared %+v, want one with a reason", spared)
	}

	// And removed when that protection is turned off.
	doomed, _ = Retention{KeepDays: 90}.Doomed(all, time.Now())
	if len(doomed) != 1 {
		t.Error("keepUndelivered:false did not allow removal")
	}
}

// A recording still being written to, or still being transcribed, is not old.
// It is in use, whatever its timestamp says.
func TestLiveRecordingIsNeverRemoved(t *testing.T) {
	root := t.TempDir()
	all := []Status{statusAt(t, root, "2026-01-01-000000", 100*24*time.Hour, true, itoa(os.Getpid()))}
	doomed, spared := Retention{KeepDays: 90}.Doomed(all, time.Now())
	if len(doomed) != 0 {
		t.Fatal("selected a recording that is still running")
	}
	if len(spared) != 1 || spared[0].Reason != "still running" {
		t.Errorf("spared %+v, want it noted as still running", spared)
	}
}
