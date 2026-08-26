package deliver

import (
	"os"
	"path/filepath"
)

// Finding this node's own session.
//
// A recording has to go somewhere, and the honest default is the session that
// belongs to this machine: it is always there, it is driven by the person who
// started the recording, and delivering to it keeps the transcript on the
// machine that made it. Which project a meeting's notes belong to is still a
// judgment — it is just made by a session with a person behind it, rather than
// by whoever remembered to type a project name.
//
// That distinction is what makes an automatic delivery safe. Sending to this
// node's core session is not publishing; sending to another project is.
//
// The agent socket cannot answer "who is core here" — its allowlist carries the
// messaging endpoints and nothing else — so this reads the node directory
// shabadoo keeps beside the socket. Exactly one means the answer is unambiguous.
// Anything else and there is no default worth guessing at.
func CoreSession() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	entries, err := os.ReadDir(filepath.Join(home, ".config", "shabadoo"))
	if err != nil {
		return ""
	}
	var found string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if found != "" {
			// More than one node configured here. Guessing which is ours would
			// mean guessing where a meeting goes.
			return ""
		}
		found = e.Name()
	}
	return found
}
