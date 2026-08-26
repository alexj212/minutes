package main

import (
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
	re := regexp.MustCompile(`case "([a-z-]+)":`)
	for _, m := range re.FindAllStringSubmatch(body[start:start+end], -1) {
		name := m[1]
		if name == "help" || notForHumans[name] {
			continue
		}
		cmds = append(cmds, name)
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
