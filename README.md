# minutes

Records a meeting from a desktop — **both sides of it** — so that afterwards
somebody can answer what was decided.

**R1 to R4 are done and in use on Windows**: capture, storage and lifecycle,
transcription, and delivery. macOS is R5 and is absent.

- **[docs/status.md](docs/status.md)** — the plan, what is built, and what is proven versus assumed.
- **[docs/usage.md](docs/usage.md)** — how to use it, command by command.
- **[docs/gaps.md](docs/gaps.md)** — what it does not do yet, worst first.
- **[docs/protocol.md](docs/protocol.md)** — the wire format between the helper and the orchestrator.
- **[CLAUDE.md](CLAUDE.md)** — what this is, and why it is a worker rather than a tray application.

Two things to know before recording a meeting that matters, both in
[gaps.md](docs/gaps.md):

- **System-wide loopback captures everything the machine plays**, so a video in
  another window becomes dialogue in the notes, attributed to a participant.
  Pause other audio.
- **Speakers put the far end on your microphone too.** Echoes are removed, but a
  short fragment can survive and be attributed to you. Headphones avoid it.

## What works

```
$ make install
$ minutes preflight
platform: wsl
helper:   /c/projects/minutes/dist/minutes-capture.exe
  mic     ok   wasapi-capture   Microphone (2- Insta360 Link)  48000 Hz, 2 ch, 32-bit
  system  ok   wasapi-loopback  Speakers (Realtek(R) Audio)  44100 Hz, 2 ch, 32-bit

Both tracks can be captured.

$ minutes start --name standup
┌──────────────────────────────────────────────┐
│  ● RECORDING — microphone and system audio   │
└──────────────────────────────────────────────┘
  id:       2026-08-25-104604-standup
  files:    /home/alexj/minutes/2026-08-25-104604-standup
  segments: 5m0s

  stop with:  minutes stop 2026-08-25-104604-standup

$ minutes stop
  ○ stopped
  ...
  transcribing in the background — `minutes list` shows when it is done.

$ minutes list
  ID                           STATE          LENGTH     SIZE  TRANSCRIPT   DELIVERED
  2026-08-25-104604-standup    stopped         73.5s   27.1MB  312 lines    —

$ minutes deliver --to homelab
```

`start` returns and the recording continues without it, because a meeting is
longer than a command. **Transcription then starts by itself** once the audio is
safe, and runs in the background — `stop` comes back in about a tenth of a
second, because transcribing a 30-minute meeting takes about 30 minutes and
blocking on that would hang the terminal for the length of the meeting.

Delivery stays manual: which project a meeting's notes belong to is a judgment
call, and this program does not make it.

`list` shows what you have, with sizes and what has been transcribed and
delivered. `rm` removes recordings, and refuses to delete one whose notes were
never delivered.

Each track becomes a series of WAV segments with a `manifest.json` beside them —
never a mix, and never a blob in a database, because a database gets copied by
every backup and an hour of stereo audio is hundreds of megabytes to copy
nightly forever.

```
~/minutes/2026-08-25-104604-standup/
  manifest.json
  mic-000.wav      system-000.wav
  mic-001.wav      system-001.wav
  transcript.txt   transcript.json
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
Transcription additionally needs `openai-whisper` on `PATH` (and a working
`tqdm` — a `tqdm` older than 4.56 cannot import on Python 3.9+, and whisper's
model download goes through it).

```
make all         # Go binary + C++ helper, left in dist/
make install     # both of them into ~/bin, which is on PATH
make test        # unit tests
make preflight   # ask the machine whether it could record right now
```

Recordings go to `~/minutes` by default — not a path relative to wherever you
ran it from. Override with `$MINUTES_ROOT` or `--root`.

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

## Transcription

```
$ minutes transcribe
  local-whisper:small — audio stays on this machine.

  transcribing 4 file(s) with small on cuda (audio stays on this machine)

  5 lines in 17s — 0 you, 5 everyone else
  recordings/2026-08-25-123708-two-track-proof/transcript.txt
