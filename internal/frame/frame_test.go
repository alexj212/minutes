package frame

import (
	"bytes"
	"encoding/binary"
	"io"
	"testing"
)

// build assembles a frame the way the C++ helper does, so the test exercises
// the same layout docs/protocol.md specifies rather than a Go-side convenience.
func build(typ Type, track Track, qpc, devicePos uint64, flags uint32, payload []byte) []byte {
	h := make([]byte, HeaderSize)
	binary.LittleEndian.PutUint32(h[0:], Magic)
	binary.LittleEndian.PutUint16(h[4:], uint16(typ))
	binary.LittleEndian.PutUint16(h[6:], uint16(track))
	binary.LittleEndian.PutUint64(h[8:], qpc)
	binary.LittleEndian.PutUint64(h[16:], devicePos)
	binary.LittleEndian.PutUint32(h[24:], uint32(len(payload)))
	binary.LittleEndian.PutUint32(h[28:], flags)
	return append(h, payload...)
}

func buildTrackInfo(track Track, rate uint32, ch, bits, tag, blockAlign uint16, device string) []byte {
	p := make([]byte, 24+len(device))
	binary.LittleEndian.PutUint32(p[0:], rate)
	binary.LittleEndian.PutUint16(p[4:], ch)
	binary.LittleEndian.PutUint16(p[6:], bits)
	binary.LittleEndian.PutUint16(p[8:], tag)
	binary.LittleEndian.PutUint16(p[10:], blockAlign)
	binary.LittleEndian.PutUint64(p[12:], 10_000_000)
	binary.LittleEndian.PutUint32(p[20:], uint32(len(device)))
	copy(p[24:], device)
	return build(TypeTrackInfo, track, 0, 0, 0, p)
}

func TestReaderDecodesAStream(t *testing.T) {
	var buf bytes.Buffer
	buf.Write(buildTrackInfo(TrackSystem, 44100, 2, 32, 3, 8, "Speakers"))
	buf.Write(build(TypeAudio, TrackSystem, 123456789, 512, FlagSilent, []byte{1, 2, 3, 4}))
	buf.Write(build(TypeLog, TrackMic, 0, 0, 0, []byte("hello")))
	buf.Write(build(TypeEnd, TrackSystem, 0, 999, 0, nil))

	r := NewReader(&buf)

	f, err := r.Next()
	if err != nil {
		t.Fatal(err)
	}
	if f.Type != TypeTrackInfo || f.Track != TrackSystem {
		t.Fatalf("first frame is %s/%s, want TRACK_INFO/system", f.Type, f.Track)
	}
	info, err := ParseTrackInfo(f.Payload)
	if err != nil {
		t.Fatal(err)
	}
	if info.SampleRate != 44100 || info.Channels != 2 || info.FormatTag != 3 || info.Device != "Speakers" {
		t.Fatalf("track info decoded as %+v", info)
	}
	if got, want := info.BytesPerSecond(), 44100*8; got != want {
		t.Errorf("BytesPerSecond = %d, want %d", got, want)
	}

	f, err = r.Next()
	if err != nil {
		t.Fatal(err)
	}
	if f.Type != TypeAudio {
		t.Fatalf("second frame is %s, want AUDIO", f.Type)
	}
	if f.QPC100ns != 123456789 {
		t.Errorf("QPC100ns = %d, want 123456789 — the timestamp was lost", f.QPC100ns)
	}
	if f.DevicePos != 512 {
		t.Errorf("DevicePos = %d, want 512", f.DevicePos)
	}
	if f.Flags&FlagSilent == 0 {
		t.Error("silent flag did not survive decoding")
	}
	if !bytes.Equal(f.Payload, []byte{1, 2, 3, 4}) {
		t.Errorf("payload = %v, want [1 2 3 4]", f.Payload)
	}

	f, err = r.Next()
	if err != nil {
		t.Fatal(err)
	}
	if f.Type != TypeLog || string(f.Payload) != "hello" {
		t.Errorf("third frame is %s %q", f.Type, f.Payload)
	}

	f, err = r.Next()
	if err != nil {
		t.Fatal(err)
	}
	if f.Type != TypeEnd || f.DevicePos != 999 {
		t.Errorf("fourth frame is %s devicePos=%d", f.Type, f.DevicePos)
	}

	if _, err := r.Next(); err != io.EOF {
		t.Errorf("after the last frame got %v, want io.EOF", err)
	}
}

// Silently resynchronising on a bad magic would mean interpreting arbitrary
// bytes as a length and a timestamp. Stopping is the honest outcome.
func TestReaderRejectsBadMagic(t *testing.T) {
	b := build(TypeAudio, TrackMic, 1, 2, 0, []byte{9})
	binary.LittleEndian.PutUint32(b[0:], 0xDEADBEEF)
	if _, err := NewReader(bytes.NewReader(b)).Next(); err == nil {
		t.Fatal("expected an error on bad magic, got nil")
	}
}

func TestReaderRejectsTruncatedPayload(t *testing.T) {
	b := build(TypeAudio, TrackMic, 1, 2, 0, []byte{1, 2, 3, 4, 5, 6, 7, 8})
	b = b[:len(b)-4] // lose half the payload
	_, err := NewReader(bytes.NewReader(b)).Next()
	if err == nil {
		t.Fatal("expected an error on a truncated payload, got nil")
	}
	if err == io.EOF {
		t.Fatal("a truncated payload reported a clean EOF, which hides a lost tail")
	}
}

// The length bound must reject the frame on its own merits, before any read is
// attempted. Feeding the reader an unlimited source is what makes this a real
// test: against a short buffer the read would fail anyway, and the test would
// pass with the bound deleted.
func TestReaderRejectsImplausiblePayloadLength(t *testing.T) {
	b := build(TypeAudio, TrackMic, 1, 2, 0, nil)
	binary.LittleEndian.PutUint32(b[24:], maxPayload+1)

	stream := io.MultiReader(bytes.NewReader(b), endlessZeros{})
	if _, err := NewReader(stream).Next(); err == nil {
		t.Fatal("expected an error on an implausible payload length, got nil")
	}
}

// endlessZeros never runs out, so a reader that trusts the length field will
// happily satisfy it rather than erroring on a short buffer.
type endlessZeros struct{}

func (endlessZeros) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 0
	}
	return len(p), nil
}

func TestParseTrackInfoRejectsNonsense(t *testing.T) {
	t.Run("short", func(t *testing.T) {
		if _, err := ParseTrackInfo([]byte{1, 2, 3}); err == nil {
			t.Error("expected an error, got nil")
		}
	})
	t.Run("zero block align", func(t *testing.T) {
		p := make([]byte, 24)
		binary.LittleEndian.PutUint32(p[0:], 48000) // rate ok, blockAlign 0
		if _, err := ParseTrackInfo(p); err == nil {
			t.Error("expected an error for blockAlign 0, got nil")
		}
	})
	t.Run("name overruns payload", func(t *testing.T) {
		p := make([]byte, 24)
		binary.LittleEndian.PutUint32(p[0:], 48000)
		binary.LittleEndian.PutUint16(p[10:], 8)
		binary.LittleEndian.PutUint32(p[20:], 500)
		if _, err := ParseTrackInfo(p); err == nil {
			t.Error("expected an error for an overrunning name, got nil")
		}
	})
}
