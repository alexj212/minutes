package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/alexj/minutes/internal/transcribe"
)

func writeConfig(t *testing.T, body string) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MINUTES_CONFIG", p)
}

// Nothing uploads by accident: with no config at all, transcription is local.
func TestDefaultKeepsAudioOnThisMachine(t *testing.T) {
	t.Setenv("MINUTES_CONFIG", filepath.Join(t.TempDir(), "absent.json"))
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Transcription.Backend != transcribe.BackendLocalWhisper {
		t.Errorf("default backend is %q, want %q", cfg.Transcription.Backend, transcribe.BackendLocalWhisper)
	}
	if !cfg.Transcription.AfterStop {
		t.Error("afterStop defaults off; forgetting the second command is the common case")
	}
}

// A field left out of the file keeps its default, but one written as false must
// be honoured — otherwise turning the behaviour off would silently not work.
func TestAfterStopCanBeTurnedOff(t *testing.T) {
	writeConfig(t, `{"transcription":{"afterStop":false}}`)
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Transcription.AfterStop {
		t.Error("afterStop:false in the config was ignored")
	}
	if cfg.Transcription.Backend != transcribe.BackendLocalWhisper {
		t.Errorf("an unrelated field lost its default: %q", cfg.Transcription.Backend)
	}
}

func TestOmittedFieldsKeepDefaults(t *testing.T) {
	writeConfig(t, `{"transcription":{"model":"medium"}}`)
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Transcription.Model != "medium" {
		t.Errorf("model = %q, want medium", cfg.Transcription.Model)
	}
	if !cfg.Transcription.AfterStop {
		t.Error("afterStop lost its default when another field was set")
	}
}

// Ignoring a malformed config and running the default would differ from what
// somebody wrote in exactly the way that matters.
func TestMalformedConfigIsAnError(t *testing.T) {
	writeConfig(t, `{not json`)
	if _, err := Load(); err == nil {
		t.Error("a malformed config loaded without complaint")
	}
}

func TestAPIKeyComesFromTheEnvironmentNotTheFile(t *testing.T) {
	writeConfig(t, `{"transcription":{"backend":"openai","apiKeyEnv":"TEST_KEY_VAR"}}`)
	t.Setenv("TEST_KEY_VAR", "secret-value")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	opt := cfg.TranscribeOptions(nil)
	if opt.APIKey != "secret-value" {
		t.Errorf("APIKey = %q, want the environment variable's value", opt.APIKey)
	}
}
