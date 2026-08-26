package session

import (
	"encoding/json"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// Saying out loud that this machine is recording.
//
// `minutes list` will tell you, but only if you think to ask, and the whole
// point is the times you do not think to ask. A recording that outlives the
// terminal that started it is exactly the one somebody forgets is running —
// which on a real call meant thirteen minutes of a family conversation ending
// up in a work transcript.
//
// So a marker file at a fixed, boring path that anything can read: a shell
// prompt, a status bar, a cron job, a person with `cat`.

// Marker describes the recording currently running on this machine.
type Marker struct {
	ID        string    `json:"id"`
	Name      string    `json:"name,omitempty"`
	Dir       string    `json:"dir"`
	PID       int       `json:"pid"`
	StartedAt time.Time `json:"startedAt"`
}

// MarkerPath is where the marker lives.
func MarkerPath() string {
	if v := os.Getenv("MINUTES_MARKER"); v != "" {
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(os.TempDir(), "minutes-recording")
	}
	return filepath.Join(home, ".config", "minutes", "recording")
}

// SetMarker records that a recording has started.
func SetMarker(m Marker) error {
	path := MarkerPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	body, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	// Written and renamed, so a reader never sees half a marker and concludes
	// the machine is not recording.
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(body, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// ClearMarker removes it.
func ClearMarker() error {
	err := os.Remove(MarkerPath())
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

// ReadMarker returns the current recording, if there is one.
//
// A marker whose process is gone is treated as absent and removed: a recorder
// killed without cleaning up would otherwise leave the machine claiming to be
// recording forever, and a warning that is permanently on is a warning nobody
// reads.
func ReadMarker() (Marker, bool) {
	body, err := os.ReadFile(MarkerPath())
	if err != nil {
		return Marker{}, false
	}
	var m Marker
	if err := json.Unmarshal(body, &m); err != nil {
		_ = ClearMarker()
		return Marker{}, false
	}
	if m.PID > 0 {
		if err := syscall.Kill(m.PID, 0); err != nil {
			_ = ClearMarker()
			return Marker{}, false
		}
	}
	return m, true
}
