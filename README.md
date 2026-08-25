# minutes

Records a meeting from a desktop — **both sides of it** — so that afterwards
somebody can answer what was decided.

This is **R1: Windows capture, and nothing else.** Transcription, summary and
delivery are later phases and are deliberately absent. The risk in this project
is concentrated here: transcription is well-trodden, and capturing two aligned
tracks on someone else's operating system is not.

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

$ ./dist/minutes record --duration 15s --out recordings
```

Two files come out, one per track, never a mix.

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

## Three things that are load-bearing

**Two tracks, never one mix.** Your track is you; the other is everyone else, so
speaker attribution is free. Mixing is irreversible, so this cannot be deferred
and revisited.

**Timestamps survive the capture boundary.** `GetBuffer` reports the performance
counter position for every packet, and both streams are stamped from that one
clock. Alignment is therefore arithmetic. Measured on the target machine: the
two tracks — running at *different* sample rates, 48000 and 44100 — land within
**0.37 ms** of each other.

This also handles a trap. A WASAPI loopback stream delivers nothing at all while
the machine is silent, so a writer that appended packets rather than placing
them by timestamp would slide everything after a quiet passage earlier by the
length of the quiet, and nothing about the resulting file would look broken.

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

## Next

**R2** — orchestrator: segments, manifest, `start`/`stop`, storage on disk.
