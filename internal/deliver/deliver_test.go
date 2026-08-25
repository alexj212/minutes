package deliver

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alexj/minutes/internal/manifest"
	"github.com/alexj/minutes/internal/transcript"
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
	// A short path: unix sockets have a ~100 byte limit and t.TempDir() names
	// are long enough to blow it on some systems.
	dir := t.TempDir()
	path := filepath.Join(dir, "a.sock")

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
	t.Setenv("SHABADOO_SOCKET", filepath.Join(t.TempDir(), "nothing-here.sock"))
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
