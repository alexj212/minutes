package deliver

import (
	"io"
	"net"
	"net/http"
	"testing"
)

// fakeWhoami serves the one route CoreSession asks for.
func fakeWhoami(t *testing.T, status int, body string) {
	t.Helper()
	path := shortSocketPath(t)
	mux := http.NewServeMux()
	mux.HandleFunc("/whoami", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		io.WriteString(w, body)
	})
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("listening on %s: %v", path, err)
	}
	srv := &http.Server{Handler: mux}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close(); ln.Close() })
	t.Setenv("SHABADOO_SOCKET", path)
}

// The agent names this machine's own session, which is the safe default
// destination: delivering there keeps the transcript on the machine that made
// it.
//
// This used to be inferred from shabadoo's on-disk layout, and the layout was
// never an interface. The first version identified the node directory by being
// the only one beside the socket, and broke four days later when an unrelated
// mcp/ directory appeared next to it.
func TestCoreSessionComesFromTheAgent(t *testing.T) {
	fakeWhoami(t, http.StatusOK, `{"node":"wsl","core_session":"claude-wsl-1df88a1a",
		"core_path":"/home/alexj/.config/shabadoo/wsl"}`)
	if got := CoreSession(); got != "claude-wsl-1df88a1a" {
		t.Errorf("CoreSession = %q, want the id the agent gave", got)
	}
}

// Absent and empty are different answers, and the agent distinguishes them
// deliberately: the field is omitted when it cannot determine a core session,
// which is not the same as this host having none.
//
// Asserted against the answering case above rather than alone — a function that
// always returns empty passes any test that only knows this one, and returning
// empty is exactly what the broken version did.
func TestAnAgentThatCannotSayGivesNoDefault(t *testing.T) {
	fakeWhoami(t, http.StatusOK, `{"node":"wsl","core_path":"/home/alexj/.config/shabadoo/wsl"}`)
	if got := CoreSession(); got != "" {
		t.Errorf("CoreSession = %q; the agent did not name one", got)
	}
}

func TestNoAgentGivesNoDefault(t *testing.T) {
	t.Setenv("SHABADOO_SOCKET", shortSocketPath(t))
	if got := CoreSession(); got != "" {
		t.Errorf("CoreSession = %q, want empty with no agent listening", got)
	}
}

// A refusal or an error page is not a name.
func TestAFailedWhoamiGivesNoDefault(t *testing.T) {
	fakeWhoami(t, http.StatusInternalServerError, `{"core_session":"claude-wsl-1df88a1a"}`)
	if got := CoreSession(); got != "" {
		t.Errorf("CoreSession = %q; the agent returned an error", got)
	}
}
