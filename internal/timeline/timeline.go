// Package timeline decides where a captured packet belongs.
//
// Two clocks are available for every packet, and they are good at different
// things.
//
// `qpcPosition` is wall-clock: it is the only thing the microphone and the
// system stream have in common, so it is what relates one track to the other.
// But it is the time the endpoint was *read*, and it carries a millisecond or
// so of jitter.
//
// `devicePosition` is the endpoint's own sample counter. It has no jitter at
// all — measured on the target machine it tracked wall-clock to 0.1 ms over 13
// seconds — but it counts from whenever that stream started, so it says nothing
// about the other track.
//
// Placing every packet by wall-clock accumulates the jitter, and it accumulates
// in one direction: a packet landing behind the write head is appended, one
// landing ahead leaves a gap that is filled. Both make the file longer, never
// shorter, so a track drifts steadily long. Measured before this existed, a
// nominal four-second segment came out 176403 frames instead of 176400.
//
// So: wall-clock once per track, to place its beginning relative to the shared
// epoch, and the sample counter for everything after.
package timeline

// unitsPerSecond is the resolution of a qpc position: 100-nanosecond units.
const unitsPerSecond = 10_000_000

// Track places one track's packets on the shared timeline.
type Track struct {
	rate  uint64
	epoch uint64

	anchored    bool
	devAnchor   uint64
	frameAnchor uint64

	// clockOnly places every packet by wall-clock, for tracks whose device
	// counter measures delivered frames rather than elapsed time.
	clockOnly bool

	// Reanchors counts how often the two clocks disagreed enough to fall back
	// to wall-clock. It is reported rather than hidden: a non-zero value means
	// the stream was interrupted in a way worth knowing about.
	Reanchors int
}

// NewTrack creates a placer for a track at the given sample rate, measuring
// from a shared epoch expressed as a qpc position.
func NewTrack(rate, epoch uint64) *Track {
	return &Track{rate: rate, epoch: epoch}
}

// NewClockTrack creates a placer that uses wall-clock only.
//
// For a track scoped to one process, the device counter counts frames
// delivered rather than time elapsed: the stream delivers nothing while its
// target is quiet, so the counter falls behind by however long nobody spoke.
// That is not drift to be corrected — it is the counter measuring a different
// thing — and feeding it to the usual placer makes the guard fire continuously.
// Measured: 98 re-anchors in ten seconds.
func NewClockTrack(rate, epoch uint64) *Track {
	return &Track{rate: rate, epoch: epoch, clockOnly: true}
}

// tolerance is how far the sample counter may drift from wall-clock before
// wall-clock wins.
//
// A tenth of a second is far wider than jitter — which is milliseconds — and
// far narrower than an idle gap, which is however long nobody spoke. It exists
// for a case that could not be reproduced on the target machine: a render
// endpoint going idle mid-recording, where the sample counter might stop while
// the world does not. If that happens, audio after the gap would otherwise be
// placed too early, silently, which is exactly the failure this design is
// supposed to prevent.
func (t *Track) tolerance() uint64 { return t.rate / 10 }

// byClock converts a qpc position into a frame offset from the epoch.
func (t *Track) byClock(qpc uint64) uint64 {
	if qpc <= t.epoch {
		return 0
	}
	return (qpc - t.epoch) * t.rate / unitsPerSecond
}

// Place returns the frame offset a packet belongs at.
func (t *Track) Place(qpc, dev uint64) uint64 {
	clock := t.byClock(qpc)

	if t.clockOnly {
		return clock
	}

	if !t.anchored {
		t.anchored = true
		t.devAnchor = dev
		t.frameAnchor = clock
		return clock
	}

	// A counter that went backwards is not a counter worth trusting.
	if dev < t.devAnchor {
		t.Reanchors++
		t.devAnchor = dev
		t.frameAnchor = clock
		return clock
	}

	counted := t.frameAnchor + (dev - t.devAnchor)

	var apart uint64
	if counted > clock {
		apart = counted - clock
	} else {
		apart = clock - counted
	}
	if apart > t.tolerance() {
		t.Reanchors++
		t.devAnchor = dev
		t.frameAnchor = clock
		return clock
	}
	return counted
}
