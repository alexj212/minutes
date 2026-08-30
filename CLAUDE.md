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
segments              5-minute chunks; the header is synced every few seconds,
    ↓                 so a crash costs seconds rather than a chunk
transcribe            each track separately, merged on the shared clock
    ↓                 your track is you; the other is everyone else
    ↓                 starts by itself when a recording stops, in the
    ↓                 background — it runs at about real time
deliver               shabadoo's local socket → the right project's inbox
                                              → a notification
    ↓
core session          writes the notes: decisions, action items, open questions
                      and files them where they belong
```

Summarising sits at the *bottom* of that, not the middle. What mattered in a
meeting is a judgment, and a session driven by a person is where judgments are
made — the same reason the worker does not choose which project the notes belong
to. See `docs/status.md` for what is built and what is still assumed.

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
| **macOS** | HAL audio unit, default input | **CoreAudio process tap** through a private aggregate | R5 is built on it and proven on 26.5.2. Taps need no entitlement, but do need a `kTCCServiceAudioCapture` grant — held against whatever **launched** the helper, not the helper, so its own signature does not buy a durable grant (see below). The microphone is a separate grant that can be denied while the tap is allowed |
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

System-wide loopback captures *everything*, so notification sounds, music and a
video in another window land on the meeting track. **Process-specific loopback
is now built**: `minutes start --app Zoom` uses `ActivateAudioInterfaceAsync`
with `AUDIOCLIENT_ACTIVATION_PARAMS` to record one process and its children.
Proven by playing a tone from one process while capturing another — the tone is
20.7 dB down, which is leakage rather than capture.

It is not the default, because system-wide always works and needs no process
discovery while mis-targeting records silence, a failure you discover after the
meeting. `--app` refuses a name matching nothing or matching two things rather
than quietly widening, and the target is chosen from what the audio engine says
is actually producing sound.

Two things that cost time and are not in any documentation prominently enough:

- **The completion handler must be agile.** Without a free-threaded marshaler the
  activation is refused with `E_ILLEGAL_METHOD_CALL`, which reads as "you called
  this wrong" rather than "your callback cannot be reached".
- **A process-scoped stream's device counter measures delivered frames, not
  elapsed time**, because it delivers nothing while the target is quiet. Placing
  audio with it makes the timeline guard fire continuously — measured at 98
  re-anchors in ten seconds. Such a track is placed by wall-clock alone.

### macOS: a tap, and three things the platform gets wrong

Everything here was measured on the target Mac. The parts that were reasoned
about first were, again, the parts that turned out wrong.

A `CATapDescription` excluding no processes is the direct equivalent of
system-wide WASAPI loopback, and it is read through an aggregate device. Two
properties of that arrangement are deliberate and neither is a default worth
relying on:

- **The tap is `CATapUnmuted`.** Audio is captured *and* still reaches the
  hardware. `CATapMuted` and `CATapMutedWhenTapped` are the refused failure —
  the recording succeeding while the human stops hearing the meeting —
  available as a one-line mistake.
- **The aggregate is private and carries the default output device only as a
  clock source.** It never becomes anybody's default and the user's output is
  not changed. A tap-only aggregate with no sub-device *creates and starts
  without error and then delivers nothing*, which is the enumerate-versus-start
  failure wearing a new hat.

CoreAudio reclaims taps and private aggregates when the creating process dies —
verified across five killed helpers, nothing leaked — so a crash costs the
recording but not the machine's audio configuration.

**The sample rate both obvious properties report is not the rate audio arrives
at.** This is the one that would have gone unnoticed:

    kAudioTapPropertyFormat        -> 48000    the obvious property
    aggregate input StreamFormat   -> 48000    the other obvious property
    aggregate NominalSampleRate    -> 44100    the truth
    measured delivery              -> ~44184   agrees with 44100

Declaring 48000 in `TRACK_INFO` makes the orchestrator write a WAV header of
48000 over samples that are 44100. The file opens, plays, and is 8% fast, with
every transcript timestamp sliding — roughly ten minutes of drift across a
two-hour call, in a recording that looks entirely fine. It was caught only
because the wall-clock and device-position spans disagreed by 488 ms instead of
zero, and the deficit was the same *fraction* in every run: 7.95%, 7.99% —
44100/48000, not anything to do with silence. Read
`kAudioDevicePropertyNominalSampleRate` on the aggregate and believe that.

**Consent is a different service than you expect, it is checked later than you
expect, and it blocks rather than fails.** The gate *for the tap* is
`kTCCServiceAudioCapture` — not `kTCCServiceMicrophone` — and it is not
consulted when the tap is created. Creating the tap and the aggregate both
return `noErr` on a machine that cannot capture a single sample. It is checked
in the `AudioDeviceCreateIOProcIDWithBlock` path, where it does not return
"denied": it sits in a synchronous `mach_msg` to `coreaudiod` indefinitely,
waiting for a dialog to be answered. The identical call on an aggregate with no
tap returns instantly, which is how that was isolated.

So every setup call is bounded and a timeout is reported as its own outcome.
Without that the helper produces no report at all and the operator is told "the
capture helper produced no report", which is true and useless.

**That is the *undecided* case. A decided "no" behaves in the opposite
direction, and the two need separating.** macOS remembers a denial exactly as
it remembers a grant, so a denied device never prompts again: nothing blocks,
no dialog appears, and every call returns success. Measured on the target Mac
with the microphone denied — the helper opened the device, emitted `TRACK_INFO`
at 48000 Hz, delivered 188 audio packets and exited **zero**. Of the 96256
samples in them there was exactly **1 distinct value**, and it was 0.

So the platform gives you a hang when it wants an answer and a clean success
when it already has one, which means **"no dialog appeared" is not evidence of
a grant** and the `waiting` state cannot catch this at all: from inside the
helper, told-no is indistinguishable from working-and-quiet. Only looking at
the samples separates them, which is why preflight probes the microphone and
refuses on an unvarying signal rather than a silent one — a quiet room still
has a noise floor, and a constant signal is a device that is not listening.

The two services are also independent in practice, not just in name: on that
same machine the tap was delivering system audio at 44100 Hz while the
microphone returned zeros. **`kTCCServiceAudioCapture` does not carry
`kTCCServiceMicrophone` with it**, so a machine that has proven it can record
the far end has proven nothing about recording the operator.

**TCC records the grant against the responsible process, not the helper.** The
dialog names whatever launched it — here the session coordinator — and
`minutes-capture` appears nowhere in System Settings. It works, and it discloses
the wrong program's name.

Measured rather than assumed, and it is not a launch-shape problem: across 24
attribution checks — run directly, run under `setsid`, and spawned by the
orchestrator — TCC named the launcher every time. Signing did not change who
is held responsible, only what is known about the accessor, which now records
as `com.github.alexj212.minutes-capture`. The system can name the recorder and reports
the launcher anyway.

`responsibility_spawnattrs_setdisclaim` would fix it, but it is a
`posix_spawnattr` applied by the parent and Go's `os/exec` has no hook for
one — so it means cgo in `internal/capture`, which is the half of this project
that builds anywhere. The helpers are native because the platform forces it;
the orchestrator is portable because nothing forces it not to be. Spending
that on a disclosure improvement would make it "portable except on darwin",
which is the kind of exception that does not come back.

**So this is a real cost accepted, not an open question.** The trigger to
revisit is not somebody complaining about the dialog: it is the moment
something else carries the disclosure instead — a desktop indicator, most
likely — because whatever does that job had better name the right program.

**The grant sticks only if the helper is signed, and that is why `build.sh`
signs it.** An unsigned binary gets an ad-hoc signature whose designated
requirement is a bare cdhash — a hash of the bytes. TCC attaches the decision
to that, so every rebuild is a new stranger and the operator is asked again.
Measured: consent granted, helper rebuilt, asked again.

Signing with a real identity replaces that requirement with one naming the
identifier and the certificate:

    designated => identifier "com.github.alexj212.minutes-capture"
         and anchor apple generic
         and certificate leaf[subject.CN] = "Apple Development: ..."

which does not move when the code does.

**That paragraph is wrong about why, and the correction matters more than the
claim.** It was written from a correlation: before signing the operator was
asked after every rebuild, after signing he was not. Both halves are true and
the cause was something else.

The grant is keyed on the **responsible process** — whatever launched the
helper — and the helper is only ever the *accessing* process. Measured
directly, same session, same responsible process, back to back:

    properly signed helper   -> 0 prompts
    ad-hoc helper            -> 0 prompts

Renaming the identifier, which changes the designated requirement outright,
also cost nothing. **The helper's signature does not determine whether TCC
asks.** The earlier before-and-after was confounded: the session coordinator
that actually holds the grant was itself ad-hoc signed and being rebuilt
several times a day during the "before" window, and stopped being rebuilt
during the "after" one. The variable that moved was never the helper.

So sign the helper for the reasons signing is worth anything — integrity, a
name a stranger can resolve, not tripping Gatekeeper on a machine that
downloaded it — and do not expect it to buy a durable grant. What buys that is
the *launcher* being signed with a stable identity, which is not this project's
binary to control.

`build.sh` discovers the identity rather than hardcoding it — `security
find-identity`, overridable with `MINUTES_CODESIGN_IDENTITY` — and falls back
to ad-hoc with a warning, because this repo is shared with machines that have
no signing identity.

Untested, and worth saying rather than assuming: whether the helper's own
signature ever governs consent on a launch path where nothing else is
responsible for it. Every path measured here had a launcher.

Preflight still has to be able to report *waiting for a human* as its own
state. Signing makes that rare rather than constant, and the first grant on any
machine still has to be given by somebody looking at a screen.

**A tap delivers nothing at all while the render endpoint is idle**, and the
endpoint is idle at the start of every recording. So `TRACK_INFO` is emitted
when the track *starts*, not when its first packet arrives — matching
`capture.cpp`, which has always done it that way. Waiting for the first packet
loses the track entirely on a quiet machine, and the orchestrator then builds a
manifest with no such track. "There was no far end track" and "the far end was
silent" are different claims, and only one of them is true.

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

**Never hand it silence, and believe it when it doubts itself.** Whisper does not
return nothing for nothing; it invents, confidently, and the invention lands in
the notes as something somebody said. Segments below -60 dBFS are skipped, which
the manifest's per-segment peak makes free.

That floor is not enough on its own. A microphone that captured nothing measured
-55.7 dBFS — above the floor — and was transcribed as a nine-second sentence
attributed to the operator. Whisper had reported `no_speech_prob` 0.908 for it,
against 0.001 for real speech, and the pipeline was throwing that field away.
Spans the model flags are now dropped, and a track peaking below -40 dBFS is
reported as carrying no speech at all.

**A missing disclosure gets noticed; a fabricated quote gets believed.** That is
the sharper form of the trust argument this project already makes about
disclosing that a recording happened.

**Order the tests so the robust one decides.** The model's doubt is a threshold
on a continuous value, and a threshold decides borderline cases by whichever
side of the line they fall on. Measured: an echo of the far end was withheld on
a probability of 0.6001495718955994 against a 0.6 cutoff — a margin of 0.00015,
four hundredths of a percent of the way from the threshold to certainty. The
outcome was right and it was right by luck. So echo detection, which compares
two recordings of the same room, decides first; the model's doubt catches what
is left, chiefly invention over silence where there is no far end to compare
against and nothing else can catch it.

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

**And attribution requires two sources, not one microphone.** A microphone track
means *the operator* only because the other track holds everyone else. A
recording with nothing captured from the far end — the render endpoint idle
throughout, or a device that cannot capture system audio at all — carries no
speaker labels, rather than labelling the room as you. Warning over the labels
would not do: a summariser reads lines and skims headers.

### Stopping is closing stdin, and nothing else may reach the helper

A terminal Ctrl-C signals the whole foreground process group. The helper caught
in that dies non-zero, and a recording that was captured perfectly well gets
reported as failed — which happened, on a real standup, the first day this was
used in anger.

So the helper is started in its own process group. Stopping happens by closing
its stdin, which is the only route that lets it finish the packet in hand and
emit its END frames. Nothing is orphaned by this: if the orchestrator dies
without closing stdin, the pipe closes when its process exits and the helper
sees EOF anyway.

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
it**, not this program. The default destination is therefore **this node's own
core session**: delivering there keeps the transcript on the machine that made
it and puts a session with a person behind it in the loop, which is the judgment
happening rather than being skipped. A recording bound anywhere else is stored
and waits, because sending a meeting to another project is publishing rather
than filing. That is the whole reason a worker is driven by a core
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
- **R5 — macOS. Done.** CoreAudio process tap through a private aggregate,
  same framed-stdout shape, driven by the same orchestrator. Proven on the
  target Mac with two non-silent tracks whose clocks agree to 0.004 ms. The
  platform lies about its own sample rate, which is the part worth reading
  before touching it.

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
- **The skill is part of the interface.** `skills/minutes/SKILL.md` is what a
  Claude session reads before driving this tool, so a change to the commands is
  a change to it — in the same commit. `cmd/minutes/skill_test.go` pins it
  against `main()`'s dispatch rather than trusting that anybody remembered.
- **Verify against a real device before believing a design.** Everything in the
  platform table above was checked on the actual machines; the parts that were
  reasoned about instead were the parts that turned out wrong.
