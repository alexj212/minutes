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
// shabadoo keeps beside the socket.
//
// Counting directories is what this used to do, and it broke four days after it
// was written: shabadoo created an unrelated `mcp/` state directory beside the
// node directories, the count went from one to two, and CoreSession silently
// began returning nothing. Nothing failed. Automatic delivery just had no
// destination any more, which is the quiet kind of break — the recording is
// still made and still stored, and the notes simply never go anywhere.
//
// So a node directory is now identified by what makes it one rather than by
// being the only one: it is a core session's working directory, and it holds
// that session's CLAUDE.md. This is still inference about somebody else's
// layout, and it is still the wrong way to answer the question. The right way
// is a /whoami on the agent socket, which has been asked for. Until that
// exists, being specific is at least better than counting.
func CoreSession() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	root := filepath.Join(home, ".config", "shabadoo")
	entries, err := os.ReadDir(root)
	if err != nil {
		return ""
	}
	var found string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if _, err := os.Stat(filepath.Join(root, e.Name(), "CLAUDE.md")); err != nil {
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
