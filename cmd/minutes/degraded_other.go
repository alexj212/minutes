//go:build !darwin

package main

// helperDegraded reports why a helper works here but will not work as well on
// the machine that installs it.
//
// Nothing degrades a Windows helper. It carries no signature anybody depends
// on, so a copy of it on another machine behaves exactly as it does here, and
// there is nothing to report.
//
// The darwin build has a real answer; see degraded_darwin.go. Deliberately not
// a single function with a runtime.GOOS switch: the check there has to run
// codesign against the binary, which does not exist to compile against on any
// other platform, and a stub that silently returns "fine" everywhere is how a
// check becomes a check that always passes.
func helperDegraded(path string) string { return "" }
