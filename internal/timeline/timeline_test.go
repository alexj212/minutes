package timeline

import "testing"

const u = unitsPerSecond // 100ns units per second

// Jitter in the wall clock must not accumulate. This is the bug the package
// exists for: measured before it, a nominal four-second segment at 44100 Hz
// came out 176403 frames instead of 176400, because every jittered packet
// either appended behind the write head or left a gap that was filled, and both
// make the result longer.
func TestClockJitterDoesNotAccumulate(t *testing.T) {
	const rate = 44100
	tr := NewTrack(rate, 0)

	// 400 packets of 441 frames each — exactly 4 seconds — with the wall clock
	// wobbling by up to a millisecond either way in a repeating pattern.
	jitter := []int64{0, +9000, -7000, +5000, -9000, +3000, -3000, +7000}
	var dev uint64
	var last uint64
	for i := 0; i < 400; i++ {
		trueQPC := int64(uint64(i) * 441 * u / rate)
		qpc := uint64(trueQPC + jitter[i%len(jitter)])
		last = tr.Place(qpc, dev)
		dev += 441
	}
	// The last packet starts at frame 399*441.
	if want := uint64(399 * 441); last != want {
		t.Errorf("last packet placed at frame %d, want %d (drift of %+d frames)",
			last, want, int64(last)-int64(want))
	}
	if tr.Reanchors != 0 {
		t.Errorf("re-anchored %d times on ordinary jitter; the tolerance is too tight", tr.Reanchors)
	}
}

// The first packet fixes the track's position relative to the shared epoch.
// A track that starts late must be placed late, or the two tracks lose their
// relationship to each other.
func TestFirstPacketIsPlacedByTheSharedClock(t *testing.T) {
	const rate = 48000
	// Epoch is 2 seconds before this track's first packet.
	tr := NewTrack(rate, 0)
	got := tr.Place(2*u, 0)
	if want := uint64(2 * rate); got != want {
		t.Errorf("a track starting 2s after the epoch was placed at frame %d, want %d", got, want)
	}
}

// Two tracks at different rates starting at the same instant must be placed at
// the same instant.
func TestTracksAtDifferentRatesAgree(t *testing.T) {
	const epoch = 1000 * u
	mic := NewTrack(48000, epoch)
	sys := NewTrack(44100, epoch)

	// Both first deliver 3 seconds after the epoch.
	at := uint64(epoch + 3*u)
	m := mic.Place(at, 0)
	s := sys.Place(at, 0)
	if mSec, sSec := float64(m)/48000, float64(s)/44100; mSec != sSec {
		t.Errorf("mic placed at %vs, system at %vs — they disagree", mSec, sSec)
	}
	if m != 3*48000 || s != 3*44100 {
		t.Errorf("placed at frames %d and %d, want %d and %d", m, s, 3*48000, 3*44100)
	}
}

// A packet stamped before the epoch cannot be placed at a negative offset. It
// belongs at the start, not wrapped around to the end of the recording.
func TestPacketBeforeTheEpochClampsToZero(t *testing.T) {
	tr := NewTrack(48000, 5*u)
	if got := tr.Place(3*u, 0); got != 0 {
		t.Errorf("a packet stamped before the epoch was placed at frame %d, want 0", got)
	}
}

// The guard for a sample counter that stalls while the world does not. Without
// it, audio after such a gap is placed too early and silently, which is the
// failure the whole design exists to prevent.
func TestStalledCounterFallsBackToTheClock(t *testing.T) {
	const rate = 44100
	tr := NewTrack(rate, 0)
	tr.Place(0, 0)
	tr.Place(uint64(u/10), 4410) // 100ms in, counter agrees

	// Five seconds pass on the wall clock, but the counter only advanced by
	// another 100ms worth of frames.
	got := tr.Place(uint64(5*u), 8820)

	if want := uint64(5 * rate); got != want {
		t.Errorf("placed at frame %d, want %d — a stalled counter was trusted over the clock", got, want)
	}
	if tr.Reanchors != 1 {
		t.Errorf("Reanchors = %d, want 1 — the disagreement was not recorded", tr.Reanchors)
	}
}

// After falling back, the counter is trusted again from its new position rather
// than every subsequent packet being forced onto the jittery clock.
func TestReanchorResumesCountingFromTheNewPosition(t *testing.T) {
	const rate = 44100
	tr := NewTrack(rate, 0)
	tr.Place(0, 0)
	tr.Place(uint64(5*u), 4410) // stall -> re-anchor at frame 5*rate

	got := tr.Place(uint64(5*u+u/10), 4410+4410)
	if want := uint64(5*rate + rate/10); got != want {
		t.Errorf("placed at frame %d, want %d", got, want)
	}
	if tr.Reanchors != 1 {
		t.Errorf("Reanchors = %d, want 1 — it re-anchored again when it should have resumed counting", tr.Reanchors)
	}
}

func TestBackwardsCounterFallsBackToTheClock(t *testing.T) {
	const rate = 48000
	tr := NewTrack(rate, 0)
	tr.Place(0, 100000)
	got := tr.Place(uint64(u), 50) // counter went backwards
	if want := uint64(rate); got != want {
		t.Errorf("placed at frame %d, want %d", got, want)
	}
	if tr.Reanchors != 1 {
		t.Errorf("Reanchors = %d, want 1", tr.Reanchors)
	}
}
