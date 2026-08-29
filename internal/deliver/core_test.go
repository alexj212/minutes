package deliver

import (
	"os"
	"path/filepath"
	"testing"
)

func shabadooDir(t *testing.T, dirs ...string) string {
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
	// A node directory is a core session's working directory and holds that
	// session's CLAUDE.md. Building them without it made every fixture here a
	// node, which is why none of these tests noticed when a directory that was
	// not one appeared beside them.
	for _, d := range dirs {
		if err := os.MkdirAll(filepath.Join(base, d), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(base, d, "CLAUDE.md"), []byte("# node"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return home
}

// stateDir makes a directory shabadoo keeps for its own purposes, which is not
// a node however much it looks like one from a listing.
func stateDir(t *testing.T, home, name string) {
	t.Helper()
	d := filepath.Join(home, ".config", "shabadoo", name)
	if err := os.MkdirAll(d, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d, "834150.14732792"), []byte("token"), 0o600); err != nil {
		t.Fatal(err)
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

// Counting directories broke four days after it was written, when shabadoo
// created an mcp/ state directory beside the node directories. The count went
// from one to two and CoreSession silently returned nothing: no failure, no
// message, and automatic delivery with nowhere to go.
//
// None of the tests above could have caught it, because the fixture only ever
// built node directories — every directory it made was one, so "is this a node"
// and "is this a directory" were the same question. A fixture cannot tell you
// it is the wrong fixture.
func TestAStateDirectoryIsNotANode(t *testing.T) {
	home := shabadooDir(t, "wsl")
	stateDir(t, home, "mcp")
	if got := CoreSession(); got != "wsl" {
		t.Errorf("CoreSession = %q, want \"wsl\" — a state directory is not a node", got)
	}
}
