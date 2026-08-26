package transcribe

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// BackendLocalWhisper runs OpenAI's whisper locally. Audio never leaves the
// machine.
const BackendLocalWhisper = "local-whisper"

type localWhisper struct {
	binary   string
	model    string
	language string
	device   string
	log      func(string, ...any)
}

func newLocalWhisper(opt Options) (Transcriber, error) {
	binary := os.Getenv("MINUTES_WHISPER")
	if binary == "" {
		found, err := exec.LookPath("whisper")
		if err != nil {
			// Try the usual pip --user location, which is not always on PATH
			// for a process that did not come from a login shell — and the
			// supervisor never does.
			home, _ := os.UserHomeDir()
			candidate := filepath.Join(home, ".local", "bin", "whisper")
			if _, statErr := os.Stat(candidate); statErr != nil {
				return nil, fmt.Errorf("whisper is not installed or not on PATH: %w", err)
			}
			found = candidate
		}
		binary = found
	}
	w := &localWhisper{
		binary:   binary,
		model:    opt.Model,
		language: opt.Language,
		device:   opt.Device,
		log:      opt.Log,
	}
	if w.model == "" {
		w.model = "small"
	}
	if w.device == "" {
		w.device = "cuda"
	}
	return w, nil
}

func (w *localWhisper) Name() string { return BackendLocalWhisper + ":" + w.model }

func (w *localWhisper) SendsAudioOffMachine() bool { return false }

func (w *localWhisper) Transcribe(ctx context.Context, paths []string) ([][]Utterance, error) {
	if len(paths) == 0 {
		return nil, nil
	}
	out, err := os.MkdirTemp("", "minutes-whisper-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(out)

	args := append([]string{}, paths...)
	args = append(args,
		"--model", w.model,
		"--device", w.device,
		"--output_format", "json",
		"--output_dir", out,
		// Each segment is transcribed independently, so carrying context across
		// them is not just useless — it is how whisper gets into a loop and
		// repeats a phrase for a minute.
		"--condition_on_previous_text", "False",
		"--verbose", "False",
	)
	if w.language != "" {
		args = append(args, "--language", w.language)
	}

	w.log("transcribing %d file(s) with %s on %s (audio stays on this machine)",
		len(paths), w.model, w.device)

	// Whisper says nothing until it is finished, and a long meeting takes a
	// long time. Counting the transcripts as they land is the only progress
	// signal available, and an hour of silence is indistinguishable from a hang.
	stopProgress := make(chan struct{})
	go func() {
		tick := time.NewTicker(30 * time.Second)
		defer tick.Stop()
		for {
			select {
			case <-stopProgress:
				return
			case <-tick.C:
				done, _ := filepath.Glob(filepath.Join(out, "*.json"))
				if len(done) > 0 && len(done) < len(paths) {
					w.log("  %d of %d segments", len(done), len(paths))
				}
			}
		}
	}()
	defer close(stopProgress)

	cmd := exec.CommandContext(ctx, w.binary, args...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if strings.Contains(msg, "CUDA") || strings.Contains(msg, "cuda") {
			return nil, fmt.Errorf("whisper failed on device %q: %s\n"+
				"set transcription.device to \"cpu\" in the config if this machine has no usable GPU", w.device, msg)
		}
		return nil, fmt.Errorf("whisper failed: %w: %s", err, msg)
	}

	results := make([][]Utterance, len(paths))
	for i, p := range paths {
		base := strings.TrimSuffix(filepath.Base(p), filepath.Ext(p))
		body, err := os.ReadFile(filepath.Join(out, base+".json"))
		if err != nil {
			return nil, fmt.Errorf("whisper produced no transcript for %s: %w", base, err)
		}
		var parsed struct {
			Segments []struct {
				Start        float64 `json:"start"`
				End          float64 `json:"end"`
				Text         string  `json:"text"`
				NoSpeechProb float64 `json:"no_speech_prob"`
				AvgLogProb   float64 `json:"avg_logprob"`
			} `json:"segments"`
		}
		if err := json.Unmarshal(body, &parsed); err != nil {
			return nil, fmt.Errorf("reading transcript for %s: %w", base, err)
		}
		for _, s := range parsed.Segments {
			text := strings.TrimSpace(s.Text)
			if text == "" {
				continue
			}
			results[i] = append(results[i], Utterance{
				Start: s.Start, End: s.End, Text: text,
				NoSpeechProb: s.NoSpeechProb, AvgLogProb: s.AvgLogProb,
			})
		}
	}
	return results, nil
}
