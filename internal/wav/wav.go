// Package wav writes 16-bit PCM WAV files, positioning every packet by
// timestamp rather than by appending.
//
// Positioning matters because a WASAPI loopback stream delivers nothing at all
// while the render endpoint is idle. If a meeting is quiet for thirty seconds,
// the system track receives no packets for thirty seconds — and a writer that
// simply appended would produce a file where the audio after the gap has moved
// thirty seconds earlier than it happened. Every subsequent timestamp in the
// transcript would be wrong, and nothing about the file would look broken.
package wav

import (
	"encoding/binary"
	"fmt"
	"math"
	"os"
)

const headerSize = 44

// Writer writes one track to one file. Samples are placed at an absolute frame
// offset from a shared epoch, and any hole between the last write and this one
// is filled with silence.
type Writer struct {
	f          *os.File
	path       string
	sampleRate int
	channels   int

	// framesWritten counts per-channel frames, which is the unit an offset is
	// expressed in.
	framesWritten uint64
	// PaddedFrames records how much of the file is gap-fill rather than
	// captured audio. A large value on the system track is the signature of a
	// meeting nobody was speaking in, and worth surfacing rather than hiding.
	PaddedFrames uint64
	silence      []byte
}

func NewWriter(path string, sampleRate, channels int) (*Writer, error) {
	if sampleRate <= 0 || channels <= 0 {
		return nil, fmt.Errorf("invalid wav parameters: rate=%d channels=%d", sampleRate, channels)
	}
	f, err := os.Create(path)
	if err != nil {
		return nil, err
	}
	// Reserve the header; sizes are patched in at Close, once known.
	if _, err := f.Write(make([]byte, headerSize)); err != nil {
		f.Close()
		return nil, err
	}
	return &Writer{f: f, path: path, sampleRate: sampleRate, channels: channels}, nil
}

func (w *Writer) Path() string  { return w.path }
func (w *Writer) Frames() uint64 { return w.framesWritten }

// Duration returns the length of the file written so far, in seconds.
func (w *Writer) Duration() float64 {
	return float64(w.framesWritten) / float64(w.sampleRate)
}

// WriteAt places samples at frameOffset, filling any gap with silence.
//
// A frameOffset behind what is already written means the packet overlaps audio
// already committed; it is appended rather than rewound, because dropping it
// would lose speech and seeking backwards would corrupt what is already there.
func (w *Writer) WriteAt(frameOffset uint64, samples []int16) error {
	if frameOffset > w.framesWritten {
		gap := frameOffset - w.framesWritten
		if err := w.writeSilence(gap); err != nil {
			return err
		}
		w.PaddedFrames += gap
	}
	return w.writeSamples(samples)
}

func (w *Writer) writeSilence(frames uint64) error {
	const chunkFrames = 8192
	need := int(chunkFrames) * w.channels * 2
	if len(w.silence) < need {
		w.silence = make([]byte, need)
	}
	for frames > 0 {
		n := frames
		if n > chunkFrames {
			n = chunkFrames
		}
		b := w.silence[: int(n)*w.channels*2]
		if _, err := w.f.Write(b); err != nil {
			return err
		}
		w.framesWritten += n
		frames -= n
	}
	return nil
}

func (w *Writer) writeSamples(samples []int16) error {
	if len(samples) == 0 {
		return nil
	}
	buf := make([]byte, len(samples)*2)
	for i, s := range samples {
		binary.LittleEndian.PutUint16(buf[i*2:], uint16(s))
	}
	if _, err := w.f.Write(buf); err != nil {
		return err
	}
	w.framesWritten += uint64(len(samples) / w.channels)
	return nil
}

// Sync patches the RIFF header with the sizes written so far.
//
// Called periodically while a segment is open, so a recording that is killed
// mid-segment leaves a file that plays up to the last sync rather than one
// whose header still claims zero bytes. Without it, "a crash costs one chunk"
// means losing five minutes; with it, it means losing seconds.
func (w *Writer) Sync() error {
	if _, err := w.f.WriteAt(w.header(), 0); err != nil {
		return err
	}
	return w.f.Sync()
}

func (w *Writer) header() []byte {
	dataBytes := w.framesWritten * uint64(w.channels) * 2
	h := make([]byte, headerSize)
	copy(h[0:], "RIFF")
	binary.LittleEndian.PutUint32(h[4:], uint32(36+dataBytes))
	copy(h[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(h[16:], 16) // PCM fmt chunk size
	binary.LittleEndian.PutUint16(h[20:], 1)  // PCM
	binary.LittleEndian.PutUint16(h[22:], uint16(w.channels))
	binary.LittleEndian.PutUint32(h[24:], uint32(w.sampleRate))
	binary.LittleEndian.PutUint32(h[28:], uint32(w.sampleRate*w.channels*2)) // byte rate
	binary.LittleEndian.PutUint16(h[32:], uint16(w.channels*2))              // block align
	binary.LittleEndian.PutUint16(h[34:], 16)                                // bits
	copy(h[36:], "data")
	binary.LittleEndian.PutUint32(h[40:], uint32(dataBytes))
	return h
}

// Close patches the RIFF header with the final sizes and closes the file.
func (w *Writer) Close() error {
	if _, err := w.f.WriteAt(w.header(), 0); err != nil {
		w.f.Close()
		return err
	}
	return w.f.Close()
}

// ToInt16 converts a captured packet into 16-bit samples.
//
// Capture happens in the endpoint's own mix format, which on the target machine
// is 32-bit float. Storage is 16-bit PCM because that is what every transcriber
// and player accepts without negotiation; the conversion is the only one in the
// pipeline, and it happens here rather than in the helper so the capture
// boundary stays lossless.
func ToInt16(payload []byte, formatTag uint16, bitsPerSample uint16) ([]int16, error) {
	switch {
	case formatTag == 3 && bitsPerSample == 32:
		n := len(payload) / 4
		out := make([]int16, n)
		for i := 0; i < n; i++ {
			v := math.Float32frombits(binary.LittleEndian.Uint32(payload[i*4:]))
			// Clamp: a shared-mode float mix is not guaranteed to stay inside
			// unity, and wrapping a sample turns a loud moment into a bang.
			f := float64(v)
			if f > 1 {
				f = 1
			} else if f < -1 {
				f = -1
			}
			out[i] = int16(math.Round(f * 32767))
		}
		return out, nil

	case formatTag == 1 && bitsPerSample == 16:
		n := len(payload) / 2
		out := make([]int16, n)
		for i := 0; i < n; i++ {
			out[i] = int16(binary.LittleEndian.Uint16(payload[i*2:]))
		}
		return out, nil

	case formatTag == 1 && bitsPerSample == 32:
		n := len(payload) / 4
		out := make([]int16, n)
		for i := 0; i < n; i++ {
			out[i] = int16(int32(binary.LittleEndian.Uint32(payload[i*4:])) >> 16)
		}
		return out, nil
	}
	return nil, fmt.Errorf("unsupported capture format: tag=%d bits=%d", formatTag, bitsPerSample)
}
