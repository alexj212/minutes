// Package manifest is the record of what a recording actually produced.
//
// Audio lives on disk as ordinary WAV files and the metadata lives here, in a
// sidecar JSON beside them. Not in a database: a database gets copied by every
// backup, and an hour of stereo audio is hundreds of megabytes that would then
// be copied nightly, forever. This follows shabadoo's hub/release.go, for the
// same reason.
//
// The manifest always describes what is on disk, including the segment
// currently being written. A manifest that only listed finished segments would
// make a crashed recording look emptier than it is, and the files left over
// would look like litter rather than data.
package manifest

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Name is the manifest's filename inside a recording directory.
const Name = "manifest.json"

// Version is the manifest schema version. It exists so a later phase can read
// a recording made by this one.
const Version = 1

// State is where a recording is in its life.
type State string

const (
	// StateRecording means a supervisor is running and writing to this
	// directory. It is also the honest answer to "is this machine recording
	// right now".
	StateRecording State = "recording"
	// StateTranscribing means capture has finished and the supervisor is still
	// working. The audio is complete and safe at this point; only the
	// transcript is outstanding.
	StateTranscribing State = "transcribing"
	StateStopped      State = "stopped"
	// StateFailed means the recording ended for a reason other than being
	// asked to. Whatever segments completed are still listed and still good.
	StateFailed State = "failed"
)

// Segment is one chunk of one track.
type Segment struct {
	Index int    `json:"index"`
	File  string `json:"file"`
	// StartSeconds is this segment's offset from the recording epoch. Segment
	// k of every track covers the same wall-clock window, which is what lets a
	// later phase merge two transcripts without re-aligning them.
	StartSeconds    float64 `json:"startSeconds"`
	DurationSeconds float64 `json:"durationSeconds"`
	Frames          uint64  `json:"frames"`
	// PaddedFrames is how much of this segment is gap-fill rather than captured
	// audio. On the system track it is the measure of how long nothing was
	// playing.
	PaddedFrames uint64  `json:"paddedFrames"`
	PeakDBFS     float64 `json:"peakDBFS"`
	Packets      int     `json:"packets"`
	// Complete is false while a segment is still being written. A false here
	// with no supervisor running means the recording was interrupted, and the
	// file is still playable up to the last header flush.
	Complete bool   `json:"complete"`
	Size     int64  `json:"size,omitempty"`
	SHA256   string `json:"sha256,omitempty"`
}

// Track is one side of the meeting.
type Track struct {
	// Name is "mic" or "system". They are never mixed.
	Name       string    `json:"name"`
	Device     string    `json:"device"`
	SampleRate int       `json:"sampleRate"`
	Channels   int       `json:"channels"`
	Segments   []Segment `json:"segments"`
	// Reanchors counts how often this track's device clock disagreed with the
	// wall clock by more than jitter. Non-zero means the timeline was rebuilt
	// mid-recording, which a phase merging transcripts should know.
	//
	// Always written, never omitted. On a diagnostic field, absent and zero
	// mean different things — "measured, and it never happened" against "not
	// measured, or no longer reported" — and a reader cannot tell them apart
	// from a field that vanishes when it is zero.
	Reanchors int `json:"reanchors"`
}

// Duration is how far this track's segments reach.
//
// Falls back to the frame count when a segment carries no duration of its own.
// Returning a length shorter than the audio actually present is worse than
// returning nothing: anything that clamps to this would silently truncate.
func (t Track) Duration() float64 {
	var end float64
	for _, s := range t.Segments {
		length := s.DurationSeconds
		if length == 0 && s.Frames > 0 && t.SampleRate > 0 {
			length = float64(s.Frames) / float64(t.SampleRate)
		}
		if e := s.StartSeconds + length; e > end {
			end = e
		}
	}
	return end
}

// PeakDBFS is the loudest point across the track's segments.
func (t Track) PeakDBFS() float64 {
	peak := -999.0
	for _, s := range t.Segments {
		if s.PeakDBFS > peak {
			peak = s.PeakDBFS
		}
	}
	return peak
}

