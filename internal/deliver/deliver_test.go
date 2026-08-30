package deliver

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alexj212/minutes/internal/manifest"
	"github.com/alexj212/minutes/internal/transcript"
)

// fakeAgent serves a unix socket the way shabadoo's node does, and records what
// it was sent.
type fakeAgent struct {
	status int
	body   string
	calls  map[string]map[string]any
}

func startFakeAgent(t *testing.T, status int, body string) *fakeAgent {
	t.Helper()
	path := shortSocketPath(t)

	a := &fakeAgent{status: status, body: body, calls: map[string]map[string]any{}}
	mux := http.NewServeMux()
	handler := func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		var parsed map[string]any
		_ = json.Unmarshal(raw, &parsed)
		a.calls[r.URL.Path] = parsed
		w.WriteHeader(a.status)
		io.WriteString(w, a.body)
	}
	mux.HandleFunc("/message/send", handler)
	mux.HandleFunc("/notify", handler)

	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("listening on %s: %v", path, err)
	}
	srv := &http.Server{Handler: mux}
	go srv.Serve(ln)
	t.Cleanup(func() {
		srv.Close()
		ln.Close()
	})
	t.Setenv("SHABADOO_SOCKET", path)
	return a
}

// shortSocketPath returns a socket path guaranteed to fit in sun_path.
//
// macOS caps a unix socket path at 104 bytes and Linux at 108, and t.TempDir()
// on macOS is already ~80 before the test name is added — so whether a test
// binds or fails with "bind: invalid argument" depends on how long its function
// name is. TestSendRefusesWithoutADestination produced exactly 104 and failed;
// a shorter neighbour passed. That is the worst shape of bug, since it looks
// random and reappears whenever somebody adds a test with a long name.
//
// Found by minutes-mac running this suite on a Mac. It never failed here and
// never would have.
func shortSocketPath(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "min")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })

	path := filepath.Join(dir, "a.sock")
	// Assert rather than hope. If a future change lengthens this, the failure
	// should name the reason instead of surfacing as an unexplained bind error.
	if len(path) >= 104 {
		t.Fatalf("socket path is %d bytes, and macOS allows 103: %s", len(path), path)
	}
	return path
}

func fixture(t *testing.T) Brief {
	t.Helper()
	dir := t.TempDir()
	m := manifest.New(dir, "2026-08-25-101530-standup", "standup", 300)
	if err := m.SetTrack("mic", "Some Microphone", 48000, 2); err != nil {
		t.Fatal(err)
	}
	if err := m.PutSegment("mic", manifest.Segment{
		Index: 0, File: "mic-000.wav", StartSeconds: 0, DurationSeconds: 120,
		Frames: 100, PeakDBFS: -7.3, Complete: true,
	}); err != nil {
		t.Fatal(err)
	}
	return Brief{
		Recording: m,
		Transcript: &transcript.Transcript{
			RecordingID: m.ID,
			Backend:     "local-whisper:small",
			Recorded:    true,
			Lines: []transcript.Line{
				{Start: 1, End: 2, Track: "mic", Speaker: transcript.SpeakerYou, Text: "my line"},
				{Start: 3, End: 4, Track: "system", Speaker: transcript.SpeakerOthers, Text: "their line"},
			},
		},
	}
}

// The brief has to state the ask, because a session receiving it has no other
// instruction, and has to say the meeting was recorded, because that is the
// part somebody may have to answer for later.
func TestBriefStatesTheAskAndThatItWasRecorded(t *testing.T) {
	b := fixture(t)
	body := b.Body()
	for _, want := range []string{
		"decisions", "action items", "open questions",
		"the meeting was recorded",
		"transcript.txt", "manifest.json",
		"local-whisper:small",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("brief does not mention %q:\n%s", want, body)
		}
	}
	if !strings.Contains(b.Title(), "standup") {
		t.Errorf("title does not name the meeting: %q", b.Title())
	}
}

// Where the notes belong is the session's judgment. The brief must say so
// rather than quietly implying the recorder already decided.
func TestBriefLeavesFilingToTheSession(t *testing.T) {
	if body := fixture(t).Body(); !strings.Contains(body, "judgment is yours") {
		t.Errorf("brief does not leave filing to the session:\n%s", body)
	}
}

func TestShortTranscriptIsInlinedAndLongOneIsNot(t *testing.T) {
	b := fixture(t)
	if !strings.Contains(b.Body(), "my line") {
		t.Error("a short transcript was not inlined")
	}

	long := make([]transcript.Line, 0, 4000)
	for i := 0; i < 4000; i++ {
		long = append(long, transcript.Line{Start: float64(i), Speaker: transcript.SpeakerOthers,
			Text: "a line of dialogue that is reasonably long so the transcript exceeds the inline limit"})
	}
	b.Transcript.Lines = long
	body := b.Body()
	if strings.Contains(body, "a line of dialogue") {
		t.Error("a long transcript was inlined; it would fill the receiving session's context")
	}
	if !strings.Contains(body, "Too long to inline") {
		t.Error("a long transcript was dropped without saying where it is")
	}
	if !strings.Contains(body, "transcript.txt") {
		t.Error("the path is missing, so the transcript is unreachable")
	}
}

