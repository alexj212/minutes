// Package frame reads the capture helper's stdout stream.
//
// The wire format is documented in docs/protocol.md; this is the reading half
// of it. The writing half is native/windows/capture.cpp.
package frame

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// Magic leads every frame header.
const Magic uint32 = 0x314E494D // "MIN1"

// HeaderSize is the fixed size of a frame header, in bytes.
const HeaderSize = 32

// Type identifies what a frame carries.
type Type uint16

const (
	TypeTrackInfo Type = 1
	TypeAudio     Type = 2
	TypeEnd       Type = 3
	TypeLog       Type = 4
)

func (t Type) String() string {
	switch t {
	case TypeTrackInfo:
		return "TRACK_INFO"
	case TypeAudio:
		return "AUDIO"
	case TypeEnd:
		return "END"
	case TypeLog:
		return "LOG"
	}
	return fmt.Sprintf("UNKNOWN(%d)", uint16(t))
}

// Track identifies which of the two streams a frame belongs to. They are never
// mixed: track 0 is you, track 1 is everyone else.
type Track uint16

const (
	TrackMic    Track = 0
	TrackSystem Track = 1
)

func (t Track) String() string {
	switch t {
	case TrackMic:
		return "mic"
	case TrackSystem:
		return "system"
	}
	return fmt.Sprintf("track%d", uint16(t))
}

// Frame flags.
//
// On a TRACK_INFO frame, bit 0 means the track is scoped to one process. Its
// device position then counts frames delivered rather than time elapsed —
// a process loopback stream delivers nothing while its target is quiet — so
// that counter must not be used to place audio in time.
const FlagProcessScoped uint32 = 1 << 0

// On an AUDIO frame, as reported by WASAPI for the packet.
const (
	FlagSilent         uint32 = 1 << 0
	FlagDiscontinuity  uint32 = 1 << 1
	FlagTimestampError uint32 = 1 << 2
)

// Sample format tags. These match the WAVE_FORMAT_* values.
const (
	FormatPCM   uint16 = 1
	FormatFloat uint16 = 3
)

// Frame is one decoded frame.
type Frame struct {
	Type Type
	Track Track
	// QPC100ns is the performance-counter position, in 100-nanosecond units,
	// at which the endpoint read this packet. Both tracks are stamped from the
	// same clock, so this — not the sample count — is the authority on where a
	// packet belongs in time.
	QPC100ns  uint64
	DevicePos uint64 // device stream position, in audio frames
	Flags     uint32
	Payload   []byte
}

// TrackInfo describes a track's format. It arrives once per track, before any
// audio for it.
type TrackInfo struct {
	SampleRate    uint32
	Channels      uint16
	BitsPerSample uint16
	FormatTag     uint16
	BlockAlign    uint16
	QPCFrequency  uint64
	Device        string
	// ProcessScoped means this track captures one application rather than the
	// whole machine, and its device counter cannot be trusted as a clock.
	ProcessScoped bool
}

// BytesPerSecond is how much data one second of this track occupies.
func (ti TrackInfo) BytesPerSecond() int {
	return int(ti.SampleRate) * int(ti.BlockAlign)
}

// ParseTrackInfo decodes a TRACK_INFO payload.
func ParseTrackInfo(p []byte) (TrackInfo, error) {
	if len(p) < 24 {
		return TrackInfo{}, fmt.Errorf("track info payload too short: %d bytes", len(p))
	}
	ti := TrackInfo{
		SampleRate:    binary.LittleEndian.Uint32(p[0:]),
		Channels:      binary.LittleEndian.Uint16(p[4:]),
		BitsPerSample: binary.LittleEndian.Uint16(p[6:]),
		FormatTag:     binary.LittleEndian.Uint16(p[8:]),
		BlockAlign:    binary.LittleEndian.Uint16(p[10:]),
		QPCFrequency:  binary.LittleEndian.Uint64(p[12:]),
	}
	nameLen := binary.LittleEndian.Uint32(p[20:])
	if int(nameLen) > len(p)-24 {
		return TrackInfo{}, fmt.Errorf("track info name length %d exceeds payload", nameLen)
	}
	ti.Device = string(p[24 : 24+nameLen])

	if ti.BlockAlign == 0 {
		return TrackInfo{}, errors.New("track info declares blockAlign 0")
	}
	if ti.SampleRate == 0 {
		return TrackInfo{}, errors.New("track info declares sample rate 0")
	}
	return ti, nil
}

// maxPayload bounds a single frame's payload. The helper sends packets of a few
// milliseconds; anything approaching this means the stream has desynchronised
// and a length field is being read out of noise.
const maxPayload = 16 << 20

// Reader decodes frames from the helper's stdout.
type Reader struct {
	r   io.Reader
	hdr [HeaderSize]byte
}

func NewReader(r io.Reader) *Reader { return &Reader{r: r} }

// Next returns the next frame, or io.EOF at a clean end of stream.
//
// The returned Payload is only valid until the next call.
func (fr *Reader) Next() (*Frame, error) {
	if _, err := io.ReadFull(fr.r, fr.hdr[:]); err != nil {
		if err == io.ErrUnexpectedEOF {
			return nil, fmt.Errorf("truncated frame header: %w", err)
		}
		return nil, err
	}

	magic := binary.LittleEndian.Uint32(fr.hdr[0:])
	if magic != Magic {
		return nil, fmt.Errorf("bad frame magic %#08x (want %#08x): stream is not in sync", magic, Magic)
	}

	payloadLen := binary.LittleEndian.Uint32(fr.hdr[24:])
	if payloadLen > maxPayload {
		return nil, fmt.Errorf("frame declares implausible payload of %d bytes", payloadLen)
	}

	f := &Frame{
		Type:      Type(binary.LittleEndian.Uint16(fr.hdr[4:])),
		Track:     Track(binary.LittleEndian.Uint16(fr.hdr[6:])),
		QPC100ns:  binary.LittleEndian.Uint64(fr.hdr[8:]),
		DevicePos: binary.LittleEndian.Uint64(fr.hdr[16:]),
		Flags:     binary.LittleEndian.Uint32(fr.hdr[28:]),
	}
	if payloadLen > 0 {
		f.Payload = make([]byte, payloadLen)
		if _, err := io.ReadFull(fr.r, f.Payload); err != nil {
			return nil, fmt.Errorf("truncated %s payload of %d bytes: %w", f.Type, payloadLen, err)
		}
	}
	return f, nil
}
