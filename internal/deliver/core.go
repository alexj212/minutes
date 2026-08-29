package deliver

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"time"
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
// This used to infer the answer from shabadoo's on-disk layout: the node
// directory beside the socket, identified first by being the only one and then
// by holding a CLAUDE.md. Both were wrong in the same way. The layout was never
// an interface, and the first version broke four days after it was written when
// an unrelated `mcp/` directory appeared beside the node directories — the
// count went from one to two and this silently began returning nothing, with no
// error and automatic delivery quietly addressed nowhere.
//
// The agent socket now answers the question directly. Asking is not merely
// tidier: the id it returns is derived through the same window name the
// launcher injects, rather than computed a second way, and two derivations of
// an address drift into mail addressed to a session that does not exist.

// whoami is the part of the agent's answer this program needs.
//
// CoreSession is a pointer because absent and empty are different answers. The
// field is omitted when the agent cannot determine a core session, and present
// but empty would mean this host has none — collapsing those is exactly how the
// delivery default went quietly nowhere the first time.
type whoami struct {
	Node        string  `json:"node"`
	CoreSession *string `json:"core_session"`
	CorePath    string  `json:"core_path"`
}

// CoreSession names this machine's own session, or empty when there is no
// answer to be had.
func CoreSession() string {
	c := New()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://local/whoami", nil)
	if err != nil {
		return ""
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ""
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if err != nil {
		return ""
	}
	var w whoami
	if err := json.Unmarshal(body, &w); err != nil {
		return ""
	}
	if w.CoreSession == nil {
		// The agent could not determine one. Not the same as this host having
		// none, and neither is a destination to deliver a meeting to.
		return ""
	}
	return *w.CoreSession
}

// A note on what comes back. The agent returns a session *id*
// ("claude-wsl-1df88a1a"), not the alias ("wsl"), and the id does not appear in
// the list of known recipients a refusal prints — which made it look
// unaddressable. Measured against the live agent: it is. POST /message/send to
// the raw id returns 200.
//
// It matters that this is resolved at the moment of delivery rather than
// stored. An alias survives a session restart and an id may not, so a config
// that had written one down could address a session that no longer exists.
// Nothing writes it down unless somebody runs `minutes config set`, and the
// default is computed fresh every time.
