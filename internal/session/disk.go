package session

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// FreeBytes reports the space available on the filesystem holding dir.
//
// Walks up to the nearest existing ancestor, because the recording directory is
// created after this is asked — and a check that fails because the thing it is
// checking for does not exist yet is not a check.
func FreeBytes(dir string) (uint64, error) {
	path, err := filepath.Abs(dir)
	if err != nil {
		return 0, err
	}
	for {
		if _, err := os.Stat(path); err == nil {
			break
		}
		parent := filepath.Dir(path)
		if parent == path {
			return 0, fmt.Errorf("no existing ancestor of %s", dir)
		}
		path = parent
	}
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, err
	}
	return st.Bavail * uint64(st.Bsize), nil
}

// Headroom is how long a recording could run before the disk is full.
type Headroom struct {
	FreeBytes      uint64
	BytesPerSecond int
	Seconds        float64
}

// Refuse reports a disk too full to start on.
//
// Fifteen minutes is short enough that no real meeting fits in it, so starting
// anyway would mean filling the disk during the meeting — which costs the
// recording and possibly whatever else is running on the machine.
func (h Headroom) Refuse() bool { return h.Seconds < 15*60 }

// Warn reports a disk that will not last a long meeting.
func (h Headroom) Warn() bool { return h.Seconds < 2*60*60 }

func (h Headroom) String() string {
	return fmt.Sprintf("%.1f GB free — about %s of recording at %.2f GB/hour",
		float64(h.FreeBytes)/1e9, roughDuration(h.Seconds),
		float64(h.BytesPerSecond)*3600/1e9)
}

// EstimateHeadroom works out how long a recording can run in dir.
func EstimateHeadroom(dir string, bytesPerSecond int) (Headroom, error) {
	if bytesPerSecond <= 0 {
		return Headroom{}, fmt.Errorf("invalid capture rate %d", bytesPerSecond)
	}
	free, err := FreeBytes(dir)
	if err != nil {
		return Headroom{}, err
	}
	return Headroom{
		FreeBytes:      free,
		BytesPerSecond: bytesPerSecond,
		Seconds:        float64(free) / float64(bytesPerSecond),
	}, nil
}

func roughDuration(seconds float64) string {
	switch {
	case seconds < 90:
		return fmt.Sprintf("%.0f seconds", seconds)
	case seconds < 2*60*60:
		return fmt.Sprintf("%.0f minutes", seconds/60)
	default:
		return fmt.Sprintf("%.1f hours", seconds/3600)
	}
}

// Live returns the recordings under root that are actually running.
func Live(root string) ([]Status, error) {
	all, err := List(root)
	if err != nil {
		return nil, err
	}
	var out []Status
	for _, st := range all {
		if st.Live {
			out = append(out, st)
		}
	}
	return out, nil
}

// DirSize is how much a recording occupies.
//
// Walked rather than tracked in the manifest: the manifest records the size of
// each segment, but a recording directory also holds the transcript, the log
// and whatever a later phase leaves there, and the number people want is the
// one that matches `du`.
func DirSize(dir string) (int64, error) {
	var total int64
	err := filepath.Walk(dir, func(_ string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() {
			total += info.Size()
		}
		return nil
	})
	return total, err
}

// HumanBytes renders a size the way a person reads one.
func HumanBytes(n int64) string {
	const unit = 1000
	if n < unit {
		return fmt.Sprintf("%dB", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f%cB", float64(n)/float64(div), "kMGT"[exp])
}
