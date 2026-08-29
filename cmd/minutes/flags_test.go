package main

import (
	"flag"
	"reflect"
	"testing"
)

// Go's flag package stops parsing at the first non-flag argument, so
// `minutes transcribe <id> --model small` would take "--model" and "small" as
// two more recording ids and quietly transcribe with the default model. Every
// command here is that shape: an optional id followed by flags.
func TestFlagsAfterPositionalsAreParsed(t *testing.T) {
	cases := []struct {
		name      string
		args      []string
		wantModel string
		wantForce bool
		wantIDs   []string
	}{
		{
			name:      "flags before the id, which always worked",
			args:      []string{"--model", "small", "--force", "rec-1"},
			wantModel: "small", wantForce: true, wantIDs: []string{"rec-1"},
		},
		{
			name:      "flags after the id, which silently did not",
			args:      []string{"rec-1", "--model", "small", "--force"},
			wantModel: "small", wantForce: true, wantIDs: []string{"rec-1"},
		},
		{
			name:      "interleaved",
			args:      []string{"--model", "small", "rec-1", "--force", "rec-2"},
			wantModel: "small", wantForce: true, wantIDs: []string{"rec-1", "rec-2"},
		},
		{
			name:      "no positionals",
			args:      []string{"--model", "medium"},
			wantModel: "medium", wantIDs: nil,
		},
		{
			name:    "no flags",
			args:    []string{"rec-1", "rec-2"},
			wantIDs: []string{"rec-1", "rec-2"},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fs := flag.NewFlagSet("test", flag.ContinueOnError)
			model := fs.String("model", "", "")
			force := fs.Bool("force", false, "")

			ids := parseFlags(fs, c.args)

			if *model != c.wantModel {
				t.Errorf("model = %q, want %q", *model, c.wantModel)
			}
			if *force != c.wantForce {
				t.Errorf("force = %v, want %v", *force, c.wantForce)
			}
			if !reflect.DeepEqual(ids, c.wantIDs) {
				t.Errorf("ids = %v, want %v", ids, c.wantIDs)
			}
		})
	}
}

func TestFirstReturnsEmptyForNoIDs(t *testing.T) {
	if got := first(nil); got != "" {
		t.Errorf("first(nil) = %q, want empty", got)
	}
	if got := first([]string{"a", "b"}); got != "a" {
		t.Errorf("first = %q, want \"a\"", got)
	}
}
