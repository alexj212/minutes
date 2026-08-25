package transcript

import "fmt"

// A meeting is not always a meeting.
//
// When the far end drops — a reboot, a dropped call, someone stepping away —
// the recorder keeps going, and everything the microphone hears in that window
// lands in the transcript attributed to you, indistinguishable from something
// you said in the meeting. Observed on a real two-hour call: thirteen minutes
// of private household conversation, including a child, sitting in the middle
// of a work transcript with nothing to mark it.
//
// That is not something to delete on the recorder's own judgment — those
// minutes might equally be you thinking aloud, and losing that would be its own
// failure. But it must be visible, because the person deciding what to do with
// a transcript cannot decide well if the two are mixed together.

// farEndSilentThreshold is how long the other side must say nothing before the
// stretch is marked.
//
// Two minutes: long enough that ordinary pauses, someone presenting, or a
// slow-moving discussion do not trip it, short enough to catch a call that
// actually dropped.
const farEndSilentThreshold = 120.0

// Silence is a stretch where only your microphone was producing speech.
type Silence struct {
	Start float64 `json:"start"`
	End   float64 `json:"end"`
	// Lines is how much of the transcript falls inside it.
	Lines int `json:"lines"`
}

// Duration in seconds.
func (s Silence) Duration() float64 { return s.End - s.Start }

// findFarEndSilence returns the stretches where the system track carried no
// speech while the microphone did.
//
// Derived from the transcript rather than the audio: what matters here is
// whether anything the other side said ended up in the record, which is exactly
// what the transcript answers.
func findFarEndSilence(lines []Line) []Silence {
	// With nothing on the system track at all this is a one-sided recording,
	// not a meeting the far end dropped out of, and marking the whole thing
	// would say nothing useful.
	hasSystem := false
	for _, l := range lines {
		if l.Track != "mic" {
			hasSystem = true
			break
		}
	}
	if !hasSystem {
		return nil
	}

	var out []Silence
	var lastSystemEnd float64
	var pending []Line

	flush := func(nextSystemStart float64) {
		if len(pending) == 0 {
			return
		}
		if nextSystemStart-lastSystemEnd < farEndSilentThreshold {
			pending = nil
			return
		}
		out = append(out, Silence{
			Start: lastSystemEnd,
			End:   nextSystemStart,
			Lines: len(pending),
		})
		pending = nil
	}

	for _, l := range lines {
		if l.Track == "mic" {
			pending = append(pending, l)
			continue
		}
		flush(l.Start)
		if l.End > lastSystemEnd {
			lastSystemEnd = l.End
		}
	}
	// A recording that ends with the far end already gone.
	if len(pending) > 0 {
		end := lastSystemEnd
		for _, l := range pending {
			if l.End > end {
				end = l.End
			}
		}
		flush(end)
	}
	return out
}

// markFarEndSilence tags every line that falls inside one of the stretches.
func markFarEndSilence(lines []Line, gaps []Silence) {
	for i := range lines {
		for _, g := range gaps {
			if lines[i].Start >= g.Start && lines[i].Start < g.End {
				lines[i].FarEndSilent = true
				break
			}
		}
	}
}

// describeSilence renders the banner shown in the readable transcript.
func describeSilence(s Silence) string {
	return fmt.Sprintf("── the other side was silent for %s from here — "+
		"anything below may be the room, not the meeting ──", roughMinutes(s.Duration()))
}

func roughMinutes(seconds float64) string {
	if seconds < 120 {
		return fmt.Sprintf("%.0f seconds", seconds)
	}
	return fmt.Sprintf("%.0f minutes", seconds/60)
}
