package main

import (
	"bytes"
	"os"
	"regexp"
	"strings"
	"testing"
)

// skills/minutes/SKILL.md is what another Claude session reads before driving
// this tool, and a stale one is worse than none: it teaches a command that no
// longer exists, or hides one that does. So it is pinned rather than reviewed.
//
// The command list is read out of main()'s own dispatch rather than written
// here, for the reason the rest of this project keeps rediscovering — a
// hardcoded copy agrees with whatever its author last assumed, which is
// precisely how the drift starts.
const skillPath = "../../skills/minutes/SKILL.md"

// supervise is the detached supervisor re-executing itself. It is not a command
// anybody types, and a skill that advertised it would invite a session to start
// one by hand.
var notForHumans = map[string]bool{"supervise": true}

func dispatchedCommands(t *testing.T) []string {
	t.Helper()
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("reading main.go: %v", err)
	}
	body := string(src)
	start := strings.Index(body, "switch os.Args[1] {")
	if start < 0 {
		t.Fatal("main.go no longer has the subcommand switch this test reads")
	}
	end := strings.Index(body[start:], "\n\tdefault:")
	if end < 0 {
		t.Fatal("subcommand switch has no default arm; cannot tell where it ends")
	}

	var cmds []string
	// A case can carry several names — `case "version", "--version", "-v":` —
	// so match the whole clause and pull every quoted word out of it. The
	// narrower pattern silently saw no command at all on such a line, which is
	// how `version` came to be documented and reported as non-existent.
	re := regexp.MustCompile(`case ((?:"[a-z-]+",?\s*)+):`)
	word := regexp.MustCompile(`"([a-z-]+)"`)
	for _, clause := range re.FindAllStringSubmatch(body[start:start+end], -1) {
		for _, m := range word.FindAllStringSubmatch(clause[1], -1) {
			name := m[1]
			// A case may also carry flag spellings of the same thing —
			// "--version", "-v". Those are aliases, not commands, and a skill
			// documenting the command has documented them.
			if strings.HasPrefix(name, "-") {
				continue
			}
			if name == "help" || notForHumans[name] {
				continue
			}
			cmds = append(cmds, name)
		}
	}
	if len(cmds) < 5 {
		t.Fatalf("only found %d commands in the dispatch; the parse is wrong, not the skill", len(cmds))
	}
	return cmds
}

func TestSkillDocumentsEveryCommand(t *testing.T) {
	skill, err := os.ReadFile(skillPath)
	if err != nil {
		t.Fatalf("reading %s: %v", skillPath, err)
	}
	text := string(skill)

	for _, cmd := range dispatchedCommands(t) {
		if !strings.Contains(text, "minutes "+cmd) {
			t.Errorf("`minutes %s` is dispatched but never mentioned in %s — "+
				"document it, or add it to notForHumans if it is not for people",
				cmd, skillPath)
		}
	}
}

// The other direction: a skill naming a command that was removed sends a
// session to run something that exits 2.
func TestSkillNamesNoCommandThatIsGone(t *testing.T) {
	skill, err := os.ReadFile(skillPath)
	if err != nil {
		t.Fatalf("reading %s: %v", skillPath, err)
	}

	known := map[string]bool{}
	for _, c := range dispatchedCommands(t) {
		known[c] = true
	}
	for c := range notForHumans {
		known[c] = true
	}

	re := regexp.MustCompile("`?minutes ([a-z-]+)")
	for _, m := range re.FindAllStringSubmatch(string(skill), -1) {
		name := m[1]
		// Prose says "minutes list totals the directory"; only treat a word as
		// a command claim if it is one this program would recognise, or is not
		// an English word we would expect after the binary name.
		if known[name] {
			continue
		}
		switch name {
		case "and", "is", "are", "recordings", "binary", "capture", "does", "of", "on", "to", "with":
			continue
		}
		t.Errorf("%s names `minutes %s`, which is not a command this binary dispatches",
			skillPath, name)
	}
}