// Started reports whether any audio has been written to this track yet. A
// recording still in its first moments has segments but no samples, and
// judging it silent then would be a false alarm rather than a finding.
func (t Track) Started() bool {
	for _, s := range t.Segments {
		if s.Frames > 0 {
			return true
		}
	}
	return false
}

// speechFloorDBFS is the peak below which a track holds no speech, whatever
// else it holds.
//
// Speech peaks between about -6 and -20 dBFS. A microphone that was muted, at
// the wrong gain, or simply not the device in use sits far below that: measured
// on a real recording, -55.7 dBFS, which is not silence by any digital
// definition and is still nothing anybody said.
//
// The gap matters because a track above the digital-silence floor gets
// transcribed, and a speech model given inaudible audio invents.
const speechFloorDBFS = -40

// CarriesSpeech reports whether anything on this track is loud enough to be
// somebody talking.
func (t Track) CarriesSpeech() bool { return t.PeakDBFS() > speechFloorDBFS }

// Silent reports a track that carries no signal. This is the failure the whole
// design exists to catch, so it is asked of every recording rather than left
// for somebody to notice.
func (t Track) Silent() bool { return t.PeakDBFS() <= -90 }

// Manifest is one recording.
type Manifest struct {
	Version int    `json:"version"`
	ID      string `json:"id"`
	Name    string `json:"name,omitempty"`
	State   State  `json:"state"`

	StartedAt time.Time  `json:"startedAt"`
	StoppedAt *time.Time `json:"stoppedAt,omitempty"`

	Host     string `json:"host"`
	Platform string `json:"platform"`

	// SegmentSeconds is the chunk length. Chunking bounds what a crash costs.
	SegmentSeconds float64 `json:"segmentSeconds"`
	// EpochQPC100ns is the instant every track calls zero, taken from the
	// clock both capture streams are stamped with.
	EpochQPC100ns uint64 `json:"epochQPC100ns"`

	Tracks []Track `json:"tracks"`

	// Recorded is always true and is written so the fact survives into whatever
	// reads this later. Recording is a trust matter and in some places a legal
	// one, so a summary built from this manifest can say the meeting was
	// recorded without having to infer it.
	Recorded bool `json:"recorded"`

	// Transcript records that this recording has been transcribed, and how.
	Transcript *TranscriptRecord `json:"transcript,omitempty"`

	// App names the process captured instead of the whole machine, if one was
	// targeted. Empty means system-wide, which takes everything that played.
	App string `json:"app,omitempty"`

	// IntendedFor is where this meeting's notes are meant to go, named when the
	// recording started. Captured then because that is when somebody knows what
	// the meeting is about; two hours later the context has drained away.
	IntendedFor string `json:"intendedFor,omitempty"`

	// Delivery records that the notes were handed to a session. Kept so that
	// "did I send this one?" is answerable from the recording rather than from
	// memory.
	Delivery *DeliveryRecord `json:"delivery,omitempty"`

	// Error explains a failed state.
	Error string `json:"error,omitempty"`

	mu  sync.Mutex
	dir string
}

// New creates a manifest for a recording directory.
func New(dir, id, name string, segmentSeconds float64) *Manifest {
	host, _ := os.Hostname()
	return &Manifest{
		Version:        Version,
		ID:             id,
		Name:           name,
		State:          StateRecording,
		StartedAt:      time.Now(),
		Host:           host,
		Platform:       "wsl/windows",
		SegmentSeconds: segmentSeconds,
		Recorded:       true,
		dir:            dir,
	}
}

// Dir is the directory this manifest describes.
func (m *Manifest) Dir() string { return m.dir }

// Load reads the manifest in dir.
func Load(dir string) (*Manifest, error) {
	body, err := os.ReadFile(filepath.Join(dir, Name))
	if err != nil {
		return nil, err
	}
	var m Manifest
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, fmt.Errorf("manifest in %s is unreadable: %w", dir, err)
	}
	m.dir = dir
	return &m, nil
}

