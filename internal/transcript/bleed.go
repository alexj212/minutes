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

// SuppressBleed removes microphone lines that are echoes of system lines, and
// reports how many it dropped.
//
// The count is returned rather than swallowed. Silently discarding transcript
// lines would be a bad thing to do quietly, and a large number here means the
// meeting was on speakers — worth knowing when reading the result.
func SuppressBleed(lines []Line) ([]Line, int) {
	system := make([]Line, 0, len(lines))
	for _, l := range lines {
		if l.Track != "mic" {
			system = append(system, l)
		}
	}
	if len(system) == 0 {
		return lines, 0
	}

	out := make([]Line, 0, len(lines))
	dropped := 0
	for _, l := range lines {
		if l.Track == "mic" && isEchoOf(l, system) {
			dropped++
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
		if containment(a, words(s.Text)) >= bleedSimilarity {
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
