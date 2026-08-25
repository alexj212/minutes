package transcript

import "testing"

func mic(start, end float64, text string) Line {
	return Line{Start: start, End: end, Track: "mic", Speaker: SpeakerYou, Text: text}
}
func sys(start, end float64, text string) Line {
	return Line{Start: start, End: end, Track: "system", Speaker: SpeakerOthers, Text: text}
}

// The failure this exists for: a meeting on speakers puts the far end on both
// tracks, and the transcript then says you said what somebody else said.
func TestEchoOfTheSystemTrackIsDroppedFromTheMic(t *testing.T) {
	lines := []Line{
		sys(4.0, 8.0, "We agreed to ship the recorder on Thursday."),
		mic(4.1, 8.2, "we agreed to ship the recorder on thursday"),
	}
	kept, dropped := SuppressBleed(lines)
	if dropped != 1 {
		t.Fatalf("dropped %d lines, want 1", dropped)
	}
	if len(kept) != 1 || kept[0].Track != "system" {
		t.Fatalf("kept %+v, want only the system line", kept)
	}
}

// The mic track is the reason the recorder exists. Removing an echo must not
// remove anything the operator actually said.
func TestDistinctMicSpeechIsKept(t *testing.T) {
	lines := []Line{
		sys(4.0, 8.0, "We agreed to ship the recorder on Thursday."),
		mic(4.1, 8.2, "Actually I think Friday is more realistic given the migration."),
	}
	kept, dropped := SuppressBleed(lines)
	if dropped != 0 {
		t.Fatalf("dropped %d lines of genuine speech", dropped)
	}
	if len(kept) != 2 {
		t.Fatalf("kept %d lines, want 2", len(kept))
	}
}

// The same words said at a different time are not an echo — they are somebody
// agreeing, or repeating themselves, and both belong in the transcript.
func TestSameWordsFarApartInTimeAreKept(t *testing.T) {
	lines := []Line{
		sys(4.0, 6.0, "ship it on Thursday"),
		mic(600.0, 602.0, "ship it on Thursday"),
	}
	kept, dropped := SuppressBleed(lines)
	if dropped != 0 {
		t.Fatalf("dropped %d lines said ten minutes apart", dropped)
	}
	if len(kept) != 2 {
		t.Fatalf("kept %d lines, want 2", len(kept))
	}
}

// A model splits the same speech differently on each track, so a short mic
// fragment wholly inside a longer system line is still an echo.
func TestFragmentOfALongerSystemLineIsAnEcho(t *testing.T) {
	lines := []Line{
		sys(7.0, 12.0, "Karyn will write the migration notes and the open question is the old endpoint"),
		mic(8.0, 9.5, "write the migration notes"),
	}
	_, dropped := SuppressBleed(lines)
	if dropped != 1 {
		t.Errorf("dropped %d lines, want 1 — a fragment of a system line is an echo", dropped)
	}
}

// With nothing on the system track there is nothing to have echoed, so the mic
// track must pass through untouched.
func TestMicOnlyRecordingIsUntouched(t *testing.T) {
	lines := []Line{mic(0, 1, "one"), mic(1, 2, "two")}
	kept, dropped := SuppressBleed(lines)
	if dropped != 0 || len(kept) != 2 {
		t.Errorf("dropped %d of a mic-only recording", dropped)
	}
}

// Echo travels from the speakers to the microphone, never the other way:
// conferencing software does not send your voice back to you. A system line
// must never be removed.
func TestSystemLinesAreNeverDropped(t *testing.T) {
	lines := []Line{
		mic(4.0, 8.0, "we agreed to ship the recorder on thursday"),
		sys(4.1, 8.2, "We agreed to ship the recorder on Thursday."),
	}
	kept, _ := SuppressBleed(lines)
	found := false
	for _, l := range kept {
		if l.Track == "system" {
			found = true
		}
	}
	if !found {
		t.Error("the system line was dropped; echo suppression must only ever remove microphone lines")
	}
}

func TestContainment(t *testing.T) {
	cases := []struct {
		a, b string
		want float64
	}{
		{"one two three", "one two three", 1},
		{"one two", "one two three four", 1},
		{"one two three four", "five six seven eight", 0},
		{"one two three four", "one two five six", 0.5},
	}
	for _, c := range cases {
		if got := containment(words(c.a), words(c.b)); got != c.want {
			t.Errorf("containment(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

// Punctuation and case differ between two transcriptions of one sentence and
// must not stop them matching.
func TestNormalisationIgnoresCaseAndPunctuation(t *testing.T) {
	if got := containment(words("Right, let us start the stand-up!"), words("right let us start the stand up")); got < 0.9 {
		t.Errorf("containment across punctuation and case = %v, want ~1", got)
	}
}
