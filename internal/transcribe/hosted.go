package transcribe

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// BackendOpenAI posts audio to an OpenAI-compatible transcription endpoint.
// Groq and others speak the same shape.
//
// Choosing it is how meeting audio leaves this machine. Nothing selects it
// implicitly.
const BackendOpenAI = "openai"

type hosted struct {
	baseURL string
	apiKey  string
	model   string
	lang    string
	client  *http.Client
	log     func(string, ...any)
}

func newHosted(opt Options) (Transcriber, error) {
	if opt.APIKey == "" {
		return nil, fmt.Errorf("the %q backend needs an API key: set transcription.apiKeyEnv in the config "+
			"to the name of an environment variable holding it", BackendOpenAI)
	}
	h := &hosted{
		baseURL: strings.TrimSuffix(opt.BaseURL, "/"),
		apiKey:  opt.APIKey,
		model:   opt.Model,
		lang:    opt.Language,
		log:     opt.Log,
		// A meeting segment is five minutes of audio; the upload alone can take
		// a while on a domestic connection.
		client: &http.Client{Timeout: 10 * time.Minute},
	}
	if h.baseURL == "" {
		h.baseURL = "https://api.openai.com/v1"
	}
	if h.model == "" {
		h.model = "whisper-1"
	}
	return h, nil
}

func (h *hosted) Name() string { return BackendOpenAI + ":" + h.model }

func (h *hosted) SendsAudioOffMachine() bool { return true }

func (h *hosted) Transcribe(ctx context.Context, paths []string) ([][]Utterance, error) {
	results := make([][]Utterance, len(paths))
	for i, p := range paths {
		h.log("uploading %s to %s", filepath.Base(p), h.baseURL)
		u, err := h.one(ctx, p)
		if err != nil {
			return nil, err
		}
		results[i] = u
	}
	return results, nil
}

func (h *hosted) one(ctx context.Context, path string) ([]Utterance, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	part, err := mw.CreateFormFile("file", filepath.Base(path))
	if err != nil {
		return nil, err
	}
	if _, err := io.Copy(part, f); err != nil {
		return nil, err
	}
	mw.WriteField("model", h.model)
	mw.WriteField("response_format", "verbose_json")
	if h.lang != "" {
		mw.WriteField("language", h.lang)
	}
	if err := mw.Close(); err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.baseURL+"/audio/transcriptions", &body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+h.apiKey)
	req.Header.Set("Content-Type", mw.FormDataContentType())

	resp, err := h.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	payload, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s returned %s: %s", h.baseURL, resp.Status, strings.TrimSpace(string(payload)))
	}

	var parsed struct {
		Segments []struct {
			Start        float64 `json:"start"`
			End          float64 `json:"end"`
			Text         string  `json:"text"`
			NoSpeechProb float64 `json:"no_speech_prob"`
			AvgLogProb   float64 `json:"avg_logprob"`
		} `json:"segments"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(payload, &parsed); err != nil {
		return nil, fmt.Errorf("reading response: %w", err)
	}

	var out []Utterance
	for _, s := range parsed.Segments {
		if text := strings.TrimSpace(s.Text); text != "" {
			out = append(out, Utterance{
				Start: s.Start, End: s.End, Text: text,
				NoSpeechProb: s.NoSpeechProb, AvgLogProb: s.AvgLogProb,
			})
		}
	}
	// Some compatible endpoints return only the whole text. Better one
	// untimed utterance than a silently empty transcript.
	if len(out) == 0 && strings.TrimSpace(parsed.Text) != "" {
		out = append(out, Utterance{Text: strings.TrimSpace(parsed.Text)})
	}
	return out, nil
}
