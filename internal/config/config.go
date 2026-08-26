// Package config holds the choices that must be made deliberately.
//
// There is exactly one of those so far, and it is which transcription backend
// runs. The default is local, and the file exists so that choosing otherwise —
// sending meeting audio to a third party — is something somebody wrote down
// rather than something that happened.
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/alexj/minutes/internal/deliver"
	"github.com/alexj/minutes/internal/session"
	"github.com/alexj/minutes/internal/transcribe"
)

// Transcription selects and configures a backend.
type Transcription struct {
	// Backend is "local-whisper" (the default) or "openai". Naming a hosted
	// backend is the act that lets audio leave this machine.
	Backend string `json:"backend,omitempty"`
	// Model is the backend's model name: a whisper size locally, an API model
	// name remotely.
	Model    string `json:"model,omitempty"`
	Language string `json:"language,omitempty"`
	// Device is where local inference runs: "cuda" or "cpu".
	Device string `json:"device,omitempty"`
	// BaseURL points a hosted backend somewhere OpenAI-compatible.
	BaseURL string `json:"baseUrl,omitempty"`
	// AfterStop transcribes automatically once a recording stops, in the
	// background. On by default: forgetting the second command is the common
	// case, and the supervisor is already detached so it costs nothing to wait
	// on.
	AfterStop bool `json:"afterStop"`
	// APIKeyEnv names the environment variable holding the key. The key itself
	// is deliberately not storable here: a config file gets copied, committed
	// and pasted into bug reports.
	APIKeyEnv string `json:"apiKeyEnv,omitempty"`
}

// Delivery decides where a meeting's notes go, and whether they go by
// themselves.
type Delivery struct {
	// To is the default destination when `--to` is not given. It defaults to
	// this node's own core session, which is the safe answer: delivering there
	// keeps the transcript on the machine that made it and puts a session with
	// a person behind it in the loop.
	To string `json:"to,omitempty"`
	// CoreSession names the destination that may receive a meeting
	// automatically. Anything else waits for somebody to ask, because sending
	// to another project is publishing rather than filing.
	CoreSession string `json:"coreSession,omitempty"`
	// Auto delivers to the core session once a transcript exists, provided the
	// transcript carries no stretches where the far end was silent.
	Auto bool `json:"auto"`
}

// Config is the whole of it.
type Config struct {
	Transcription Transcription     `json:"transcription"`
	Delivery      Delivery          `json:"delivery"`
	Retention     session.Retention `json:"retention"`
}

// Path is where the config lives.
func Path() string {
	if p := os.Getenv("MINUTES_CONFIG"); p != "" {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "minutes.json"
	}
	return filepath.Join(home, ".config", "minutes", "config.json")
}

// Default is what runs when nobody has said otherwise: everything stays here,
// and a meeting goes to this machine's own session.
func Default() *Config {
	core := deliver.CoreSession()
	return &Config{
		Transcription: Transcription{
			Backend:   transcribe.BackendLocalWhisper,
			Model:     "small",
			Language:  "en",
			Device:    "cuda",
			AfterStop: true,
		},
		Delivery: Delivery{
			To:          core,
			CoreSession: core,
			Auto:        true,
		},
		// Off. Deleting somebody's meetings without being asked is worse than
		// using their disk, so retention is opt-in — but when it is turned on,
		// an undelivered recording is protected unless that is turned off too.
		Retention: session.Retention{KeepUndelivered: true},
	}
}

// Load reads the config, falling back to defaults when there is none.
//
// A missing file is the ordinary case and not an error. A malformed one is an
// error: the alternative is silently ignoring what somebody wrote and running
// the default, and the default differs from what they asked for in exactly the
// way that matters.
func Load() (*Config, error) {
	body, err := os.ReadFile(Path())
	if err != nil {
		if os.IsNotExist(err) {
			return Default(), nil
		}
		return nil, err
	}
	cfg := Default()
	if err := json.Unmarshal(body, cfg); err != nil {
		return nil, fmt.Errorf("config at %s is unreadable: %w", Path(), err)
	}
	if cfg.Transcription.Backend == "" {
		cfg.Transcription.Backend = transcribe.BackendLocalWhisper
	}
	return cfg, nil
}

// TranscribeOptions turns the config into backend options, resolving the API
// key from the environment.
func (c *Config) TranscribeOptions(log func(string, ...any)) transcribe.Options {
	t := c.Transcription
	opt := transcribe.Options{
		Backend:  t.Backend,
		Model:    t.Model,
		Language: t.Language,
		Device:   t.Device,
		BaseURL:  t.BaseURL,
		Log:      log,
	}
	if t.APIKeyEnv != "" {
		opt.APIKey = os.Getenv(t.APIKeyEnv)
	}
	return opt
}
