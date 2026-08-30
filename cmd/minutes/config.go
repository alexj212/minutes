package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/alexj212/minutes/internal/config"
	"github.com/alexj212/minutes/internal/transcribe"
)

// Editing the config from the command line.
//
// The file was always hand-written JSON, which meant a typo in a key was
// silently ignored — the value sat in the file looking set, and the default ran
// instead. That is the worst shape a settings file has: it disagrees with what
// somebody believes they configured, and nothing says so.
//
// So keys are an explicit table rather than reflection over the struct. An
// unknown key is refused and near-misses are suggested, values are validated
// before anything is written, and `minutes config` prints what is actually in
// effect rather than what the file happens to contain.

// setting is one editable key.
type setting struct {
	// get reads the effective value for display.
	get func(*config.Config) string
	// set validates and applies. It returns a message when the change deserves
	// one beyond "ok".
	set  func(*config.Config, string) (string, error)
	help string
}

func boolSetting(get func(*config.Config) *bool, help string) setting {
	return setting{
		get: func(c *config.Config) string { return strconv.FormatBool(*get(c)) },
		set: func(c *config.Config, v string) (string, error) {
			b, err := strconv.ParseBool(v)
			if err != nil {
				return "", fmt.Errorf("want true or false, got %q", v)
			}
			*get(c) = b
			return "", nil
		},
		help: help,
	}
}

func intSetting(get func(*config.Config) *int, help string) setting {
	return setting{
		get: func(c *config.Config) string { return strconv.Itoa(*get(c)) },
		set: func(c *config.Config, v string) (string, error) {
			n, err := strconv.Atoi(v)
			if err != nil || n < 0 {
				return "", fmt.Errorf("want a whole number of zero or more, got %q", v)
			}
			*get(c) = n
			return "", nil
		},
		help: help,
	}
}

func stringSetting(get func(*config.Config) *string, help string) setting {
	return setting{
		get:  func(c *config.Config) string { return *get(c) },
		set:  func(c *config.Config, v string) (string, error) { *get(c) = v; return "", nil },
		help: help,
	}
}

func settings() map[string]setting {
	return map[string]setting{
		"transcription.backend": {
			get: func(c *config.Config) string { return c.Transcription.Backend },
			set: func(c *config.Config, v string) (string, error) {
				switch v {
				case transcribe.BackendLocalWhisper:
					c.Transcription.Backend = v
					return "audio stays on this machine", nil
				case transcribe.BackendOpenAI:
					c.Transcription.Backend = v
					// Stated at the moment it is chosen, because this is the
					// setting that changes who can hear the meeting. It is a
					// legitimate choice and it is not the default; what it must
					// not be is a thing that happened quietly.
					return "⚠ meeting audio will now be UPLOADED to a third party for " +
						"transcription. Every recording made from now on leaves this machine.", nil
				}
				return "", fmt.Errorf("want %q or %q, got %q",
					transcribe.BackendLocalWhisper, transcribe.BackendOpenAI, v)
			},
			help: "local-whisper (default, stays here) or openai (uploads the audio)",
		},
		"transcription.model":     stringSetting(func(c *config.Config) *string { return &c.Transcription.Model }, "whisper size locally, or an API model name"),
		"transcription.language":  stringSetting(func(c *config.Config) *string { return &c.Transcription.Language }, "language hint, e.g. en"),
		"transcription.device":    stringSetting(func(c *config.Config) *string { return &c.Transcription.Device }, "where local inference runs: cuda, cpu, mps"),
		"transcription.baseUrl":   stringSetting(func(c *config.Config) *string { return &c.Transcription.BaseURL }, "OpenAI-compatible endpoint for a hosted backend"),
		"transcription.apiKeyEnv": stringSetting(func(c *config.Config) *string { return &c.Transcription.APIKeyEnv }, "env var holding the API key; the key is never stored here"),
		"transcription.afterStop": boolSetting(func(c *config.Config) *bool { return &c.Transcription.AfterStop }, "transcribe automatically when a recording stops"),

		"delivery.to":          stringSetting(func(c *config.Config) *string { return &c.Delivery.To }, "default destination for notes"),
		"delivery.coreSession": stringSetting(func(c *config.Config) *string { return &c.Delivery.CoreSession }, "the only destination that may receive a meeting automatically"),
		"delivery.auto":        boolSetting(func(c *config.Config) *bool { return &c.Delivery.Auto }, "deliver to the core session once a transcript exists"),

		"retention.keepDays":        intSetting(func(c *config.Config) *int { return &c.Retention.KeepDays }, "remove recordings older than this; 0 means no age limit"),
		"retention.keepCount":       intSetting(func(c *config.Config) *int { return &c.Retention.KeepCount }, "keep only the newest N; 0 means no count limit"),
		"retention.keepUndelivered": boolSetting(func(c *config.Config) *bool { return &c.Retention.KeepUndelivered }, "protect recordings whose notes never went anywhere"),
	}
}

