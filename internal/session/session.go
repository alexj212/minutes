// Package session is the lifecycle of a recording: start it, stop it, and find
// out what happened.
//
// A meeting outlasts the command that starts it, so `start` leaves a detached
// supervisor behind and returns. The supervisor owns the recording directory
// from then on; everything else reads the manifest.
//
// State lives in the recording directory rather than in a daemon's memory. A
// supervisor that dies leaves a directory that still says what it was doing and
// still holds the audio it captured, which is the difference between losing a
// meeting and losing the end of one.
package session

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/alexj212/minutes/internal/manifest"
)

const (
	// PIDFile names the supervisor's process id inside a recording directory.
	PIDFile = "recorder.pid"
	// LogFile is where a detached supervisor's output goes, since it has no
	// terminal to write to.
	LogFile = "recorder.log"
)

// DefaultSegment is the chunk length. Five minutes bounds what an interrupted
// recording loses to the last few seconds, while keeping the file count for a
// long meeting in the dozens rather than the thousands.
const DefaultSegment = 5 * time.Minute

var slugUnsafe = regexp.MustCompile(`[^a-zA-Z0-9]+`)

// NewID builds a recording id from the time and an optional name.
//
// Time first so the directory listing sorts chronologically, which is the order
// anybody looking for a meeting actually wants.
func NewID(at time.Time, name string) string {
	id := at.Format("2006-01-02-150405")
	if s := strings.Trim(slugUnsafe.ReplaceAllString(name, "-"), "-"); s != "" {
		id += "-" + strings.ToLower(s)
	}
	return id
}

// StartOptions configures a detached recording.
type StartOptions struct {
	Root    string
	Name    string
	Segment time.Duration
	Helper  string
	// AppPID captures only that process and its children. Zero means
	// everything the machine plays.
	AppPID int
	App    string
}

// Start creates a recording directory and leaves a supervisor running in it.
//
// The caller is expected to have run preflight already: starting a supervisor
// that will immediately refuse would put the explanation in a log file nobody
// is watching.
func Start(opt StartOptions) (*manifest.Manifest, error) {
	if opt.Segment <= 0 {
		opt.Segment = DefaultSegment
	}
	id := NewID(time.Now(), opt.Name)
	dir := filepath.Join(opt.Root, id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}

	m := manifest.New(dir, id, opt.Name, opt.Segment.Seconds())
	m.App = opt.App
	if err := m.Save(); err != nil {
		return nil, err
	}

	self, err := os.Executable()
	if err != nil {
		return nil, err
	}
	logf, err := os.Create(filepath.Join(dir, LogFile))
	if err != nil {
		return nil, err
	}
	defer logf.Close()

	args := []string{"supervise", "--dir", dir, "--helper", opt.Helper}
	if opt.AppPID > 0 {
		args = append(args, "--app-pid", strconv.Itoa(opt.AppPID))
	}
	cmd := exec.Command(self, args...)
	cmd.Stdout = logf
	cmd.Stderr = logf
	// Its own session, so the recording outlives the shell that started it.
	// A meeting is longer than a command.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("starting supervisor: %w", err)
	}
	pid := cmd.Process.Pid
	if err := os.WriteFile(filepath.Join(dir, PIDFile), []byte(strconv.Itoa(pid)), 0o644); err != nil {
		return nil, err
	}
	// The parent must not wait on it; releasing lets it be reparented rather
	// than left as a zombie when this process exits.
	if err := cmd.Process.Release(); err != nil {
		return nil, err
	}
	return m, nil
}

// PID returns the supervisor's process id, and whether it is still alive.
func PID(dir string) (int, bool) {
	b, err := os.ReadFile(filepath.Join(dir, PIDFile))
	if err != nil {
		return 0, false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || pid <= 0 {
		return 0, false
	}
	// Signal 0 tests for existence without delivering anything.
	if err := syscall.Kill(pid, 0); err != nil {
		return pid, false
	}
	return pid, true
}

// ErrNotRecording is returned when there is no live supervisor to stop.
var ErrNotRecording = errors.New("no recording is running in that directory")

// Stop asks the supervisor to finish and waits for it.
//
// SIGTERM rather than SIGKILL: the supervisor closes the helper's stdin, lets
// it emit the packet in hand, closes the open segments and writes the final
// manifest. Killing it outright would cost the last chunk for no reason.
func Stop(dir string, timeout time.Duration) (*manifest.Manifest, error) {
	pid, alive := PID(dir)
	if !alive {
		return nil, ErrNotRecording
	}
	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
		return nil, fmt.Errorf("signalling supervisor %d: %w", pid, err)
	}

	// Waits for capture to finish, not for the supervisor to exit.
	//
	// The supervisor carries on into transcription, which runs at about
	// real time — so waiting for the process would hang the terminal for the
	// length of the meeting. Once the state leaves "recording" the audio is
	// complete on disk and there is nothing left to wait for.
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if m, err := manifest.Load(dir); err == nil && m.State != manifest.StateRecording {
			return m, nil
		}
		if _, alive := PID(dir); !alive {
			return manifest.Load(dir)
		}
		time.Sleep(50 * time.Millisecond)
	}
	return nil, fmt.Errorf("supervisor %d did not stop within %s", pid, timeout)
}

// Status is a manifest plus what is true of it right now.
type Status struct {
	*manifest.Manifest
	PID int
	// Live is whether a supervisor is actually running. A manifest that says
	// "recording" with no live supervisor was interrupted, and saying so is the
	// point of checking rather than trusting the file.
	Live bool
}

// Interrupted reports a recording that claims to be working but is not.
//
// Covers transcribing as well as recording: a supervisor that died partway
// through a transcript leaves a manifest saying it is still going, and the fix
// differs — an interrupted recording is as long as it is, while an interrupted
// transcript can simply be run again.
func (s Status) Interrupted() bool {
	if s.Live {
		return false
	}
	return s.State == manifest.StateRecording || s.State == manifest.StateTranscribing
}

// StateLabel is the honest one-word state, reconciling the manifest against
// whether a supervisor exists.
func (s Status) StateLabel() string {
	if s.Interrupted() {
		return "interrupted"
	}
	return string(s.State)
}

// Open loads one recording directory.
func Open(dir string) (Status, error) {
	m, err := manifest.Load(dir)
	if err != nil {
		return Status{}, err
	}
	pid, live := PID(dir)
	return Status{Manifest: m, PID: pid, Live: live}, nil
}

// List returns every recording under root, newest first.
//
// A directory without a readable manifest is skipped rather than failing the
// listing: one damaged recording must not hide the others.
func List(root string) ([]Status, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []Status
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		st, err := Open(filepath.Join(root, e.Name()))
		if err != nil {
			continue
		}
		out = append(out, st)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].StartedAt.After(out[j].StartedAt) })
	return out, nil
}

// Resolve turns a recording reference into a directory.
//
// An empty ref, or "latest", means the most recent recording — and when one is
// running, that one, since "stop" almost always means "stop what is recording".
func Resolve(root, ref string) (string, error) {
	if ref != "" && ref != "latest" {
		dir := filepath.Join(root, ref)
		if _, err := os.Stat(filepath.Join(dir, manifest.Name)); err != nil {
			return "", fmt.Errorf("no recording %q under %s", ref, root)
		}
		return dir, nil
	}
	all, err := List(root)
	if err != nil {
		return "", err
	}
	if len(all) == 0 {
		return "", fmt.Errorf("no recordings under %s", root)
	}
	for _, st := range all {
		if st.Live {
			return st.Dir(), nil
		}
	}
	return all[0].Dir(), nil
}
