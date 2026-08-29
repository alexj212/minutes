//go:build darwin

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// signAs copies a small real binary and signs the copy, returning its path.
func signAs(t *testing.T, args ...string) string {
	t.Helper()
	src, err := os.ReadFile("/bin/echo")
	if err != nil {
		t.Skipf("no binary to copy: %v", err)
	}
	p := filepath.Join(t.TempDir(), "helper")
	if err := os.WriteFile(p, src, 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("/usr/bin/codesign", append(args, p)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("codesign %v failed: %v: %s", args, err, out)
	}
	return p
}

// signingIdentity returns a codesigning identity, or "" if this machine has none.
func signingIdentity(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("/usr/bin/security", "find-identity", "-v", "-p", "codesigning").Output()
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) >= 2 && strings.HasSuffix(f[0], ")") && len(f[1]) == 40 {
			return f[1]
		}
	}
	return ""
}

// The two signing states must produce different answers.
//
// A check that only knew the ad-hoc case would pass just as happily if this
// function returned the warning unconditionally, and one that only knew the
// signed case would pass if it always returned "". Neither half is evidence on
// its own — which is the same blindness that let an else-branch credit the far
// end in four separate places.
func TestSignedAndAdHocHelpersAreDistinguished(t *testing.T) {
	adhoc := signAs(t, "--force", "--sign", "-", "--identifier", "io.dumpr.test")
	gotAdhoc := helperDegraded(adhoc)
	if gotAdhoc == "" {
		t.Error("an ad-hoc signed helper reported no degradation; its grant will not " +
			"survive installation and nothing would say so")
	}
	if strings.Contains(gotAdhoc, "could not determine") {
		t.Errorf("ad-hoc helper reported as undetermined rather than ad-hoc: %q", gotAdhoc)
	}

	id := signingIdentity(t)
	if id == "" {
		t.Skip("no codesigning identity on this machine — the pair cannot be formed, " +
			"and the ad-hoc half alone does not prove the function can say yes")
	}
	signed := signAs(t, "--force", "--sign", id, "--identifier", "io.dumpr.test")
	gotSigned := helperDegraded(signed)
	if gotSigned != "" {
		t.Errorf("a properly signed helper reported degraded %q, want empty", gotSigned)
	}
	if gotSigned == gotAdhoc {
		t.Fatal("both signing states produced the same answer — this check is blind")
	}
}

// Unknown and undegraded are different answers.
func TestAnUncheckableHelperIsNotReportedAsFine(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does-not-exist")
	if got := helperDegraded(missing); got == "" {
		t.Error("a helper that could not be checked reported as fine; " +
			"defaulting unknown to undegraded is how a check goes quiet")
	}
	if got := helperDegraded(""); got == "" {
		t.Error("an empty path reported as fine")
	}
}
