package deliver

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/alexj/minutes/internal/manifest"
	"github.com/alexj/minutes/internal/transcript"
)

// inlineLimit is how much transcript is put in the message body rather than
// left on disk to be read.
//
// A short meeting arrives whole, which saves the receiving session a round
// trip. A long one does not, because filling a session's context with ninety
// minutes of dialogue costs it the room it needs to do the actual work. The
// path is always given, and the reader is on the same machine.
const inlineLimit = 24 << 10

// Notes is a summary somebody has already written, sent instead of the
// transcript.
//
// This exists because of a meeting that could not be delivered at all. The
// transcript contained thirteen minutes of private household conversation
// captured while the other party rebooted, and everything this package could
// send carried the transcript with it — inlined, or as a path to a file on the
// same machine as the reader. The only way to hand the notes over was to write
// them by hand and send them out of band, which is a gap in the tool rather
// than a workflow.
type Notes struct {
	Recording *manifest.Manifest
	Text      string
}

// Title is the one line an inbox listing shows.
func (n Notes) Title() string {
	what := n.Recording.Name
	if what == "" {
		what = n.Recording.ID
	}
	return fmt.Sprintf("Meeting notes: %s", what)
}

// Body is the notes, with just enough provenance to be trusted, and
// deliberately no transcript and no path to one.
func (n Notes) Body() string {
	m := n.Recording
	var s strings.Builder
	fmt.Fprintf(&s, "Notes from a meeting on %s, lasting %s.\n",
		m.StartedAt.Format("2006-01-02 15:04 MST"), duration(m.Duration()))
	s.WriteString("**The meeting was recorded and transcribed.**\n\n")
	s.WriteString("These are notes only. The transcript is not included and its location is " +
		"not given: a recording is of the room and not only of the meeting, and what is " +
		"safe to summarise is not always safe to hand over whole.\n\n---\n\n")
	s.WriteString(n.Text)
	return s.String()
}

// NotifyBody is the short form a human sees.
func (n Notes) NotifyBody() string {
	what := n.Recording.Name
	if what == "" {
		what = n.Recording.ID
	}
	return fmt.Sprintf("%s — %s. Notes delivered.", what, duration(n.Recording.Duration()))
}

// Brief is what a session is given to work from.
//
// The worker does not summarise. What matters in a meeting, and which project
// it belongs to, are judgments — and a session driven by a person is where
// those are made. This assembles the material and states the ask.
type Brief struct {
	Recording  *manifest.Manifest
	Transcript *transcript.Transcript
}

// Title is the one line an inbox listing shows.
func (b Brief) Title() string {
	what := b.Recording.Name
	if what == "" {
		what = b.Recording.ID
	}
	return fmt.Sprintf("Meeting notes needed: %s", what)
}

// Body is the message a session receives.
func (b Brief) Body() string {
	m, t := b.Recording, b.Transcript
	var s strings.Builder

	fmt.Fprintf(&s, "A meeting was recorded on this machine and transcribed. It needs notes.\n\n")

	fmt.Fprintf(&s, "## What happened\n\n")
	if m.Name != "" {
		fmt.Fprintf(&s, "- **%s**\n", m.Name)
	}
	fmt.Fprintf(&s, "- Started %s, ran %s\n", m.StartedAt.Format("2006-01-02 15:04 MST"), duration(m.Duration()))
	// The brief is read by a session that writes notes from it, so a false
	// attribution split here is worse than the same mistake on a console: prose
	// asserting "0 yours, 3 everyone else's" carries no field a reader can check
	// it against. When the recording cannot say who spoke, say that instead.
	if t.Unattributed {
		fmt.Fprintf(&s, "- %d lines, **none of them attributed** — this recording captured only one "+
			"source, so it cannot say who spoke. Do not credit anybody in the notes.\n", len(t.Lines))
	} else {
		c := transcript.Count(t.Lines)
		fmt.Fprintf(&s, "- %d lines: %d yours, %d everyone else's%s\n", len(t.Lines), c.You, c.Others,
			map[bool]string{true: fmt.Sprintf(", %d unattributed", c.Unattributed)}[c.Unattributed > 0])
	}
	for _, tr := range m.Tracks {
		fmt.Fprintf(&s, "- %s track: %s, peak %.1f dBFS\n", tr.Name, tr.Device, tr.PeakDBFS())
	}
	fmt.Fprintf(&s, "- Transcribed by %s; the audio %s this machine\n",
		t.Backend, map[bool]string{true: "**was sent off**", false: "stayed on"}[t.AudioLeftMachine])
	// Stated before the ask, not among the details. A session writing notes from
	// a half-recorded meeting has to know it is half a meeting.
	if t.MicrophoneLost {
		fmt.Fprintf(&s, "- **Nothing was captured from the microphone.** Everything the operator "+
			"said is absent. The speaker labels are correct — what is here really was the "+
			"other side — but this is one half of a conversation. Say so in the notes, and "+
			"do not record the operator as having been silent.\n")
	}
	if t.BleedSuppressed > 0 {
		fmt.Fprintf(&s, "- %d microphone line(s) were dropped as echoes of the system track: "+
			"this meeting played through speakers rather than headphones, so the microphone also heard "+
			"the far end. Attribution is otherwise exact — your track is you, the other track is everyone else.\n",
			t.BleedSuppressed)
	}

	fmt.Fprintf(&s, "\n## What is being asked\n\n")
	s.WriteString("1. Write the notes: **decisions**, **action items** with owners, and **open questions**.\n")
	s.WriteString("2. File them wherever they belong in your project. That judgment is yours; " +
		"the recorder deliberately does not make it.\n")
	s.WriteString("3. **Say in the notes that the meeting was recorded.** Recording is a trust matter " +
		"and in some places a legal one, so it should be stated rather than left to be discovered.\n")

	// Absolute, always. The reader is a different process with its own working
	// directory, so a relative path here is a path to nothing.
	fmt.Fprintf(&s, "\n## Where things are\n\n")
	fmt.Fprintf(&s, "- transcript: `%s`\n", abs(m.Dir(), transcript.TextName))
	fmt.Fprintf(&s, "- structured: `%s`\n", abs(m.Dir(), transcript.JSONName))
	fmt.Fprintf(&s, "- manifest:   `%s`\n", abs(m.Dir(), manifest.Name))
	fmt.Fprintf(&s, "- audio:      `%s`\n", abs(m.Dir()))

	text := t.Text()
	if len(text) <= inlineLimit {
		fmt.Fprintf(&s, "\n## Transcript\n\n```\n%s```\n", text)
	} else {
		fmt.Fprintf(&s, "\n## Transcript\n\nToo long to inline (%d KB). Read it from the path above.\n", len(text)>>10)
	}
	return s.String()
}

// NotifyBody is the short form a human sees.
func (b Brief) NotifyBody() string {
	what := b.Recording.Name
	if what == "" {
		what = b.Recording.ID
	}
	return fmt.Sprintf("%s — %s, %d lines transcribed. Notes requested.",
		what, duration(b.Recording.Duration()), len(b.Transcript.Lines))
}

// abs joins the parts and makes the result absolute, falling back to the
// relative form if the working directory cannot be determined — a wrong-looking
// path is better than an empty one.
func abs(parts ...string) string {
	joined := filepath.Join(parts...)
	if out, err := filepath.Abs(joined); err == nil {
		return out
	}
	return joined
}

func duration(seconds float64) string {
	d := time.Duration(seconds * float64(time.Second))
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
}