// Speaker bleed changes how the transcript should be read, so the session is
// told rather than left to wonder why nobody said anything.
func TestBriefMentionsSuppressedBleed(t *testing.T) {
	b := fixture(t)
	b.Transcript.BleedSuppressed = 4
	if !strings.Contains(b.Body(), "echoes of the system track") {
		t.Error("the brief does not explain that microphone lines were dropped")
	}
}

func TestSendRefusesWithoutADestination(t *testing.T) {
	startFakeAgent(t, 200, "{}")
	err := New().Send(context.Background(), Message{Title: "x", Body: "y"})
	if err == nil {
		t.Fatal("sent with no destination")
	}
	if !strings.Contains(err.Error(), "judgment call") {
		t.Errorf("error does not explain why: %v", err)
	}
}

func TestSendPostsTheExpectedShape(t *testing.T) {
	a := startFakeAgent(t, 200, `{"ok":true}`)
	err := New().Send(context.Background(), Message{
		To: "homelab", From: "minutes", Title: "T", Body: "B", Tag: "minutes",
	})
	if err != nil {
		t.Fatal(err)
	}
	got := a.calls["/message/send"]
	if got == nil {
		t.Fatal("nothing was posted to /message/send")
	}
	for k, want := range map[string]string{
		"to_session": "homelab", "from_session": "minutes",
		"title": "T", "body": "B", "tag": "minutes", "type": "info",
	} {
		if got[k] != want {
			t.Errorf("field %q = %v, want %q", k, got[k], want)
		}
	}
}

func TestNotifyPostsToNotify(t *testing.T) {
	a := startFakeAgent(t, 200, "{}")
	if err := New().Notify(context.Background(), "T", "B", "minutes"); err != nil {
		t.Fatal(err)
	}
	if a.calls["/notify"] == nil {
		t.Fatal("nothing was posted to /notify")
	}
	if a.calls["/notify"]["body"] != "B" {
		t.Errorf("body = %v, want B", a.calls["/notify"]["body"])
	}
}

// A coordinator that blipped and a coordinator that said no call for different
// behaviour, so they must be distinguishable by the caller.
func TestUnreachableAgentIsDistinguishable(t *testing.T) {
	t.Setenv("SHABADOO_SOCKET", shortSocketPath(t)+".absent")
	err := New().Send(context.Background(), Message{To: "homelab", Title: "T", Body: "B"})
	if !errors.Is(err, ErrUnreachable) {
		t.Fatalf("got %v, want ErrUnreachable — the caller cannot tell a blip from a refusal", err)
	}
}

func TestThrottlingIsDistinguishable(t *testing.T) {
	startFakeAgent(t, http.StatusTooManyRequests, "rate limit exceeded")
	err := New().Send(context.Background(), Message{To: "homelab", Title: "T", Body: "B"})
	if !errors.Is(err, ErrThrottled) {
		t.Fatalf("got %v, want ErrThrottled", err)
	}
	if errors.Is(err, ErrUnreachable) {
		t.Error("a refusal was reported as an outage")
	}
}

func TestOtherFailuresAreNeitherUnreachableNorThrottled(t *testing.T) {
	startFakeAgent(t, http.StatusInternalServerError, "boom")
	err := New().Send(context.Background(), Message{To: "homelab", Title: "T", Body: "B"})
	if err == nil {
		t.Fatal("a 500 was reported as success")
	}
	if errors.Is(err, ErrUnreachable) || errors.Is(err, ErrThrottled) {
		t.Errorf("a 500 was misclassified: %v", err)
	}
}

func TestDurationRendering(t *testing.T) {
	cases := map[float64]string{45: "45s", 90: "1m30s", 3725: "62m05s"}
	for in, want := range cases {
		if got := duration(in); got != want {
			t.Errorf("duration(%v) = %q, want %q", in, got, want)
		}
	}
}

var _ = time.Second

// The brief is read by a different process with its own working directory, so
// a relative path in it is a path to nothing.
//
// The recording directory here is deliberately relative — which is what
// `minutes deliver --root recordings` produces. With an absolute one this test
// passes whether or not the conversion exists, because there is nothing left to
// convert.
func TestBriefPathsAreAbsolute(t *testing.T) {
	b := fixture(t)
	b.Recording = manifest.New(filepath.Join("recordings", "2026-08-25-101530"), "2026-08-25-101530", "standup", 300)
	for _, line := range strings.Split(b.Body(), "\n") {
		if !strings.HasPrefix(line, "- transcript:") &&
			!strings.HasPrefix(line, "- structured:") &&
			!strings.HasPrefix(line, "- manifest:") &&
			!strings.HasPrefix(line, "- audio:") {
			continue
		}
		path := strings.Trim(strings.SplitN(line, ":", 2)[1], " `")
		if !filepath.IsAbs(path) {
			t.Errorf("path is relative and so unusable by the receiving session: %q", line)
		}
	}
}

