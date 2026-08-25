# minutes

Records a meeting from a desktop — **both sides of it** — so that afterwards
somebody can answer what was decided.

This is **R1 and R2**: capture, and the storage and lifecycle around it.
Transcription, summary and delivery are later phases and are deliberately
absent. The risk in this project is concentrated here: transcription is
well-trodden, and capturing two aligned tracks on someone else's operating
system is not.

For what this is and why it is a worker rather than a tray application, see
[CLAUDE.md](CLAUDE.md). For the wire format, see [docs/protocol.md](docs/protocol.md).

## What works

```
$ make all
$ ./dist/minutes preflight
platform: wsl
helper:   /c/projects/minutes/dist/minutes-capture.exe
  mic     ok   wasapi-capture   Microphone (2- Insta360 Link)  48000 Hz, 2 ch, 32-bit
  system  ok   wasapi-loopback  Speakers (Realtek(R) Audio)  44100 Hz, 2 ch, 32-bit

Both tracks can be captured.

$ ./dist/minutes start --name standup
┌──────────────────────────────────────────────┐
│  ● RECORDING — microphone and system audio   │
└──────────────────────────────────────────────┘
  id:       2026-08-25-104604-standup
  files:    recordings/2026-08-25-104604-standup
  segments: 5m0s

  stop with:  minutes stop 2026-08-25-104604-standup

$ ./dist/minutes stop
```

`start` returns and the recording continues without it, because a meeting is
longer than a command. `list`, `status` and `stop` read the directory it left
behind. `record` does the same thing in the foreground.

Each track becomes a series of WAV segments with a `manifest.json` beside them —
never a mix, and never a blob in a database, because a database gets copied by
every backup and an hour of stereo audio is hundreds of megabytes to copy
nightly forever.

```
recordings/2026-08-25-104604-standup/
  manifest.json
  mic-000.wav      system-000.wav
  mic-001.wav      system-001.wav
  recorder.log     recorder.pid
```

## The shape of it

A C++ helper built with MSVC captures both endpoints and writes framed,
timestamped chunks to stdout. A Go orchestrator in WSL runs that helper over
interop, reads the frames, and writes one WAV per track.

```
minutes (Linux/WSL)  ──exec──▶  minutes-capture.exe (Windows)
                                    ├─ WASAPI capture   → track 0, microphone
                                    └─ WASAPI loopback  → track 1, system
        ◀── framed timestamped chunks on stdout ──┘
```

The orchestrator stays a Linux process, which is the point: it keeps
authenticating to shabadoo's agent socket by file permissions, and needs no
device token. A Windows-side orchestrator would have needed one.

## Segments land on the same boundaries in both tracks

Segment k of the microphone covers the same wall-clock window as segment k of
the system track, even though the two run at different sample rates. Measured:
every full four-second segment came out at exactly 176400 frames at 44100 Hz and
exactly 192000 at 48000, with the joins continuous — no sample lost or repeated
where one chunk meets the next.

That is what lets a later phase transcribe the two separately and merge them
without re-deriving any alignment.

## Three things that are load-bearing

**Two tracks, never one mix.** Your track is you; the other is everyone else, so
speaker attribution is free. Mixing is irreversible, so this cannot be deferred
and revisited.

**Two clocks, each used for what it is good at.** `GetBuffer` reports both a
performance-counter position and the endpoint's own sample counter. Wall-clock
is the only thing the two streams share, so it places each track's *beginning*
relative to the other — measured at **0.37 ms** apart across 48000 and 44100.
But it carries a millisecond of jitter, and placing every packet by it
accumulates that jitter in one direction, because a packet landing behind the
write head is appended and one landing ahead leaves a gap that is filled: both
make the file longer, never shorter. Measured before this was fixed, a nominal
four-second segment came out 176403 frames instead of 176400.

So the sample counter, which has no jitter at all — it tracked wall-clock to
0.1 ms over 13 seconds — carries everything after the first packet.

This also handles a trap. A WASAPI loopback stream delivers nothing while the
render endpoint is **idle** — before any application has opened it. (Once one
has, silence arrives as real silent packets and the stream stays dense.) A
writer that appended rather than placing by timestamp would slide everything
after such a gap earlier by the length of it, and nothing about the resulting
file would look broken.

**Preflight refuses rather than recording half a meeting.** Under WSL,
`RDPSink.monitor` exists, opens, and records — but carries audio only from Linux
applications inside WSL. A meeting in Teams, Zoom or a browser never touches it,
so recording there yields your voice and silence, in a file of the right length
that plays. There is no code path in this program that records from it.

## Loopback, not a virtual cable

The target machine has VB-Audio Virtual Cable and Voicemeeter installed, and
routing through them would work. It is not used, because its failure mode is the
bad one: the recording succeeds while the human stops hearing the meeting.
System-wide WASAPI loopback observes the render endpoint and leaves playback
untouched.

The accepted cost is that loopback captures everything, so notification sounds
land on the meeting track. Process-specific loopback is the obvious refinement
and nothing blocks it; system-wide simply always works and needs no process
discovery, and mis-targeting a process records silence — a failure you discover
after the meeting.

## Building

Requires WSL with interop enabled, a complete MSVC toolchain, and Go.

```
make all         # Go binary + C++ helper
make test        # unit tests
make preflight   # ask the machine whether it could record right now
```

A note on the toolchain: `native/windows/build.bat` probes for an install that
has both `vcvarsall.bat` and `vcvars64.bat`, rather than taking the newest one.
On the target machine the Visual Studio 2022 and 18 installs have a `vcvars64.bat`
but not the `vcvarsall.bat` it calls, so "newest wins" selects an install that
cannot compile and fails one level down with a confusing message.

## Verifying a recording

Both tracks must be non-silent. The failure to look for is a silent system
track, and `record` exits non-zero and says so if it sees one.

```
$ ffprobe -v error -show_entries stream=sample_rate,channels,duration \
    -of default=noprint_wrappers=1 recordings/<prefix>-system.wav
$ ffmpeg -i recordings/<prefix>-system.wav -af volumedetect -f null /dev/null
```

## Recording is a trust matter

An active recording is announced rather than quiet, and a track that came out
silent is reported rather than left to be discovered later. When this grows a
summary, the summary should say the meeting was recorded.

## What a crash costs

Segments bound it, and the header is patched more often than segments rotate:
every five seconds, or every quarter-segment if the segments are shorter than
that. A recording killed with `SIGKILL` mid-segment keeps every completed
segment, keeps a valid manifest — it is written to a temporary file and renamed,
so an interrupted write leaves the previous one rather than a broken one — and
keeps the in-progress segment as a playable file, minus at most one sync
interval.

`status` reconciles the manifest against reality: a directory that claims to be
recording with no supervisor alive is reported as `interrupted`, not as
`recording`.

## Next

**R3** — transcription, per track, merged on the shared clock.