```

Both tracks are transcribed separately and merged on the shared clock, so every
line arrives already attributed: the microphone track is you, the system track
is everyone else. No diarization model is involved and there is nothing for one
to get wrong. That is the return on never mixing the tracks.

### Audio does not leave this machine unless somebody says so

The default backend is local whisper. Getting a recording off this machine takes
naming a hosted backend in `~/.config/minutes/config.json`:

```json
{
  "transcription": {
    "backend": "local-whisper",
    "model": "small",
    "language": "en",
    "device": "cuda"
  }
}
```

There is no fallback that reaches the network when the local path fails, because
the failure mode of such a fallback is that a confidential meeting is uploaded
on the day the GPU driver breaks. An unknown backend name is an error rather
than a quiet substitution.

Which backend ran, and whether the audio left, is printed before the run and
written into both the manifest and the transcript — it is a question somebody
may have to answer later, possibly to somebody else.

### Three things it does that are not obvious

**Silent segments are never sent to the model.** Below −60 dBFS there is nothing
to transcribe, and a speech model asked to transcribe silence *invents*: whisper
will confidently return "Thank you." for a minute of nothing, and that lands in
the notes as something somebody said. The peak is already in the manifest, so
this costs nothing to check.

**Leading silence is trimmed, and the amount is added back.** Given a file that
opens with silence, whisper anchors its first utterance at zero rather than
where the speech is. Measured: a system track whose audio began 8.25s in had its
opening line timestamped `00:00:00`, while every later line in the same file was
correct — so the error is invisible unless you check, and it lands on the first
thing anybody said. The system track opens this way in *every* recording,
because the render endpoint is idle until something plays.

**Echo of the far end is removed from the microphone track.** With the meeting on
speakers rather than headphones, the microphone also hears the far end, so the
same sentence is transcribed on both tracks and arrives attributed to both
people. A transcript that omits a line is incomplete; one that says you said what
somebody else said is wrong, and nothing about it looks wrong. The count of
dropped lines is reported rather than swallowed — a large number means the
meeting was on speakers.

Headphones avoid the problem entirely, and are worth preferring.

## Delivery

```
$ minutes deliver --to homelab
  notes requested from homelab
```

The transcript goes to a session's inbox through shabadoo's local agent socket,
along with a brief saying what is wanted: decisions, action items with owners,
and open questions. The human gets a notification.

**The worker does not summarise, and does not decide where the notes go.** What
mattered in a meeting and which project it belongs to are both judgments, and a
session driven by a person is where those are made. `deliver` refuses without a
`--to` rather than guessing.

> This is a departure from the build order in `CLAUDE.md`, which put
> `summarise` inside the worker. It was chosen deliberately: the machine has no
> local model and no API key, and adding either would have meant a multi-gigabyte
> download or per-meeting cost to do a job the driving session can already do.

### No credential, and nothing to rotate

The socket is `~/.config/shabadoo/agent.sock`, mode 0600 in the operator's own
directory, so being able to open it means already being this user. This is the
property that decided the whole architecture: it is why the orchestrator stayed
a Linux process instead of moving to the Windows side, where it would have
needed a device token.

### It degrades rather than failing

An unreachable agent is not an error. The brief is written to `delivery.md` in
the recording directory, and the command says so and exits zero — a recorder
that lost a meeting because a coordinator blipped would be worse than one that
never integrated at all.

A refusal is treated differently from an outage, because they mean different
things: a `429` means the coordinator is throttling the sender, and since notes
go out once per meeting, reaching that limit means something is sending in a
loop and says so.

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

## Where this stands

Built and used on Windows through WSL. The numbers below were measured on the
target machine, not estimated:

| | |
|---|---|
| Track alignment | **0.37 ms**, across two streams at different sample rates |
| Segment boundaries | exact — 176400 frames at 44.1 kHz, 192000 at 48 kHz |
| Transcription | about **real time** on an RTX 2080 SUPER with whisper `small` |
| Disk | **1.33 GB/hour** for both tracks |
| Crash cost | one sync interval, ≤5 s — verified with `SIGKILL` |

See [docs/status.md](docs/status.md) for the full picture, including what is
proven against real hardware and what is still taken on trust.

## Next

**R5** — macOS, CoreAudio process taps, the same framed-stdout shape.
