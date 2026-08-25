package transcript

import "testing"

// The failure this exists for: a call drops, the recorder keeps going, and
// everything the room says lands in the transcript attributed to you,
// indistinguishable from something you said in the meeting. Observed on a real
// two-hour call — thirteen minutes of private household conversation sitting in
// the middle of a work transcript with nothing to mark it.
func TestStretchWithNoFarEndIsFlagged(t *testing.T) {
	lines := []Line{
		sys(0, 10, "shall we start"),
		mic(11, 20, "yes lets"),
		sys(21, 30, "I need to reboot, back in a bit"),
		// Ten minutes of room.
		mic(40, 50, "have you seen my keys"),
		mic(300, 310, "dinner is nearly ready"),
		mic(600, 610, "tell her to come down"),
		sys(700, 710, "ok I am back"),
		mic(711, 715, "great, where were we"),
	}
	Sort(lines)
	gaps := findFarEndSilence(lines)
	if len(gaps) != 1 {
		t.Fatalf("found %d silent stretches, want 1: %+v", len(gaps), gaps)
	}
	if gaps[0].Start != 30 || gaps[0].End != 700 {
		t.Errorf("stretch is %v-%v, want 30-700", gaps[0].Start, gaps[0].End)
	}
	if gaps[0].Lines != 3 {
		t.Errorf("stretch holds %d lines, want 3", gaps[0].Lines)
	}

	markFarEndSilence(lines, gaps)
	for _, l := range lines {
		inside := l.Start >= 30 && l.Start < 700
		if l.FarEndSilent != inside {
			t.Errorf("line at %.0fs marked %v, want %v: %q", l.Start, l.FarEndSilent, inside, l.Text)
		}
	}
}

// An ordinary pause, or one person talking for a while, must not be flagged —
// a marker on every quiet minute would be noise, and noise gets ignored.
func TestOrdinaryPausesAreNotFlagged(t *testing.T) {
	lines := []Line{
		sys(0, 10, "so here is the thing"),
		mic(12, 20, "go on"),
		sys(25, 90, "a long explanation"),
		mic(95, 100, "understood"),
		sys(105, 110, "good"),
	}
	Sort(lines)
	if gaps := findFarEndSilence(lines); len(gaps) != 0 {
		t.Errorf("flagged %d stretches in an ordinary conversation: %+v", len(gaps), gaps)
	}
}

// A recording that ends after the far end has gone still has the tail flagged,
// because that tail is exactly where somebody forgets the recorder is running.
func TestSilenceRunningToTheEndIsFlagged(t *testing.T) {
	lines := []Line{
		sys(0, 10, "I have to go"),
		mic(15, 20, "bye"),
		mic(400, 410, "right, what is for dinner"),
	}
	Sort(lines)
	gaps := findFarEndSilence(lines)
	if len(gaps) != 1 {
		t.Fatalf("found %d stretches, want 1", len(gaps))
	}
	if gaps[0].Start != 10 {
		t.Errorf("stretch starts at %v, want 10", gaps[0].Start)
	}
}

// A microphone-only recording is not a meeting whose far end dropped out, and
// marking the whole thing would say nothing.
func TestMicOnlyRecordingIsNotFlagged(t *testing.T) {
	lines := []Line{mic(0, 10, "a note to myself"), mic(600, 610, "and another")}
	if gaps := findFarEndSilence(lines); len(gaps) != 0 {
		t.Errorf("flagged %d stretches in a mic-only recording", len(gaps))
	}
}

// The readable transcript has to carry the warning, because that is the file a
// person actually reads before deciding what to do with it.
func TestReadableTranscriptCarriesTheWarning(t *testing.T) {
	tr := &Transcript{
		RecordingID:  "rec-1",
		FarEndSilent: []Silence{{Start: 30, End: 700, Lines: 3}},
		Lines: []Line{
			{Start: 0, Track: "system", Speaker: SpeakerOthers, Text: "shall we start"},
			{Start: 40, Track: "mic", Speaker: SpeakerYou, Text: "have you seen my keys", FarEndSilent: true},
			{Start: 700, Track: "system", Speaker: SpeakerOthers, Text: "ok I am back"},
		},
	}
	text := tr.Text()
	if !containsAll(text, "may be the room", "11 minutes") {
		t.Errorf("transcript does not warn about the stretch:\n%s", text)
	}
}

func containsAll(s string, subs ...string) bool {
	for _, sub := range subs {
		found := false
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}