// The description is the routing card: it is all another session sees when
// deciding whether this skill applies. An empty or missing one means the skill
// is installed and unreachable.
func TestSkillHasFrontmatter(t *testing.T) {
	skill, err := os.ReadFile(skillPath)
	if err != nil {
		t.Fatalf("reading %s: %v", skillPath, err)
	}
	text := string(skill)
	if !strings.HasPrefix(text, "---\n") {
		t.Fatal("SKILL.md must open with YAML frontmatter")
	}
	head, _, ok := strings.Cut(strings.TrimPrefix(text, "---\n"), "\n---\n")
	if !ok {
		t.Fatal("SKILL.md frontmatter is not closed")
	}
	for _, field := range []string{"name: minutes", "description: "} {
		if !strings.Contains(head, field) {
			t.Errorf("frontmatter is missing %q", field)
		}
	}
	for _, line := range strings.Split(head, "\n") {
		if desc, ok := strings.CutPrefix(line, "description: "); ok && len(desc) < 120 {
			t.Errorf("description is %d chars; too short to route on", len(desc))
		}
	}
}

// The skill must cover every flag the CLI advertises in its own usage text.
//
// Commands were already pinned; flags were not, so a new one could reach users
// while the skill went on describing the tool without it. `usage()` is the
// right boundary rather than every flag in the file: it is the curated surface,
// and it deliberately omits the internal ones — --dir, --helper, --app-pid —
// that only the supervisor passes to itself.
func TestSkillDocumentsEveryAdvertisedFlag(t *testing.T) {
	source, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	skill, err := os.ReadFile(skillPath)
	if err != nil {
		t.Fatal(err)
	}

	start := bytes.Index(source, []byte("func usage()"))
	if start < 0 {
		t.Fatal("usage() not found; this test reads the CLI's own advertised surface")
	}
	end := bytes.Index(source[start:], []byte("\n}\n"))
	if end < 0 {
		t.Fatal("could not find the end of usage()")
	}
	usage := string(source[start : start+end])

	advertised := map[string]bool{}
	for _, m := range regexp.MustCompile(`\[--([a-z-]+)`).FindAllStringSubmatch(usage, -1) {
		advertised[m[1]] = true
	}
	if len(advertised) < 5 {
		t.Fatalf("only found %d advertised flags; the pattern is probably wrong", len(advertised))
	}

	for flag := range advertised {
		if !bytes.Contains(skill, []byte("--"+flag)) {
			t.Errorf("usage() advertises --%s but %s never mentions it: a session driving "+
				"this tool would not know it exists", flag, skillPath)
		}
	}
}

// The skill's platform table must not contradict what is actually built.
//
// R5 landed and the platform table was updated; a line in the "do not use for"
// list saying macOS was not built was not. That list is what a session reads to
// decide whether to reach for the tool at all, so it is the most expensive place
// to be wrong — and because the payload had vendored the pre-R5 copy, the stale
// half was what every node carried.
//
// Caught by shabadoo-wsl reading the file, not by any test here. The pin is
// cheap: a helper build script existing means that platform is supported.
func TestSkillDoesNotDenyAPlatformThatIsBuilt(t *testing.T) {
	skill, err := os.ReadFile(skillPath)
	if err != nil {
		t.Fatal(err)
	}
	lower := strings.ToLower(string(skill))

	for _, p := range []struct {
		build  string
		names  []string
		denied []string
	}{
		{
			build:  "../../native/darwin/build.sh",
			names:  []string{"macos", "mac"},
			denied: []string{"not built", "not started", "r5 is not", "refuses"},
		},
		{
			build:  "../../native/windows/build.bat",
			names:  []string{"windows"},
			denied: []string{"not built", "not started"},
		},
	} {
		if _, err := os.Stat(p.build); err != nil {
			continue // that platform genuinely has no helper
		}
		for _, line := range strings.Split(lower, "\n") {
			// Word-bounded: "mac" is a substring of "machine", and this file
			// says "on this machine" constantly. A substring match would fire
			// on unrelated lines and get switched off.
			mentionsPlatform := false
			for _, n := range p.names {
				if regexp.MustCompile(`\b` + n + `\b`).MatchString(line) {
					mentionsPlatform = true
					break
				}
			}
			if !mentionsPlatform {
				continue
			}
			for _, d := range p.denied {
				if strings.Contains(line, d) {
					t.Errorf("%s exists, so that platform is built, but the skill says:\n  %s\n"+
						"A session reads this to decide whether to reach for the tool at all.",
						p.build, strings.TrimSpace(line))
				}
			}
		}
	}
}
