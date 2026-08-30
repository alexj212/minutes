package session

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/alexj212/minutes/internal/manifest"
)

// The recording directory is created after the check runs, so a check that
// failed on a missing directory would never run at all.
func TestFreeBytesWalksUpToAnExistingAncestor(t *testing.T) {
	root := t.TempDir()
	deep := filepath.Join(root, "not", "created", "yet")
	free, err := FreeBytes(deep)
	if err != nil {
		t.Fatalf("checking a directory that does not exist yet failed: %v", err)
	}
	if free == 0 {
		t.Error("reported zero free bytes")
	}
}

func TestHeadroomThresholds(t *testing.T) {
	const rate = 368400 // both tracks, 16-bit stereo at 48k and 44.1k

	cases := []struct {
		name         string
		seconds      float64
		refuse, warn bool
	}{
		{"ten minutes", 600, true, true},
		{"half an hour", 1800, false, true},
		{"one hour", 3600, false, true},
		{"three hours", 3 * 3600, false, false},
	}
	for _, c := range cases {
		h := Headroom{Seconds: c.seconds, BytesPerSecond: rate,
			FreeBytes: uint64(c.seconds * rate)}
		if h.Refuse() != c.refuse {
			t.Errorf("%s: Refuse() = %v, want %v", c.name, h.Refuse(), c.refuse)
		}
		if h.Warn() != c.warn {
			t.Errorf("%s: Warn() = %v, want %v", c.name, h.Warn(), c.warn)
		}
	}
}

// Refusing has to be the strictly stronger condition, or a disk could be too
// full to start on without ever having been warned about.
func TestRefuseImpliesWarn(t *testing.T) {
	for s := 0.0; s < 4*3600; s += 60 {
		h := Headroom{Seconds: s}
		if h.Refuse() && !h.Warn() {
			t.Fatalf("at %.0fs the disk is refused but not warned about", s)
		}
	}
}

func TestEstimateHeadroom(t *testing.T) {
	dir := t.TempDir()
	h, err := EstimateHeadroom(dir, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if h.BytesPerSecond != 1000 {
		t.Errorf("BytesPerSecond = %d, want 1000", h.BytesPerSecond)
	}
	if want := float64(h.FreeBytes) / 1000; h.Seconds != want {
		t.Errorf("Seconds = %v, want %v", h.Seconds, want)
	}
	if h.String() == "" {
		t.Error("no description")
	}
}

func TestEstimateHeadroomRejectsNonsenseRate(t *testing.T) {
	if _, err := EstimateHeadroom(t.TempDir(), 0); err == nil {
		t.Error("accepted a zero capture rate, which would divide by zero")
	}
}

// Starting a second recording captures the same meeting twice and makes a bare
// stop ambiguous, so the live ones have to be findable.
func TestLiveReturnsOnlyRunningRecordings(t *testing.T) {
	root := t.TempDir()
	fixture(t, root, "2026-08-25-100000", manifest.StateStopped, "")
	fixture(t, root, "2026-08-25-110000", manifest.StateRecording, "4194304") // dead pid
	running := fixture(t, root, "2026-08-25-120000", manifest.StateRecording, itoa(os.Getpid()))

	live, err := Live(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(live) != 1 {
		var got []string
		for _, s := range live {
			got = append(got, s.ID)
		}
		t.Fatalf("Live returned %v, want only the one with a running process", got)
	}
	if live[0].Dir() != running {
		t.Errorf("Live returned %s, want %s", live[0].Dir(), running)
	}
}

func TestLiveOfEmptyRootIsEmpty(t *testing.T) {
	live, err := Live(filepath.Join(t.TempDir(), "nope"))
	if err != nil {
		t.Fatalf("listing a missing root errored: %v", err)
	}
	if len(live) != 0 {
		t.Errorf("got %d live recordings from a missing root", len(live))
	}
}

func TestDirSizeCountsEverything(t *testing.T) {
	dir := t.TempDir()
	for name, size := range map[string]int{"a.wav": 1000, "b.wav": 2000, "manifest.json": 50} {
		if err := os.WriteFile(filepath.Join(dir, name), make([]byte, size), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got, err := DirSize(dir)
	if err != nil {
		t.Fatal(err)
	}
	if want := int64(3050); got != want {
		t.Errorf("DirSize = %d, want %d", got, want)
	}
}

func TestHumanBytes(t *testing.T) {
	cases := map[int64]string{
		512: "512B", 2100: "2.1kB", 27_100_000: "27.1MB", 1_330_000_000: "1.3GB",
	}
	for in, want := range cases {
		if got := HumanBytes(in); got != want {
			t.Errorf("HumanBytes(%d) = %q, want %q", in, got, want)
		}
	}
}

// A delivered message names the paths of the recording it describes, and is
// read later — sometimes much later, since mail waits for a session to start.
// Delivering from somewhere that gets cleared produces a message pointing at
// nothing, which is indistinguishable from a typo.
//
// Learned by doing it: a recording under /tmp was delivered and then deleted.
func TestVolatileDirectoriesAreRecognised(t *testing.T) {
	cases := map[string]bool{
		"/tmp/route/2026-08-25-000000":      true,
		"/tmp":                              true,
		"/var/tmp/thing":                    true,
		"/dev/shm/thing":                    true,
		"/home/you/minutes/2026-08-25":      false,
		"/c/projects/minutes/recordings/x":  false,
		"/home/you/tmp/not-really-volatile": false,
	}
	for dir, want := range cases {
		if got := IsVolatile(dir); got != want {
			t.Errorf("IsVolatile(%q) = %v, want %v", dir, got, want)
		}
	}
}

// A directory merely named like a temporary one is not one. "/tmpfoo" is not
// inside "/tmp".
func TestSimilarlyNamedDirectoriesAreNotVolatile(t *testing.T) {
	if IsVolatile("/tmpfoo/recordings") {
		t.Error("/tmpfoo was treated as being inside /tmp")
	}
}

func TestTMPDIRIsHonoured(t *testing.T) {
	t.Setenv("TMPDIR", "/scratch/mytmp")
	if !IsVolatile("/scratch/mytmp/recording") {
		t.Error("a directory under $TMPDIR was not recognised as volatile")
	}
}
