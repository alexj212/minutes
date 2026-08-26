package preflight

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"sort"
	"strings"
	"time"
)

// Choosing what to record.
//
// System-wide loopback takes everything the machine plays, so a video in
// another window becomes dialogue in the transcript. Capturing one application
// fixes that and introduces a worse failure in its place: name the wrong
// process and the recording is silent, which is discovered after the meeting.
//
// So the target is picked from what the audio engine says is actually producing
// sound, and a name that matches nothing is refused rather than recorded.

// App is a process the audio engine knows about.
type App struct {
	PID  int    `json:"pid"`
	Name string `json:"name"`
	// Active means it is producing sound now, rather than merely holding a
	// session open.
	Active bool `json:"active"`
}

// ListApps asks the helper which processes have audio sessions.
func ListApps(ctx context.Context, helper string) ([]App, error) {
	probe, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	out, err := exec.CommandContext(probe, helper, "--list-apps").Output()
	if err != nil && len(out) == 0 {
		return nil, fmt.Errorf("listing audio applications: %w", err)
	}
	var parsed struct {
		Apps  []App  `json:"apps"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(out, &parsed); err != nil {
		return nil, fmt.Errorf("reading the helper's application list: %w", err)
	}
	if parsed.Error != "" {
		return nil, fmt.Errorf("%s", parsed.Error)
	}
	// Playing first, then by name, so a listing reads the way somebody looking
	// for their meeting would expect.
	sort.SliceStable(parsed.Apps, func(i, j int) bool {
		if parsed.Apps[i].Active != parsed.Apps[j].Active {
			return parsed.Apps[i].Active
		}
		return strings.ToLower(parsed.Apps[i].Name) < strings.ToLower(parsed.Apps[j].Name)
	})
	return parsed.Apps, nil
}

// ErrNoSuchApp means nothing matched, which must never be treated as "record
// everything" — that is how a meeting is captured from the wrong source.
type ErrNoSuchApp struct {
	Want string
	Saw  []App
}

func (e *ErrNoSuchApp) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "no application matching %q is playing audio", e.Want)
	if len(e.Saw) == 0 {
		b.WriteString("\nnothing is producing sound at all right now")
		return b.String()
	}
	b.WriteString("\n\nthese are:")
	for _, a := range e.Saw {
		state := "idle"
		if a.Active {
			state = "playing"
		}
		fmt.Fprintf(&b, "\n  %-24s pid %-7d %s", a.Name, a.PID, state)
	}
	b.WriteString("\n\nStart the meeting first, then record: a process with no audio session " +
		"cannot be captured, and naming the wrong one records silence.")
	return b.String()
}

// ErrAmbiguousApp means several processes matched and choosing between them
// would be a guess.
type ErrAmbiguousApp struct {
	Want    string
	Matched []App
	// Playing distinguishes "several are making noise" from "several are open
	// and none of them is", which are different problems for whoever is trying
	// to start a recording.
	Playing bool
}

func (e *ErrAmbiguousApp) Error() string {
	var b strings.Builder
	state := "have an audio session open"
	if e.Playing {
		state = "are playing audio"
	}
	fmt.Fprintf(&b, "%q matches %d processes that %s:", e.Want, len(e.Matched), state)
	for _, a := range e.Matched {
		fmt.Fprintf(&b, "\n  %-24s pid %d", a.Name, a.PID)
	}
	b.WriteString("\n\nName one by its pid instead; picking for you would be a guess, " +
		"and the wrong guess records silence.")
	return b.String()
}

// FindApp resolves what somebody typed into a single process id.
//
// Matches a pid exactly, or a case-insensitive substring of the executable
// name. Processes that are actually playing win over ones merely holding a
// session open, because that is what somebody means by "the meeting".
func FindApp(apps []App, want string) (App, error) {
	var active, idle []App
	for _, a := range apps {
		if fmt.Sprintf("%d", a.PID) == want ||
			strings.Contains(strings.ToLower(a.Name), strings.ToLower(want)) {
			if a.Active {
				active = append(active, a)
			} else {
				idle = append(idle, a)
			}
		}
	}
	switch {
	case len(active) == 1:
		return active[0], nil
	case len(active) > 1:
		return App{}, &ErrAmbiguousApp{Want: want, Matched: active, Playing: true}
	case len(idle) == 1:
		// Holding a session open but silent: a meeting that has not started
		// making noise yet. Allowed, since refusing would mean you cannot start
		// recording before somebody speaks.
		return idle[0], nil
	case len(idle) > 1:
		return App{}, &ErrAmbiguousApp{Want: want, Matched: idle}
	}
	return App{}, &ErrNoSuchApp{Want: want, Saw: apps}
}