// The whole point of Notes: nothing about the transcript travels with them.
// Not the text, and not a path to a file on the same machine as the reader.
//
// This exists because of a meeting whose transcript held thirteen minutes of
// private household conversation, and every way this package could send it
// carried the transcript along.
func TestNotesCarryNoTranscriptAndNoPath(t *testing.T) {
	b := fixture(t)
	n := Notes{Recording: b.Recording, Text: "## Decisions\n- ship on Thursday"}
	body := n.Body()

	if strings.Contains(body, "my line") || strings.Contains(body, "their line") {
		t.Error("notes carried transcript content")
	}
	if strings.Contains(body, b.Recording.Dir()) {
		t.Error("notes carried the recording directory, so the reader can open the transcript")
	}
	for _, leak := range []string{"transcript.txt", "transcript.json", "manifest.json", ".wav"} {
		if strings.Contains(body, leak) {
			t.Errorf("notes mention %q", leak)
		}
	}
	if !strings.Contains(body, "ship on Thursday") {
		t.Error("the notes themselves are missing")
	}
}

// A recording is a trust matter, so notes have to say they came from one even
// when the transcript does not travel with them.
func TestNotesSayTheMeetingWasRecorded(t *testing.T) {
	b := fixture(t)
	n := Notes{Recording: b.Recording, Text: "something"}
	body := n.Body()
	if !strings.Contains(body, "recorded") {
		t.Errorf("notes do not say the meeting was recorded:\n%s", body)
	}
	if !strings.Contains(n.Title(), "standup") {
		t.Errorf("title does not name the meeting: %q", n.Title())
	}
}

// Brief and Notes must be distinguishable in an inbox listing: one is asking
// for work, the other is delivering it.
func TestNotesAndBriefTitlesDiffer(t *testing.T) {
	b := fixture(t)
	n := Notes{Recording: b.Recording, Text: "x"}
	if n.Title() == b.Title() {
		t.Errorf("both titled %q; a session cannot tell a request from a delivery", n.Title())
	}
}

// The brief is the input to whoever writes the notes, so a false attribution
// split in it is worse than the same mistake on a console: it is prose, and
// there is no field beside it a reader can check it against.
//
// Asserted as a pair rather than against the unattributed case alone. A test
// that only knew what an unattributed brief says would pass just as happily if
// every brief said it — which is the blindness that let three copies of the
// same else-branch survive.
func TestBriefDoesNotSplitLinesItCannotAttribute(t *testing.T) {
	attributed := fixture(t).Body()

	b := fixture(t)
	b.Transcript.Unattributed = true
	for i := range b.Transcript.Lines {
		b.Transcript.Lines[i].Speaker = transcript.SpeakerUnattributed
	}
	unattributed := b.Body()

	if attributed == unattributed {
		t.Fatal("a recording that cannot say who spoke produces the same brief as one that can")
	}
	if !strings.Contains(attributed, "1 yours, 1 everyone else's") {
		t.Errorf("a two-source recording lost its split:\n%s", attributed)
	}
	for _, never := range []string{"0 yours", "yours,"} {
		if strings.Contains(unattributed, never) {
			t.Errorf("brief claims an attribution split it cannot support (%q):\n%s", never, unattributed)
		}
	}
	if !strings.Contains(unattributed, "none of them attributed") {
		t.Errorf("brief does not say the recording cannot attribute:\n%s", unattributed)
	}
}

// A destination the coordinator has never seen is refused at send time and
// nothing is kept. That is final, not transient — the message is not queued and
// will not arrive when somebody opens that project.
//
// This program was built with no fallback for it, because shabadoo's own
// documentation said mail for a project that is not running is stored and
// drains at startup. Only half true: a project the coordinator has *seen*
// queues, one it has never seen bounces. Running-or-not was never the line.
//
// Asserted as a pair, because the refusal is recognised by matching text. A 400
// that is this program sending a malformed body must NOT be reported as the
// operator naming a project that does not exist — they need opposite responses,
// and a matcher that claims every 400 looks identical to a correct one from the
// only example anybody usually writes.
func TestAnUnknownDestinationIsFinalAndAMalformedRequestIsNot(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool
	}{
		{"unknown recipient", `no session matches that recipient: "nope" (known: wsl, mac)`, true},
		{"our own bad request", `bad request: json: unknown field "to"`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			startFakeAgent(t, http.StatusBadRequest, tc.body)
			err := New().Send(context.Background(), Message{To: "somewhere", Title: "t", Body: "b"})
			if err == nil {
				t.Fatal("a 400 was reported as a successful delivery")
			}
			if got := errors.Is(err, ErrUnknownDestination); got != tc.want {
				t.Errorf("ErrUnknownDestination = %v, want %v (err: %v)", got, tc.want, err)
			}
		})
	}
}
