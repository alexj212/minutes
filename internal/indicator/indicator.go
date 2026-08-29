// Package indicator puts a recording on screen while it is happening.
//
// The rest of this program discloses a recording after the fact: a marker file,
// a notification when it starts and stops, a line in the transcript. All of
// those are true and none of them is visible during the meeting, which is the
// only time somebody in the room could object.
//
// A tray icon is the one form of disclosure that is continuously true rather
// than periodically asserted. That distinction is also what makes the macOS
// consent wart tolerable — there, the one moment the OS says "this program
// wants to record you" it names the launcher rather than the recorder, and
// something else has to carry the disclosure.
//
// It is deliberately not required. A recording whose indicator failed to start
// is still a recording, and refusing to record because a tray icon would not
// draw would be choosing the wrong thing to protect. The failure is reported
// and the meeting goes ahead.
package indicator

import (
	"bufio"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
)

// Indicator is a running tray helper.
type Indicator struct {
	cmd   *exec.Cmd
	stdin *os.File
	// read closes when the stdout reader has finished.
	//
	// os/exec documents that calling Wait before every read from StdoutPipe has
	// completed is incorrect — Wait closes the pipe, and a reader still in it
	// loses whatever was left. The race detector does not catch it and today it
	// costs nothing, because the tray sends nothing after STOP. It would cost a
	// dropped message the day it does.
	read chan struct{}
	once sync.Once
}

// Options configures one.
type Options struct {
	// Helper is the tray binary. Empty means there is none, and that is not an
	// error: this platform may not have one.
	Helper string
	// Name and Dir are what the icon shows: the meeting being recorded and
	// where its files are.
	Name string
	Dir  string
	// OnStop is called when the operator asks for the recording to end.
	//
	// The tray does not stop anything itself. Deciding to stop is the
	// operator's and carrying it out belongs to whoever owns the recording and
	// its manifest — a tray icon that killed the capture helper would produce
	// exactly the half-written recording the rest of this program exists to
	// avoid.
	OnStop func()
	Log    func(string, ...any)
}

// FindHelper locates the tray binary beside a given capture helper.
//
// Beside, rather than on PATH: the two are one release set, and a tray from a
// different version is worse than none. Returns empty when there is not one,
// which is not an error.
func FindHelper(captureHelper string) string {
	if captureHelper == "" {
		return ""
	}
	dir := filepath.Dir(captureHelper)
	for _, name := range []string{"minutes-tray.exe", "minutes-tray"} {
		p := filepath.Join(dir, name)
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p
		}
	}
	return ""
}

// Start shows the icon, or reports why it could not.
//
// A nil Indicator is usable and Stop on it does nothing, so a caller never has
// to decide whether the indicator came up before cleaning up after it.
func Start(ctx context.Context, opt Options) (*Indicator, error) {
	if opt.Log == nil {
		opt.Log = func(string, ...any) {}
	}
	if opt.Helper == "" {
		return nil, nil
	}

	args := []string{}
	if opt.Name != "" {
		args = append(args, "--name", opt.Name)
	}
	if opt.Dir != "" {
		args = append(args, "--dir", opt.Dir)
	}
	cmd := exec.Command(opt.Helper, args...)
	// Its own process group, for the same reason the capture helper gets one:
	// a Ctrl-C in the terminal signals the whole foreground group, and a tray
	// that dies in that leaves an icon behind with no process to remove it.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	r, w, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	cmd.Stdin = r
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		r.Close()
		w.Close()
		return nil, err
	}
	r.Close()

	ind := &Indicator{cmd: cmd, stdin: w, read: make(chan struct{})}

	go func() {
		defer close(ind.read)
		sc := bufio.NewScanner(stdout)
		for sc.Scan() {
			switch strings.TrimSpace(sc.Text()) {
			case "READY":
				opt.Log("recording is showing in the tray")
			case "STOP":
				opt.Log("stop requested from the tray")
				if opt.OnStop != nil {
					// In its own goroutine, and this is load-bearing rather
					// than tidiness. OnStop belongs to the caller and may
					// block — a handler that stops a recording and waits for
					// the manifest to be written is the obvious case. Run
					// inline, such a handler wedges shutdown permanently: Stop
					// waits for this reader to finish and this reader is inside
					// the handler waiting for Stop. Nothing times out and
					// nothing reports it.
					go opt.OnStop()
				}
			}
		}
	}()

	go func() {
		<-ctx.Done()
		ind.Stop()
	}()

	return ind, nil
}

// Stop removes the icon.
//
// By closing stdin rather than signalling, which is the same contract the
// capture helper has: it lets the helper take its own icon out of the tray on
// the way past. A killed tray helper leaves a dead icon that only disappears
// when somebody hovers over it.
func (i *Indicator) Stop() {
	if i == nil {
		return
	}
	i.once.Do(func() {
		i.stdin.Close()
		<-i.read
		_ = i.cmd.Wait()
	})
}
