package transcript

import (
	"strings"
	"unicode"
)

// Speaker attribution is free only while the two tracks hold different audio.
// On a machine playing the meeting through speakers rather than headphones, the
// microphone also hears the far end, so the same sentence is transcribed on
// both tracks and arrives attributed to both people.
//
// That is worse than losing it. A transcript that omits a line is incomplete; a
// transcript that says you said what somebody else said is wrong, and nothing
// about it looks wrong. So the echo is removed from the microphone track, which
// is the copy that came through the air and is the worse recording of the two.
//
// Only that direction is handled. Conferencing software suppresses echoing your
// voice back to you, so the system track does not acquire copies of yours.

// bleedOverlap is how far apart two lines may be and still be the same words
// arriving twice. The acoustic path is milliseconds; the slack is for the
// segment boundaries a speech model chooses, which differ between the two
// transcriptions of the same speech.
const bleedOverlap = 2.0

// bleedSimilarity is how alike two lines must be to count as the same words.
//
// Containment rather than equality: the two transcriptions of one sentence
// rarely agree exactly, and one is often a fragment of the other because the
// model split it differently.
const bleedSimilarity = 0.6

// bleedRunWords is a contiguous run of words shared with the far end, at which
// a microphone line is an echo whatever its overall similarity says.
//
// The containment test alone is the wrong instrument, and `minutes-mac` proved
// it by accident: their far-end audio was one script looped ten times, nine
// echoes were caught, and one escaped. Not a fragment — a complete, confident
// 84-character sentence, which is the most credible thing in a transcript to
// attribute to the wrong person. Whisper had segmented the two tracks
// differently, so the far-end line absorbed the tail of the previous sentence
// and cut early, and containment came out 9/17 = 0.529 against a 0.6 cutoff.
//
// Whisper's segmentation is not stable across two recordings of the same audio,
// so any test on whole-line similarity is measuring where the model chose to
// cut. A contiguous run is not: the same words in the same order arrive in both
// transcriptions however they are chopped.
//
// Five is measured, not chosen. Across 700+ microphone lines of real meetings
// on speakers, lines with no textual relationship to the far end (containment
// below 0.35) reached a shared run of five words ZERO times, while confirmed
// echoes clustered at four to eight and beyond. The escaped line shared nine:
// "i will put the numbers in the shared folder".
//
// Two other candidates were measured and rejected. The acoustic level test does
// not separate these at all — on both recordings the confirmed echoes were as
// loud as the operator, 177 and 313 of them, because the speakers were loud
// enough that an echo arrives at nearly full level. And a pure time-overlap
// test would have deleted 182 lines of the operator genuinely talking over the
// far end in a single meeting.
const bleedRunWords = 5

// SuppressBleed removes microphone lines that are echoes of system lines, and
// reports how many it dropped.
//
// The count is returned rather than swallowed. Silently discarding transcript
// lines would be a bad thing to do quietly, and a large number here means the
// meeting was on speakers — worth knowing when reading the result.
func SuppressBleed(lines []Line) (kept, dropped []Line) {
	system := make([]Line, 0, len(lines))
	for _, l := range lines {
		if l.Track != "mic" {
			system = append(system, l)
		}
	}
	if len(system) == 0 {
		return lines, nil
	}

	out := make([]Line, 0, len(lines))
	for _, l := range lines {
		if l.Track == "mic" && isEchoOf(l, system) {
			dropped = append(dropped, l)
			continue
		}
		out = append(out, l)
	}
	return out, dropped
}

func isEchoOf(l Line, system []Line) bool {
	a := words(l.Text)
	if len(a) == 0 {
		return false
	}
	for _, s := range system {
		// Overlapping in time, allowing for the two models cutting the same
		// speech at different points.
		if s.End < l.Start-bleedOverlap || s.Start > l.End+bleedOverlap {
			continue
		}
		b := words(s.Text)
		if containment(a, b) >= bleedSimilarity {
			return true
		}
		if longestRun(a, b) >= bleedRunWords {
			return true
		}
	}
	return false
}

// words normalises a line to comparable tokens: case and punctuation differ
// between two transcriptions of the same sentence and mean nothing here.
func words(s string) []string {
	fields := strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsNumber(r)
	})
	return fields
}

// containment is the share of the smaller line's words that appear in the
// larger. It is deliberately not symmetric-averaged: a short line wholly inside
// a long one is an echo, and treating it as only half a match would keep it.
func containment(a, b []string) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	counts := map[string]int{}
	for _, w := range b {
		counts[w]++
	}
	shared := 0
	for _, w := range a {
		if counts[w] > 0 {
			counts[w]--
			shared++
		}
	}
	smaller := len(a)
	if len(b) < smaller {
		smaller = len(b)
	}
	return float64(shared) / float64(smaller)
}

// longestRun is the longest run of words appearing contiguously in both lines.
//
// Robust to the thing containment is not: where a speech model chose to cut.
// Two transcriptions of the same speech disagree about sentence boundaries and
// therefore about whole-line similarity, but they agree about the words in the
// middle and their order.
func longestRun(a, b []string) int {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	best := 0
	prev := make([]int, len(b)+1)
	cur := make([]int, len(b)+1)
	for i := 1; i <= len(a); i++ {
		for j := 1; j <= len(b); j++ {
			if a[i-1] == b[j-1] {
				cur[j] = prev[j-1] + 1
				if cur[j] > best {
					best = cur[j]
				}
			} else {
				cur[j] = 0
			}
		}
		prev, cur = cur, prev
	}
	return best
}

// maxFragmentWords bounds what counts as a fragment.
//
// Only very short lines are eligible for the level test. A full sentence at a
// low level is somebody speaking quietly, which belongs in the transcript; a
// stray word or two at a low level, while the other side is talking, is the
// far end arriving through the air.
const maxFragmentWords = 3

// quietMarginDB is how far below your normal speaking level a fragment must be
// before it is treated as an echo.
//
// Twelve decibels is roughly a quarter of the amplitude. Speaking softly does
// not get you there; a room away does.
const quietMarginDB = 12.0

// suppressQuietFragments removes short microphone lines that are much quieter
// than the operator's own speech and land while the far end is talking.
//
// This is the second pass. The first compares words, and cannot catch a
// fragment whose words are missing from the far-end transcript — which is how
// "all", the tail of "...the old endpoint alive", ended up attributed to the
// wrong person.
func suppressQuietFragments(lines []Line, reference float64, measure func(Line) (float64, bool)) (kept, dropped []Line) {
	system := make([]Line, 0, len(lines))
	for _, l := range lines {
		if l.Track != "mic" {
			system = append(system, l)
		}
	}
	if len(system) == 0 {
		return lines, nil
	}

	out := make([]Line, 0, len(lines))
	for _, l := range lines {
		if l.Track == "mic" && len(words(l.Text)) <= maxFragmentWords && overlapsFarEnd(l, system) {
			if db, ok := measure(l); ok && db <= reference-quietMarginDB {
				dropped = append(dropped, l)
				continue
			}
		}
		out = append(out, l)
	}
	return out, dropped
}

// overlapsFarEnd reports whether the far end was talking at the same moment.
func overlapsFarEnd(l Line, system []Line) bool {
	for _, s := range system {
		if s.End >= l.Start-bleedOverlap && s.Start <= l.End+bleedOverlap {
			return true
		}
	}
	return false
}
