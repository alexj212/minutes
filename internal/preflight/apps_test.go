package preflight

import (
	"errors"
	"strings"
	"testing"
)

var running = []App{
	{PID: 28832, Name: "meetily.exe", Active: true},
	{PID: 7972, Name: "obs64.exe", Active: true},
	{PID: 23276, Name: "firefox.exe", Active: false},
	{PID: 28056, Name: "Zoom.exe", Active: false},
	{PID: 55316, Name: "Zoom.exe", Active: false},
}

func TestFindAppByNameAndPID(t *testing.T) {
	got, err := FindApp(running, "obs")
	if err != nil {
		t.Fatal(err)
	}
	if got.PID != 7972 {
		t.Errorf("matched pid %d, want 7972", got.PID)
	}
	if got, err = FindApp(running, "28832"); err != nil || got.Name != "meetily.exe" {
		t.Errorf("matching by pid gave %+v, %v", got, err)
	}
}

// A process actually making noise is what somebody means by "the meeting",
// even when something idle shares the name.
func TestPlayingBeatsIdle(t *testing.T) {
	apps := []App{
		{PID: 1, Name: "Teams.exe", Active: false},
		{PID: 2, Name: "Teams.exe", Active: true},
	}
	got, err := FindApp(apps, "teams")
	if err != nil {
		t.Fatal(err)
	}
	if got.PID != 2 {
		t.Errorf("chose pid %d, want the one that is playing", got.PID)
	}
}

// An application that is open but silent can still be recorded: refusing would
// mean you cannot start until somebody speaks.
func TestIdleApplicationIsAllowed(t *testing.T) {
	got, err := FindApp(running, "firefox")
	if err != nil {
		t.Fatal(err)
	}
	if got.PID != 23276 {
		t.Errorf("matched pid %d, want 23276", got.PID)
	}
}

// Two instances and no way to tell them apart. Choosing would be a guess, and
// the wrong guess records silence.
func TestAmbiguousNameIsRefused(t *testing.T) {
	_, err := FindApp(running, "zoom")
	var amb *ErrAmbiguousApp
	if !errors.As(err, &amb) {
		t.Fatalf("got %v, want ErrAmbiguousApp", err)
	}
	if len(amb.Matched) != 2 {
		t.Errorf("matched %d, want 2", len(amb.Matched))
	}
	if !strings.Contains(err.Error(), "pid") {
		t.Error("the error does not tell you how to disambiguate")
	}
}

// Never fall back to recording everything: silently widening the capture is how
// a meeting ends up with a film in it.
func TestUnknownNameIsRefusedAndLists(t *testing.T) {
	_, err := FindApp(running, "notepad")
	var none *ErrNoSuchApp
	if !errors.As(err, &none) {
		t.Fatalf("got %v, want ErrNoSuchApp", err)
	}
	for _, want := range []string{"meetily.exe", "Zoom.exe", "records silence"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not mention %q:\n%s", want, err)
		}
	}
}

func TestNothingPlayingIsSaidPlainly(t *testing.T) {
	_, err := FindApp(nil, "zoom")
	if err == nil || !strings.Contains(err.Error(), "nothing is producing sound") {
		t.Errorf("got %v, want it to say nothing is playing", err)
	}
}