// Save writes the manifest atomically.
//
// Written to a temporary file and renamed, so a crash during the write leaves
// the previous manifest intact rather than a half-written one. The manifest is
// the only thing that says what the WAV files beside it are.
func (m *Manifest) Save() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.saveLocked()
}

func (m *Manifest) saveLocked() error {
	body, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	body = append(body, '\n')
	tmp := filepath.Join(m.dir, ".manifest.tmp")
	if err := os.WriteFile(tmp, body, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(m.dir, Name))
}

// track returns the entry for name, creating it if absent. Caller holds mu.
func (m *Manifest) trackLocked(name string) *Track {
	for i := range m.Tracks {
		if m.Tracks[i].Name == name {
			return &m.Tracks[i]
		}
	}
	m.Tracks = append(m.Tracks, Track{Name: name})
	return &m.Tracks[len(m.Tracks)-1]
}

// SetTrack records a track's format, and saves.
func (m *Manifest) SetTrack(name, device string, sampleRate, channels int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	t := m.trackLocked(name)
	t.Device = device
	t.SampleRate = sampleRate
	t.Channels = channels
	return m.saveLocked()
}

// PutSegment inserts or replaces a segment, and saves.
//
// Called once when a segment opens, so an interrupted recording still lists the
// file it was writing, and once when it closes with the final sizes.
func (m *Manifest) PutSegment(track string, seg Segment) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	t := m.trackLocked(track)
	for i := range t.Segments {
		if t.Segments[i].Index == seg.Index {
			t.Segments[i] = seg
			return m.saveLocked()
		}
	}
	t.Segments = append(t.Segments, seg)
	return m.saveLocked()
}

// TranscriptRecord is how a recording's transcript was produced.
//
// AudioLeftMachine is the part worth keeping: whether a given meeting was sent
// to a third party is a question somebody may have to answer later, possibly to
// somebody else, and memory is not an acceptable source for it.
type TranscriptRecord struct {
	Backend          string    `json:"backend"`
	AudioLeftMachine bool      `json:"audioLeftMachine"`
	CreatedAt        time.Time `json:"createdAt"`
	Lines            int       `json:"lines"`
	File             string    `json:"file"`
}

// DeliveryRecord is where a recording's notes were sent.
type DeliveryRecord struct {
	To string    `json:"to"`
	At time.Time `json:"at"`
	// Degraded means the agent was unreachable and the brief was written to
	// disk instead. It is not the same as delivered, and should not look it.
	Degraded bool `json:"degraded,omitempty"`
}

// SetState moves the recording to a new state, and saves.
func (m *Manifest) SetState(st State) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.State = st
	return m.saveLocked()
}

// SetIntendedFor records where the notes are meant to go, and saves.
func (m *Manifest) SetIntendedFor(to string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.IntendedFor = to
	return m.saveLocked()
}

// SetDelivery records where the notes went, and saves.
func (m *Manifest) SetDelivery(r DeliveryRecord) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Delivery = &r
	return m.saveLocked()
}

// SetTranscript records a completed transcription, and saves.
func (m *Manifest) SetTranscript(r TranscriptRecord) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Transcript = &r
	return m.saveLocked()
}

// SetReanchors records that a track's timeline was rebuilt, and saves.
func (m *Manifest) SetReanchors(track string, n int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.trackLocked(track).Reanchors = n
	return m.saveLocked()
}

// Finish marks the recording ended and saves. A non-nil err records why.
func (m *Manifest) Finish(err error) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()
	m.StoppedAt = &now
	if err != nil {
		m.State = StateFailed
		m.Error = err.Error()
	} else {
		m.State = StateStopped
	}
	return m.saveLocked()
}

// SetEpoch records the shared clock's zero, and saves.
func (m *Manifest) SetEpoch(qpc100ns uint64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.EpochQPC100ns = qpc100ns
	return m.saveLocked()
}

// Duration is the length of the recording, taken from the longest track.
func (m *Manifest) Duration() float64 {
	var d float64
	for _, t := range m.Tracks {
		if td := t.Duration(); td > d {
			d = td
		}
	}
	return d
}

// HashFile returns the SHA-256 and size of a file.
func HashFile(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}
