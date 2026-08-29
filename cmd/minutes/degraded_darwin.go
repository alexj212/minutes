//go:build darwin

package main

import (
	"context"
	"os/exec"
	"strings"
	"time"
)

// helperDegraded reports why this helper works here but will not work as well
// on the machine that installs it.
//
// On macOS the answer is about signing, and it is not cosmetic. TCC attaches
// its audio-capture decision to the binary's designated requirement. An
// unsigned binary gets an ad-hoc signature whose requirement is a bare cdhash
// — a hash of the bytes — so it matches only that exact copy, and the operator
// is asked for permission again after every rebuild. A real identity gives a
// requirement naming the identifier and the certificate, which does not move
// when the code does. Measured: a change that moved the cdhash from 64589417…
// to 384c54f5… left the requirement byte-identical, and the grant survived.
//
// So an ad-hoc helper is `present: true`, correct, checksummed and complete,
// and still costs whoever installs it a permission prompt they cannot avoid.
// That is worth saying before it is distributed rather than after.
//
// # This asks the binary, and never the build
//
// The tempting implementation is for build.sh to record that it fell back to
// ad-hoc, and for this to repeat it. That is manifest.Platform again: true when
// written, in a document that outlives the moment it was true. The helper on
// disk at publish time is not necessarily the one build.sh produced — somebody
// rebuilds, copies one in, restores a backup, or `make dist` runs against a
// stale dist/. A set built once with a real identity and rebuilt ad-hoc would
// then publish claiming a signature it no longer has, and that failure runs in
// the dangerous direction.
//
// # Three outcomes, not two
//
// Signed with an identity, ad-hoc, and *could not be determined* are different
// answers. Collapsing the third into the first is how a check becomes a check
// that always passes — the same argument as a track that was never asked about.
func helperDegraded(path string) string {
	if path == "" {
		return "cannot check the signature: no helper path"
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// The designated requirement is the property TCC actually matches on, so
	// it is the one worth reading. `codesign` writes it to stderr.
	out, err := exec.CommandContext(ctx, "/usr/bin/codesign",
		"-d", "--requirements", "-", path).CombinedOutput()
	text := string(out)

	if err != nil {
		// An unsigned binary is a definite answer, not an unknown one: it is
		// the worst case, and saying so is more use than saying nothing.
		if strings.Contains(text, "not signed at all") {
			return "not signed at all: nothing identifies this binary to whoever " +
				"receives it, and a Mac that downloads it may refuse to run it"
		}
		return "could not determine the signature: " + firstLine(text)
	}

	designated := ""
	for _, line := range strings.Split(text, "\n") {
		if i := strings.Index(line, "designated =>"); i >= 0 {
			designated = strings.TrimSpace(line[i+len("designated =>"):])
			break
		}
	}
	if designated == "" {
		return "could not determine the signature: codesign reported no designated requirement"
	}

	// A requirement naming an identifier, an anchor or a certificate names an
	// author. One that is only a cdhash names nothing but the bytes.
	//
	// These strings used to say an ad-hoc helper would make macOS ask for
	// recording permission on the installing machine and after every rebuild.
	// That was wrong, and it was wrong in the worst place: this text is carried
	// by shabadoo to a stranger at install time, where nobody can check it.
	//
	// Measured directly on the target Mac, same session, same responsible
	// process, back to back: a properly signed helper and an ad-hoc one both
	// cost zero prompts. TCC keys the grant on the RESPONSIBLE process — the
	// launcher — and the helper is only ever the accessing process. The earlier
	// belief came from a before-and-after window in which the launcher itself
	// was being rebuilt several times a day, and the variable that moved was
	// never the helper.
	//
	// So what signing actually buys, and what these strings now say: an
	// identity a recipient can resolve, integrity that is checkable because it
	// is signed, and not being stopped by Gatekeeper on a Mac that downloaded
	// the file. A durable consent grant belongs to the launcher, which is not
	// this project's binary.
	if strings.Contains(designated, "anchor ") || strings.Contains(designated, "certificate ") {
		return ""
	}
	if strings.Contains(designated, "cdhash") {
		return "ad-hoc signed: its designated requirement is a bare hash of these " +
			"bytes, so it names no author a recipient can resolve and Gatekeeper " +
			"may refuse it on a Mac that downloaded it"
	}
	return "could not determine the signature: unrecognised designated requirement " + designated
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	if s == "" {
		return "codesign said nothing"
	}
	return s
}
