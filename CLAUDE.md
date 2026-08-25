---
description: Use for the meeting recorder — capturing microphone and system audio on a desktop, transcribing it, and delivering notes to the project a meeting was about.
---

# minutes

Records a meeting from a desktop — **both sides of it** — transcribes it, and
delivers notes to the project the meeting was actually about.

The name is the deliverable, not the mechanism. A recording nobody reads is
worth nothing; the point is that a meeting currently produces no durable
artifact, and afterwards nobody can answer what was decided.

> Rename to `recorder` if that reads better. One directory, one line here.

## What this is not

It is **not a tray application** installed on three operating systems that talks
to shabadoo. That was the first design and it was wrong.

In shabadoo's model this is a **worker**: a thing that registers a capability
(`audio.capture`) on the nodes that have it, is driven by that node's core
session, and delivers through the messaging plane that already exists. See
`shabadoo/docs/direction.md`, which this project is the worked example in.

The distinction is not pedantry. It decides where the code runs, what it is
allowed to do, and how it authenticates — all three fall out of being a worker
rather than an app.

## The shape

```
core session          "record the standup"
    ↓
minutes (worker)      native capture helper per OS → framed PCM on stdout
    ↓                 two tracks: microphone, system audio
segments              5-minute chunks, so a crash costs one chunk
    ↓
transcribe            each track separately, merged on the shared clock
    ↓                 your track is you; the other is everyone else
summarise             decisions, action items, open questions
    ↓
deliver               shabadoo's local socket → the right project's inbox
                                              → a notification
```

### Two tracks, never one mix

Separate microphone and system tracks make speaker attribution free — your
track is you, the other track is everyone else — and that is worth more than
any diarization model. **Mixing is irreversible**, so this is not a
optimisation to defer; it is a decision that cannot be revisited after the fact.

Measured on the target machine: the two tracks run at *different* sample rates
— the microphone at 48000 and the render endpoint at 44100 — and still land
within **0.37 ms** of each other, because they are placed by the shared clock
rather than by sample count.

### Framed, timestamped output — not raw PCM

Both platforms hand back a capture timestamp with every packet:
`IAudioCaptureClient::GetBuffer` returns a QueryPerformanceCounter position,
CoreAudio exposes host time. So the two tracks carry stamps from **one clock**
and alignment is arithmetic rather than cross-correlation.

That is why the helper's stdout carries a small frame header in front of each
chunk rather than bare samples: throwing the timestamp away at the capture
boundary makes it unrecoverable everywhere downstream, and drift between two
independently started streams is invisible until you try to align a ninety
minute transcript.

**Two clocks, and they are good at different things.** `GetBuffer` also reports
the endpoint's own sample counter, and R2 measured the difference. Wall-clock is
the only thing the two streams share, so it is what relates one track to the
other — but it carries about a millisecond of jitter, and placing *every* packet
by it accumulates that jitter in one direction, because a packet landing behind
the write head is appended while one landing ahead leaves a gap that is filled.
Both lengthen the file; neither shortens it. A nominal four-second segment came
out 176403 frames instead of 176400.

The sample counter has no jitter — it tracked wall-clock to 0.1 ms over 13
seconds — but counts from whenever its own stream started, so it says nothing
about the other track. Hence: wall-clock once per track to place its beginning,
the sample counter for everything after. Carry both in the frame header.

## Platforms

Verified on the target machines, not assumed:

| Platform | Microphone | System audio | Notes |
|---|---|---|---|
| **Windows** | WASAPI capture | **WASAPI loopback**, default render endpoint | the target, and R1 is built on it. Three Windows SDKs are installed, all carrying `audioclientactivationparams.h`; only the **2019** MSVC install is complete (see below) |
| **macOS** | avfoundation | **CoreAudio process taps** | `AudioHardwareTapping.h` + `CATapDescription.h` are in the SDK; Swift 6.3 present |
| **Linux** | pulse source | `<sink>.monitor` | works with ffmpeg alone |
| **WSL** | `RDPSource` works | **trap** | `RDPSink.monitor` carries only audio from Linux apps *inside* WSL |

