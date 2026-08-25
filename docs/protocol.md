# The framed stdout protocol

The capture helper writes a byte stream to stdout. Every byte of it is a frame:
a fixed 32-byte header, then a payload of the length the header declares.

There is no ambiguity about where a frame ends, and nothing in the stream is
positional — a reader that starts at byte zero and trusts `payloadLen` stays in
sync forever, and a reader that loses sync can find the magic again.

## Why a header at all

`IAudioCaptureClient::GetBuffer` hands back a timestamp with every packet: the
performance-counter position, in 100-nanosecond units, at which the endpoint
read that data. Both the microphone stream and the loopback stream get their
stamps from that one clock.

So alignment between the two tracks is arithmetic — subtract, divide by the
sample rate — rather than cross-correlation of waveforms.

Discarding that stamp at the capture boundary makes it unrecoverable everywhere
downstream. Drift between two independently started streams is invisible until
you try to align a ninety-minute transcript, at which point the recording
already exists and the information needed to fix it does not. Hence the header.

## Frame header — 32 bytes, little-endian

| Offset | Size | Field | Meaning |
|---|---|---|---|
| 0 | 4 | `magic` | `0x314E494D` — `"MIN1"` |
| 4 | 2 | `type` | 1 `TRACK_INFO`, 2 `AUDIO`, 3 `END`, 4 `LOG` |
| 6 | 2 | `track` | 0 microphone, 1 system |
| 8 | 8 | `qpc100ns` | performance-counter position, 100ns units, as `GetBuffer` reported it |
| 16 | 8 | `devicePos` | device stream position, in audio frames |
| 24 | 4 | `payloadLen` | payload bytes following this header |
| 28 | 4 | `flags` | bit0 `SILENT`, bit1 `DISCONTINUITY`, bit2 `TIMESTAMP_ERROR` |

`qpc100ns` is the shared clock and is the authority for alignment. `devicePos`
is carried alongside it as a cross-check: the two disagreeing means the stream
glitched, and that is worth knowing before a transcript is built on it.

## TRACK_INFO payload

Emitted once per track, before that track's first `AUDIO` frame. Capture is in
the endpoint's own mix format — the helper converts nothing, because a helper
that resamples is a helper that loses information it cannot return.

| Offset | Size | Field |
|---|---|---|
| 0 | 4 | `sampleRate` |
| 4 | 2 | `channels` |
| 6 | 2 | `bitsPerSample` |
| 8 | 2 | `formatTag` — 1 integer PCM, 3 IEEE float |
| 10 | 2 | `blockAlign` |
| 12 | 8 | `qpcFrequency` — `QueryPerformanceFrequency`, informational |
| 20 | 4 | `nameLen` |
| 24 | n | endpoint friendly name, UTF-8 |

## AUDIO payload

Raw sample bytes in the format `TRACK_INFO` declared, exactly as the endpoint
produced them. When WASAPI marks a packet `AUDCLNT_BUFFERFLAGS_SILENT` the
helper writes real zero samples rather than omitting the packet, so the byte
stream stays dense and `flags` still records that the silence was declared
rather than recorded.

## LOG payload

UTF-8 text. Diagnostics travel in-band so a failure keeps its position in the
timeline instead of arriving out of order on stderr.

## Two tracks, never one mix

Track 0 is the microphone and track 1 is the system. They are never summed.
Mixing is irreversible, and keeping them apart makes speaker attribution free —
your track is you, the other track is everyone else — which is worth more than
any diarization model.
