// Package deliver hands a finished recording to a session, and tells the human.
//
// It talks to shabadoo's local agent socket, which allowlists /message/send and
// /notify. The socket is 0600 in the operator's own directory, so being able to
// open it means already being this user: there is no credential here, no
// enrolment, and nothing to rotate. That property is the reason the orchestrator
// stayed a Linux process rather than moving to the Windows side.
//
// It does not decide where the notes belong. That is a judgment call and a
// session makes it.
package deliver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// SocketPath is shabadoo's local agent socket.
func SocketPath() string {
	if v := os.Getenv("SHABADOO_SOCKET"); v != "" {
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(os.TempDir(), "shabadoo-agent.sock")
	}
	return filepath.Join(home, ".config", "shabadoo", "agent.sock")
}

// ErrUnreachable means the agent could not be reached at all.
//
// Distinguished from a refusal, because they call for different behaviour: a
// coordinator that blipped should cost nothing, while a coordinator that said
// no is telling you something.
var ErrUnreachable = errors.New("no shabadoo agent on this host")

// ErrUnknownDestination means the coordinator has never seen that recipient and
// refused to take the message.
//
// This is the important one, because it was documented as the opposite.
// shabadoo's own CLAUDE.md said mail for a project that is not running is
// stored against the id it would have and drains when it starts, and this
// program was built with no fallback on the strength of that sentence. It is
// only half true: a project the coordinator has *seen* — in its node's
// startable folder list — queues correctly, and one it has never seen is
// refused at send time and nothing is kept. Running-or-not was never the line.
//
// So a refusal is final. It is not queued, not waiting, and not going to arrive
// when somebody opens that project. Treating it as a transient failure would
// leave a meeting addressed to nobody with the operator told it was sent.
//
// Measured against the live agent rather than assumed: HTTP 400, body
// "no session matches that recipient: ... (known: ...)". Matching the text is
// not lovely, but a 400 on this route can also be a malformed body, which is
// this program's bug rather than the operator's, and those two must not be
// reported as the same thing.
var ErrUnknownDestination = errors.New("the coordinator has never seen that destination")

// ErrThrottled means the coordinator refused because this session has sent too
// much. It is a loop guard, and a recorder should never approach it: notes go
// out once per meeting.
var ErrThrottled = errors.New("coordinator is throttling this session")

// Client talks to the local agent.
type Client struct {
	http *http.Client
	path string
}

func New() *Client {
	path := SocketPath()
	return &Client{
		path: path,
		http: &http.Client{
			// Generous: the body carries a transcript, and the coordinator may
			// be reconnecting.
			Timeout: 60 * time.Second,
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					return (&net.Dialer{}).DialContext(ctx, "unix", path)
				},
			},
		},
	}
}

func (c *Client) post(ctx context.Context, route string, body any) error {
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://agent"+route, strings.NewReader(string(raw)))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("%w (%s): %v", ErrUnreachable, c.path, err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	if resp.StatusCode == http.StatusTooManyRequests {
		return fmt.Errorf("%w: %s", ErrThrottled, strings.TrimSpace(string(out)))
	}
	if resp.StatusCode == http.StatusBadRequest && strings.Contains(string(out), "no session matches") {
		// The body carries the list of destinations that do exist, which is the
		// useful half of the refusal, so it is passed through rather than
		// summarised away.
		return fmt.Errorf("%w: %s", ErrUnknownDestination, strings.TrimSpace(string(out)))
	}
	if resp.StatusCode >= 400 {
		return fmt.Errorf("POST %s: %s: %s", route, resp.Status, strings.TrimSpace(string(out)))
	}
	return nil
}

// Message is one inbox delivery.
type Message struct {
	To    string
	Title string
	Body  string
	Type  string
	Tag   string
	From  string
}

// Send puts a message in a session's inbox.
func (c *Client) Send(ctx context.Context, m Message) error {
	if m.To == "" {
		return errors.New("no destination: which project the notes belong to is a judgment call, and this program does not make it")
	}
	if m.Type == "" {
		m.Type = "info"
	}
	return c.post(ctx, "/message/send", map[string]any{
		"to_session":   m.To,
		"title":        m.Title,
		"body":         m.Body,
		"type":         m.Type,
		"tag":          m.Tag,
		"from_session": m.From,
	})
}

// Notify tells the human something happened.
func (c *Client) Notify(ctx context.Context, title, body, tag string) error {
	return c.post(ctx, "/notify", map[string]any{
		"title": title,
		"body":  body,
		"tag":   tag,
		"type":  "info",
	})
}
