package deliver

import (
	"os"
	"path/filepath"
	"testing"
)

func shabadooDir(t *testing.T, dirs ...string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	base := filepath.Join(home, ".config", "shabadoo")
	if err := os.MkdirAll(base, 0o755); err != nil {
		t.Fatal(err)
	}
	// The files shabadoo keeps beside the node directory must not be mistaken
	// for one.
	for _, f := range []string{"agent.sock", "coord", "hub.db", "agent_key"} {
		if err := os.WriteFile(filepath.Join(base, f), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, d := range dirs {
		if err := os.MkdirAll(filepath.Join(base, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

// The node directory beside the socket names this machine's own session, which
// is the safe default destination: delivering there keeps the transcript on the
// machine that made it.
func TestCoreSessionIsTheNodeDirectory(t *testing.T) {
	shabadooDir(t, "wsl")
	if got := CoreSession(); got != "wsl" {
		t.Errorf("CoreSession = %q, want \"wsl\"", got)
	}
}

// Two nodes configured here means guessing which is ours, and guessing which is
// ours means guessing where a meeting goes.
func TestAmbiguousNodeGivesNoDefault(t *testing.T) {
	shabadooDir(t, "wsl", "mac")
	if got := CoreSession(); got != "" {
		t.Errorf("CoreSession = %q, want empty when the node is ambiguous", got)
	}
}

func TestNoShabadooGivesNoDefault(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if got := CoreSession(); got != "" {
		t.Errorf("CoreSession = %q, want empty with no shabadoo config", got)
	}
}

func TestFilesAreNotMistakenForANode(t *testing.T) {
	shabadooDir(t)
	if got := CoreSession(); got != "" {
		t.Errorf("CoreSession = %q; the files beside the socket are not nodes", got)
	}
}
