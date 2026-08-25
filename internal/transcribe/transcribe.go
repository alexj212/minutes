// Package transcribe turns segment audio into text.
//
// The backend is pluggable for one reason: some meetings must not leave the
// machine, and some are worth the latency of a hosted model. Which of those a
// given meeting is cannot be decided here.
//
// **Local is the default, and nothing uploads audio by accident.** Getting a
// recording off this machine requires naming a hosted backend in the config
// file. There is no fallback that reaches the network when the local path is
// unavailable, because the failure mode of such a fallback is that a
// confidential meeting is uploaded on the day the GPU driver breaks.
package transcribe

import (
	"context"
	"fmt"
)

// Utterance is one span of speech, in seconds relative to the start of the file
// it came from.
type Utterance struct {
	Start float64 `json:"start"`
	End   float64 `json:"end"`
	Text  string  `json:"text"`
}

// Transcriber turns audio files into utterances.
//
// It takes a batch rather than one file at a time because loading a speech
// model costs several seconds and a ninety-minute meeting is dozens of
// segments across two tracks. Paying that once instead of per file is the
// difference between a minute of work and half an hour of it.
type Transcriber interface {
	// Name identifies the backend in the manifest, so a recording says how its
	// transcript was made.
	Name() string
	// SendsAudioOffMachine reports whether using this backend uploads audio to
	// a third party. It is asked before every run and shown to the operator.
	SendsAudioOffMachine() bool
	// Transcribe returns one slice of utterances per input path, in the same
	// order. A file that yields no speech yields an empty slice, not an error.
	Transcribe(ctx context.Context, paths []string) ([][]Utterance, error)
}

// Options configures a backend.
type Options struct {
	Backend  string
	Model    string
	Language string
	Device   string
	// BaseURL and APIKey are only consulted by hosted backends.
	BaseURL string
	APIKey  string
	// Log receives progress. Transcription of a long meeting is slow enough
	// that silence looks like a hang.
	Log func(string, ...any)
}

// New builds the named backend.
//
// An unknown name is an error rather than a fall back to something that works,
// because the thing that would "work" is the one that uploads audio.
func New(opt Options) (Transcriber, error) {
	if opt.Log == nil {
		opt.Log = func(string, ...any) {}
	}
	switch opt.Backend {
	case "", BackendLocalWhisper:
		return newLocalWhisper(opt)
	case BackendOpenAI:
		return newHosted(opt)
	}
	return nil, fmt.Errorf("unknown transcription backend %q (known: %s, %s)",
		opt.Backend, BackendLocalWhisper, BackendOpenAI)
}
