package transcribe

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Whisper fetches a missing model with no output at all, so the first run with
// a new one looks exactly like a hang — and it happens after a meeting, while
// somebody waits for their notes.
//
// Asserted as a pair: a cached model and a missing one must produce different
// answers. A test that only knew the missing case would pass just as happily if
// the note fired on every run, which is a warning nobody reads.
func TestAMissingModelIsAnnouncedAndACachedOneIsNot(t *testing.T) {
	cache := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", cache)
	if err := os.MkdirAll(filepath.Join(cache, "whisper"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cache, "whisper", "small.pt"), []byte("model"), 0o644); err != nil {
		t.Fatal(err)
	}

	if note := modelDownloadNote("small"); note != "" {
		t.Errorf("a cached model was announced as a download: %q", note)
	}
	note := modelDownloadNote("large-v3")
	if note == "" {
		t.Fatal("a model that is not cached was not announced")
	}
	if !strings.Contains(note, "1.5 GB") {
		t.Errorf("note does not say how big it is: %q", note)
	}

	// An unrecognised name is not a reason to say nothing. Whatever it weighs,
	// it is not here and it is about to be fetched.
	if note := modelDownloadNote("some-future-model"); note == "" {
		t.Error("an unrecognised model that is not cached was not announced")
	}
}