### Windows: loopback, not a virtual cable

The machine has VB-Audio Virtual Cable and Voicemeeter installed, and routing
through them would work. **Do not.** It reroutes the machine's audio, and its
failure mode is the bad one: the recording succeeds while the human stops
hearing the meeting. System-wide WASAPI loopback captures what is playing and
leaves playback untouched.

**Idle is not the same as silent, and the distinction was measured.** A loopback
stream delivers no packets at all while the render endpoint is *idle* — before
any application has opened it. Once one has, silence arrives as real silent
packets and the stream stays dense: 1278 packets over 13 seconds with no gap,
across a passage where nothing was playing. So the gap to handle is at the start
of a recording, not in every quiet moment of a meeting. Both cases still need
placement by timestamp; only one of them actually occurs.

Accepted cost: loopback captures *everything*, so notification sounds and music
land on the meeting track. Process-specific loopback
(`ActivateAudioInterfaceAsync` with `AUDIOCLIENT_ACTIVATION_PARAMS`) records only
the meeting application and is the obvious refinement — deferred for sequencing,
not capability. Nothing blocks it; system-wide simply always works and needs no
process discovery, and mis-targeting a process records silence, which is a
failure you discover after the meeting.

### The transport is WSL–Windows interop, and it was measured

The shabadoo node runs in WSL and cannot see Windows audio. The two do not share
a kernel, so a Windows process cannot reach the agent's unix socket.

It does not have to: **a WSL process can exec a Windows binary and read its
stdout.** All 256 byte values survive that pipe with no CRLF translation —
tested before the design depended on it.

So there is no TCP listener, no named pipe, no shared filesystem, and **no new
credential**: the orchestrator stays a Linux process and keeps authenticating to
the agent socket by file permissions. A Windows-side orchestrator would have
needed a device token and broken that rule.

`/c/...` and `C:\...` name the same files, and `cl.exe` runs through interop
(verified: MSVC 19.29). The build stays one command from the Linux side.

**Pick the toolchain by completeness, not by version.** The Visual Studio 2022
and 18 installs on this machine have a `vcvars64.bat` but not the
`vcvarsall.bat` it calls, so "newest wins" selects an install that cannot
compile and fails one level down with a message that names the wrong file.
`native/windows/build.bat` probes for an install that has both.

### Two things about feeding this to a speech model

Both were found by doing it, not by planning it.

**Never hand it silence.** Whisper does not return nothing for nothing; it
invents, confidently, and the invention lands in the notes as something somebody
said. Segments below -60 dBFS are skipped, which the manifest's per-segment peak
makes free.

**Trim leading silence and add the offset back.** Given a file that opens with
silence, whisper anchors its first utterance at zero instead of where the speech
is — measured, a system track whose audio began 8.25s in had its opening line
stamped 00:00:00 while every later line in the same file was right. The system
track opens that way in every recording, because the render endpoint is idle
until something plays.

### Speaker attribution is free, but only with headphones

The microphone track is you and the system track is everyone else. **Verified on
real audio**: a recording with ten seconds of a live voice on the microphone
while the system track held digital silence attributed every line correctly, with
timing right to the second.

The qualifier is speakers. With the meeting playing out loud the microphone hears
the far end too, and the same sentence arrives attributed to both people. Echoes
are detected and removed from the microphone track, and the count is reported —
but a *short* fragment can evade the text comparison and be misattributed to you,
which is this design's worst failure in miniature. See `docs/gaps.md`.

Prefer headphones. With them the problem does not exist.

### A capture that dies is not a capture that ended

Failing to *open* an endpoint and failing *mid-stream* look identical from
outside unless they are separated deliberately. WASAPI returns
`AUDCLNT_E_DEVICE_INVALIDATED` from `GetBuffer` when the default endpoint
changes or a device is unplugged — and a helper that logs that and exits zero
records a meeting cut in half as a meeting that ended there.

