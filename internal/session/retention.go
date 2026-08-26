package session

import (
	"fmt"
	"time"
)

// Deciding what to delete.
//
// Recordings accumulate at a measured 1.33 GB/hour and nothing removes them.
// But deleting somebody's meetings is worse than using their disk, so the
// policy is off unless configured, refuses anything still running, and by
// default refuses anything whose notes were never delivered — because that is
// the only copy of a meeting nobody has read.

// Retention is the policy.
type Retention struct {
	// KeepDays removes recordings older than this. Zero means no age limit.
	KeepDays int `json:"keepDays,omitempty"`
	// KeepCount keeps only the newest N. Zero means no count limit.
	KeepCount int `json:"keepCount,omitempty"`
	// KeepUndelivered protects recordings whose notes never went anywhere.
	// True by default, and the reason is that an undelivered recording is the
	// only record of a meeting nobody has read.
	KeepUndelivered bool `json:"keepUndelivered"`
}

// Enabled reports whether the policy would ever remove anything.
func (r Retention) Enabled() bool { return r.KeepDays > 0 || r.KeepCount > 0 }

// Candidate is a recording the policy would remove, and why.
type Candidate struct {
	Status
	Reason string
	Size   int64
}

// Doomed returns what the policy selects from all, newest first, along with the
// recordings it deliberately spared and why.
func (r Retention) Doomed(all []Status, now time.Time) (doomed []Candidate, spared []Candidate) {
	for i, st := range all {
		var reason string
		switch {
		case r.KeepCount > 0 && i >= r.KeepCount:
			reason = fmt.Sprintf("older than the %d most recent", r.KeepCount)
		case r.KeepDays > 0 && now.Sub(st.StartedAt) > time.Duration(r.KeepDays)*24*time.Hour:
			reason = fmt.Sprintf("older than %d days", r.KeepDays)
		default:
			continue
		}

		size, _ := DirSize(st.Dir())
		c := Candidate{Status: st, Reason: reason, Size: size}

		// A recording still being written to, or still being transcribed, is
		// not old — it is in use, whatever its timestamp says.
		if st.Live {
			c.Reason = "still running"
			spared = append(spared, c)
			continue
		}
		if r.KeepUndelivered && st.Delivery == nil {
			c.Reason = "notes were never delivered"
			spared = append(spared, c)
			continue
		}
		doomed = append(doomed, c)
	}
	return doomed, spared
}
