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
	if len(dropped) != 1 {
		t.Fatalf("dropped %d lines, want 1", len(dropped))
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
	if len(dropped) != 0 {
		t.Fatalf("dropped %d lines of genuine speech", len(dropped))
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
	if len(dropped) != 0 {
		t.Fatalf("dropped %d lines said ten minutes apart", len(dropped))
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
	if len(dropped) != 1 {
		t.Errorf("dropped %d lines, want 1 — a fragment of a system line is an echo", len(dropped))
	}
}

// With nothing on the system track there is nothing to have echoed, so the mic
// track must pass through untouched.
func TestMicOnlyRecordingIsUntouched(t *testing.T) {
	lines := []Line{mic(0, 1, "one"), mic(1, 2, "two")}
	kept, dropped := SuppressBleed(lines)
	if len(dropped) != 0 || len(kept) != 2 {
		t.Errorf("dropped %d of a mic-only recording", len(dropped))
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

// The failure this second pass exists for, taken from a real recording:
//
//   [00:00:26] Others: The open question is whether we keep the old end
//   [00:00:28] You:    all
//
// "all" is the tail of "...the old endpoint alive" arriving through the air.
// The far-end transcript was cut before "alive", so the word appears nowhere in
// the line it echoes and word overlap scores zero. Level catches it.
func TestQuietFragmentDuringFarEndSpeechIsDropped(t *testing.T) {
	lines := []Line{
		sys(26, 29, "The open question is whether we keep the old end"),
		mic(28, 29, "all"),
	}
	const reference = -6.0 // this person speaks at about -6 dBFS
	measure := func(l Line) (float64, bool) { return -30.0, true } // the fragment is faint

	kept, dropped := suppressQuietFragments(lines, reference, measure)
	if len(dropped) != 1 {
		t.Fatalf("dropped %d, want 1 — the fragment was attributed to the wrong person", len(dropped))
	}
	if len(kept) != 1 || kept[0].Track != "system" {
		t.Fatalf("kept %+v, want only the far-end line", kept)
	}
}

// A short line at full volume is you actually saying it. Interjections are
// short by nature — "yes", "agreed", "no, Thursday" — and dropping them for
// being short would lose the parts of a meeting that decide things.
func TestShortButLoudInterjectionIsKept(t *testing.T) {
	lines := []Line{
		sys(26, 30, "so we ship on Thursday then"),
		mic(28, 29, "no, Friday"),
	}
	measure := func(l Line) (float64, bool) { return -7.0, true } // right at speaking level
	_, dropped := suppressQuietFragments(lines, -6.0, measure)
	if len(dropped) != 0 {
		t.Error("dropped an interjection spoken at full volume")
	}
}

// A whole quiet sentence is somebody speaking softly, not an echo. Only
// fragments are eligible.
func TestQuietButLongLineIsKept(t *testing.T) {
	lines := []Line{
		sys(26, 40, "and that is roughly where we landed on it"),
		mic(28, 34, "I think Friday is more realistic given the migration work"),
	}
	measure := func(l Line) (float64, bool) { return -30.0, true }
	_, dropped := suppressQuietFragments(lines, -6.0, measure)
	if len(dropped) != 0 {
		t.Error("dropped a full sentence for being quiet; that is somebody speaking softly")
	}
}

// Nothing to echo means nothing to drop: a quiet fragment while the far end is
// silent is just you, quietly.
func TestQuietFragmentWithNoFarEndSpeechIsKept(t *testing.T) {
	lines := []Line{
		sys(0, 5, "right"),
		mic(600, 601, "hmm"),
	}
	measure := func(l Line) (float64, bool) { return -40.0, true }
	_, dropped := suppressQuietFragments(lines, -6.0, measure)
	if len(dropped) != 0 {
		t.Error("dropped a fragment with no far-end speech anywhere near it")
	}
}

// If the audio cannot be measured, keep the line. Losing speech to a failed
// read would be a worse trade than keeping an occasional echo.
func TestUnmeasurableFragmentIsKept(t *testing.T) {
	lines := []Line{
		sys(26, 30, "the old endpoint"),
		mic(28, 29, "alive"),
	}
	measure := func(l Line) (float64, bool) { return 0, false }
	_, dropped := suppressQuietFragments(lines, -6.0, measure)
	if len(dropped) != 0 {
		t.Error("dropped a line whose level could not be measured")
	}
}

func TestMicOnlyRecordingSkipsTheLevelPass(t *testing.T) {
	lines := []Line{mic(0, 1, "one"), mic(2, 3, "two")}
	measure := func(l Line) (float64, bool) { return -99.0, true }
	kept, dropped := suppressQuietFragments(lines, -6.0, measure)
	if len(dropped) != 0 || len(kept) != 2 {
		t.Error("dropped lines from a recording with no far end at all")
	}
}