So every mid-stream error marks the track failed and exits non-zero, the
recognisable `HRESULT`s are named rather than printed as eight digits, and the
orchestrator carries the helper's last message into the manifest. A failed
recording says what happened to it.

### WSL must refuse, not record

`RDPSink.monitor` only carries audio from Linux applications inside WSL. A
meeting in Teams, Zoom or a browser never touches it, so capturing there yields
your voice and silence. **Preflight must refuse with an explanation** rather
than produce a recording that looks successful and contains half a
conversation — a failure nobody discovers until the meeting is over.

## Delivering the result

Through shabadoo's local socket at `~/.config/shabadoo/agent.sock`, which
allowlists `/message/send` and `/notify`. The socket is 0600 in the operator's
own directory, so "can open it" means "is already this user" — **no credential,
no enrolment**.

Which project the notes belong to is a **judgment call, and a session makes
it**, not this program. That is the whole reason a worker is driven by a core
session rather than deciding for itself. The same applies to what mattered in
the meeting: `deliver` carries the transcript and the ask, and refuses without a
named destination rather than guessing one.

Treat a refusal differently from an outage. An unreachable agent costs nothing —
write the file and say so. A `429` is the coordinator's loop guard, and since
notes go out once per meeting, hitting it means something is sending in a loop.

Degrade to writing the file and saying so when the agent is unreachable. A
recorder that fails because a coordinator blipped would be worse than one that
never integrated.

## Build order

Each phase is independently useful. **The risk is entirely in the first two** —
transcription and summarisation are well-trodden; capturing two aligned tracks
on someone else's operating system is not.

- **R1 — Windows capture, and nothing else. Done.** MSVC, system-wide WASAPI
  loopback, framed timestamped chunks on stdout, driven from Linux over interop.
  Proven on the target machine with two non-silent tracks and a preflight that
  refuses. See `README.md` and `docs/protocol.md`.
- **R2 — orchestrator. Done.** Segments on shared-clock boundaries, a sidecar
  manifest, `start`/`stop`/`status`/`list`, storage on disk. Proven: full
  segments come out at exactly the nominal frame count on both tracks, the joins
  are continuous, and a `SIGKILL` mid-segment leaves every completed segment,
  a valid manifest, and a playable partial.
- **R3 — transcription. Done.** Per track, merged on the shared clock,
  pluggable. The default is **local** whisper rather than hosted: this machine
  has a usable GPU, so the confidential path is also the fast one, and audio
  leaves only when a hosted backend is named in the config. Which backend ran is
  written into the manifest and the transcript.
- **R4 — delivery. Done.** The transcript goes to a session's inbox through the
  local agent socket, with a brief asking for decisions, action items and open
  questions, plus a human notification. Degrades to writing `delivery.md` when
  the agent is unreachable.

  **Summarising moved out of the worker.** The build order put it here, but the
  machine has no local model and no API key, and the session driving the worker
  can already do the job — so the worker assembles the material and states the
  ask, and the session writes the notes. This is the same argument that already
  applied to choosing the project: it is a judgment, and a session makes it.
- **R5 — macOS**, CoreAudio process taps, same framed-stdout shape.

## Conventions

- **Go for the orchestrator**, matching shabadoo. Native only where the platform
  forces it: the capture helpers, and nothing else.
- **Store audio on disk, metadata in a manifest.** Never blobs in a database —
  shabadoo's `hub/release.go` is the pattern, and the reason is that a database
  gets copied by every backup.
- **Recording is a trust matter, and in some places a legal one.** Make an active
  recording obvious rather than quiet, and say in the summary that it was
  recorded. Cheap now, awkward after the first meeting somebody did not know was
  being recorded.
- **Verify against a real device before believing a design.** Everything in the
  platform table above was checked on the actual machines; the parts that were
  reasoned about instead were the parts that turned out wrong.
