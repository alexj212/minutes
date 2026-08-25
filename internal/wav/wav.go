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
	"io"
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

// TrimLeadingSilence copies src to dst without its leading silence, and returns
// how many seconds it removed.
//
// This exists because of how a speech model behaves, not because the silence is
// unwanted. Given a file that opens with a long silence, whisper anchors its
// first utterance at zero rather than where the speech actually is — measured,
// a system track whose audio began 8.25s in had its opening line timestamped
// 00:00:00. Every later utterance in the same file was correct, so the error is
// invisible unless you check, and it lands on the first thing anybody said.
//
// The system track opens this way in every recording, because the render
// endpoint is idle until something plays.
//
// Only exact digital silence is trimmed. That is what gap-fill is made of, so
// this cannot remove quiet speech or room tone — only samples this program
// wrote itself, or ones the endpoint declared silent.
func TrimLeadingSilence(src, dst string) (float64, error) {
	in, err := os.Open(src)
	if err != nil {
		return 0, err
	}
	defer in.Close()

	var h [headerSize]byte
	if _, err := io.ReadFull(in, h[:]); err != nil {
		return 0, fmt.Errorf("%s is too short to be a WAV: %w", src, err)
	}
	if string(h[0:4]) != "RIFF" || string(h[8:12]) != "WAVE" {
		return 0, fmt.Errorf("%s is not a RIFF/WAVE file", src)
	}
	channels := int(binary.LittleEndian.Uint16(h[22:]))
	rate := int(binary.LittleEndian.Uint32(h[24:]))
	bits := int(binary.LittleEndian.Uint16(h[34:]))
	if bits != 16 || channels <= 0 || rate <= 0 {
		return 0, fmt.Errorf("%s is %d-bit %d-channel at %d Hz; only 16-bit PCM is handled", src, bits, channels, rate)
	}
	blockAlign := channels * 2

	// Scan for the first frame that is not all zero.
	buf := make([]byte, 1<<16)
	var silentBytes int64
	found := false
	for !found {
		n, err := io.ReadFull(in, buf)
		if n == 0 {
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				break
			}
			return 0, err
		}
		chunk := buf[:n-n%blockAlign]
		for i := 0; i < len(chunk); i += blockAlign {
			allZero := true
			for j := 0; j < blockAlign; j++ {
				if chunk[i+j] != 0 {
					allZero = false
					break
				}
			}
			if !allZero {
				silentBytes += int64(i)
				found = true
				break
			}
		}
		if !found {
			silentBytes += int64(len(chunk))
		}
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			break
		}
	}

	skipped := float64(silentBytes) / float64(blockAlign) / float64(rate)

	// Nothing to gain, and re-writing the file would only be a chance to get it
	// wrong.
	if !found || silentBytes == 0 {
		return 0, copyFile(src, dst)
	}

	out, err := NewWriter(dst, rate, channels)
	if err != nil {
		return 0, err
	}
	if _, err := in.Seek(int64(headerSize)+silentBytes, io.SeekStart); err != nil {
		out.Close()
		return 0, err
	}
	copied, err := io.Copy(out.f, in)
	if err != nil {
		out.Close()
		return 0, err
	}
	out.framesWritten = uint64(copied) / uint64(blockAlign)
	if err := out.Close(); err != nil {
		return 0, err
	}
	return skipped, nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