// nearest suggests keys close to what was typed, so a typo is a correction
// rather than a list of everything.
func nearest(key string, keys []string) []string {
	var out []string
	lower := strings.ToLower(key)
	for _, k := range keys {
		if strings.Contains(strings.ToLower(k), lower) || strings.Contains(lower, strings.ToLower(k)) {
			out = append(out, k)
		}
	}
	if len(out) == 0 {
		// Fall back to the same section, which is the common typo.
		if i := strings.IndexByte(key, '.'); i > 0 {
			for _, k := range keys {
				if strings.HasPrefix(k, key[:i+1]) {
					out = append(out, k)
				}
			}
		}
	}
	sort.Strings(out)
	return out
}

func cmdConfig(args []string) int {
	all := settings()
	keys := make([]string, 0, len(all))
	for k := range all {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	if len(args) > 0 && args[0] == "set" {
		return configSet(args[1:], all, keys)
	}
	if len(args) > 0 && args[0] != "--json" {
		fmt.Fprintf(os.Stderr, "usage: minutes config [--json] | minutes config set KEY VALUE\n")
		return 2
	}

	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		return 1
	}
	if len(args) == 1 && args[0] == "--json" {
		b, _ := json.MarshalIndent(cfg, "", "  ")
		fmt.Println(string(b))
		return 0
	}

	path := config.Path()
	_, statErr := os.Stat(path)
	fmt.Printf("%s\n", path)
	if os.IsNotExist(statErr) {
		// Said plainly. "These are the defaults" and "this is what you wrote"
		// look identical in a listing, and only one of them survives an upgrade
		// that changes a default.
		fmt.Printf("  (no file — every value below is a default)\n")
	}
	fmt.Println()
	section := ""
	for _, k := range keys {
		if s := k[:strings.IndexByte(k, '.')]; s != section {
			section = s
			fmt.Printf("  %s\n", section)
		}
		v := all[k].get(cfg)
		if v == "" {
			v = "(unset)"
		}
		fmt.Printf("    %-26s %s\n", k[strings.IndexByte(k, '.')+1:], v)
	}
	fmt.Printf("\n  minutes config set KEY VALUE   (minutes config set --help for what each means)\n")
	return 0
}

func configSet(args []string, all map[string]setting, keys []string) int {
	if len(args) == 1 && (args[0] == "--help" || args[0] == "-h") {
		fmt.Println("keys:")
		for _, k := range keys {
			fmt.Printf("  %-30s %s\n", k, all[k].help)
		}
		return 0
	}
	if len(args) != 2 {
		fmt.Fprintf(os.Stderr, "usage: minutes config set KEY VALUE\n")
		return 2
	}
	key, value := args[0], args[1]
	s, ok := all[key]
	if !ok {
		fmt.Fprintf(os.Stderr, "no such setting %q\n", key)
		if near := nearest(key, keys); len(near) > 0 {
			fmt.Fprintf(os.Stderr, "did you mean:\n")
			for _, k := range near {
				fmt.Fprintf(os.Stderr, "  %s\n", k)
			}
		} else {
			fmt.Fprintf(os.Stderr, "minutes config set --help lists them\n")
		}
		return 2
	}

	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		return 1
	}
	before := s.get(cfg)
	note, err := s.set(cfg, value)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", key, err)
		return 2
	}

	path := config.Path()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "creating %s: %v\n", filepath.Dir(path), err)
		return 1
	}
	body, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		return 1
	}
	// 0600: delivery destinations and an endpoint URL are nobody else's
	// business on a shared machine, and the file is in the operator's home.
	if err := os.WriteFile(path, append(body, '\n'), 0o600); err != nil {
		fmt.Fprintf(os.Stderr, "writing %s: %v\n", path, err)
		return 1
	}

	after := s.get(cfg)
	if before == after {
		fmt.Printf("%s is already %s\n", key, after)
	} else {
		fmt.Printf("%s: %s → %s\n", key, orUnset(before), orUnset(after))
	}
	if note != "" {
		fmt.Printf("  %s\n", note)
	}
	return 0
}

func orUnset(s string) string {
	if s == "" {
		return "(unset)"
	}
	return s
}
