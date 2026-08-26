# Using minutes

Records a meeting from this desktop — both sides of it — transcribes it, and
hands the result to a session that writes the notes.

Command output shown below is real output from the target machine, except where
a block is marked *illustrative* — those show the shape of a longer meeting than
anything yet recorded. Measured numbers are called out as measured.

---

## Installing

```
$ make install          # builds both binaries and puts them in ~/bin
$ minutes preflight
```

`make install` places `minutes` and `minutes-capture.exe` together in `~/bin`,
which is on `PATH`. They must stay together: the orchestrator finds the helper
by looking beside itself first, so installing one without the other leaves a
`minutes` that refuses to record and says the helper is missing.

Override the destination with `PREFIX=/somewhere make install`.

The Windows helper runs from the WSL filesystem through interop — verified,
rather than assumed.

Without installing, both binaries are in `dist/` after `make all` and can be run
as `./dist/minutes`.

### Where recordings go

`~/minutes` by default, or `$MINUTES_ROOT`, or `--root` per command.

Deliberately not a relative path. Once `minutes` is on `PATH` it gets run from
wherever you are standing, and a relative default would scatter meetings across
whichever directories you happened to be in — and then `minutes list` would show
none of them, because it would be looking beside the current directory too.

**At a measured 1.33 GB/hour, keep an eye on that directory.** Nothing prunes it.

## Before anything: ask the machine

```
$ minutes preflight
platform: wsl
helper:   /c/projects/minutes/dist/minutes-capture.exe
  mic     ok   wasapi-capture   Microphone (2- Insta360 Link)  48000 Hz, 2 ch, 32-bit
  system  ok   wasapi-loopback  Speakers (Realtek(R) Audio)  44100 Hz, 2 ch, 32-bit

Both tracks can be captured.
```

Exit code 0 means a recording started now would capture both sides. Non-zero
means it would not, and the output says why.

**Run this before a meeting you care about**, not after. Preflight opens and
starts both endpoints rather than just listing them, because a device that
enumerates and then refuses to start is exactly the failure worth catching
early. The alternative is a file of the right length, with a waveform in it,
that plays — and is missing half the conversation.

The two tracks commonly run at *different* sample rates. That is normal and
handled; they are aligned by timestamp, not by sample count.

---

## Recording

### Where the notes will go

Name it when you start, because that is when you know what the meeting is about:

```
$ minutes start --name "vendor call" --to homelab
```

Leave `--to` off and it goes to **this machine's own session** — the `wsl` core
session here, discovered from the node directory shabadoo keeps beside its
socket. That default is the safe one, and the reason is worth understanding:

**Delivering to your own machine's session is not publishing.** The transcript
stays on the machine that made it, and a session with a person behind it reads
it and decides where the notes actually belong. Delivering to another project
hands a meeting to somebody else.

So a recording bound for the core session is **delivered automatically** once it
has been transcribed. One bound for any other project is stored and waits, and
`minutes deliver` picks the destination up so nobody retypes it:

```
$ minutes stop
...
not delivering automatically: "xinthesys" is not this machine's own session, and
sending a meeting to another project is publishing rather than filing.
Use `minutes deliver`.
```

Automatic delivery also stops if the transcript has stretches where the far end
was silent, for the same reason `minutes deliver` refuses them: those may be the
room rather than the meeting.

### The usual way: start, and walk away

```
$ minutes start --name "sprint planning"
┌──────────────────────────────────────────────┐
│  ● RECORDING — microphone and system audio   │
└──────────────────────────────────────────────┘
  id:       2026-08-25-104604-sprint-planning
  files:    recordings/2026-08-25-104604-sprint-planning
  segments: 5m0s

  stop with:  minutes stop 2026-08-25-104604-sprint-planning
```

`start` returns immediately and the recording continues without it — a meeting
is longer than a command. It survives the shell closing.

It refuses if something is already recording, since that would capture the same
meeting twice and make a bare `stop` ambiguous — pass `--force` if you really
want both. It also refuses if the disk holds less than fifteen minutes of
recording, and warns below two hours. At a measured **1.33 GB/hour** for both
tracks, a long meeting is not a small file.

```
$ minutes stop
  ○ stopped

  system     11.0s   4 segment(s)  peak -25.5 dBFS  Speakers (Realtek(R) Audio)
      [00] system-000.wav      3.0s at    0.0s
      [01] system-001.wav      3.0s at    3.0s
      [02] system-002.wav      3.0s at    6.0s
      [03] system-003.wav      2.0s at    9.0s
  mic        11.0s   4 segment(s)  peak  -2.6 dBFS  Microphone (2- Insta360 Link)
```

`stop` with no argument stops the recording that is running. `stop <id>` names
one explicitly.

Stopping is a request, not a kill: the helper finishes the packet in hand,
closes its segments and writes the final manifest. That takes a moment and is
worth it.

**Transcription then starts by itself, in the background.** `stop` returns as
soon as the audio is safe on disk — measured at about a tenth of a second — and
the supervisor carries on transcribing. `minutes list` shows the recording as
`transcribing` until it is done.

It runs in the background rather than in `stop` because it takes a while.
Measured on a real two-hour call: **30 minutes** to transcribe both tracks, or
about 7.4x real time. Blocking `stop` on that would hold the terminal open long
after the meeting ended.

Turn it off with `"afterStop": false` under `transcription` in the config, and
run `minutes transcribe` by hand instead.

### The other way: record in the foreground

```
$ minutes record --duration 15m --name "standup"
```

Same pipeline, runs until the duration elapses or Ctrl-C. Useful when you want
to watch it, and for testing.

### Managing what you have

```
$ minutes list
  ID                           STATE          LENGTH     SIZE  TRANSCRIPT   DELIVERED
  2026-08-25-140144-standup    stopped         73.5s   27.1MB  312 lines    homelab
                               standup
  2026-08-25-093012-vendor     stopped        903.0s  334.0MB  —            —
                               vendor call

  2 recording(s), 361.1MB in /home/alexj/minutes
```

`TRANSCRIPT` is `—` until `minutes transcribe` has run, and carries a `↑` if
that meeting's audio was sent off this machine. `DELIVERED` names the project
its notes went to, or says `(on disk only)` if the agent was unreachable and the
brief was written locally instead.

### Knowing it is recording without asking

While a recording runs, `~/.config/minutes/recording` holds what it is:

```json
{ "id": "2026-08-25-104604-standup", "name": "standup",
  "dir": "/home/alexj/minutes/2026-08-25-104604-standup", "pid": 3869780 }
```

It is removed when capture ends — not when transcription ends — and a marker
whose process has died is ignored and cleaned up, so the machine never claims to
be recording forever.

For a shell prompt:

```sh
[ -f ~/.config/minutes/recording ] && echo "● REC"
```

A notification also goes out on start and stop, through shabadoo's agent socket
if it is reachable. Both are best effort: neither is a reason not to record.

This exists because `minutes list` only tells you if you think to ask, and the
times that matter are the times you do not.

### Removing them:

```
$ minutes rm 2026-08-25-093012-vendor
$ minutes rm --older-than 720h            # everything over 30 days
```

There is also a policy, applied by `minutes prune`:

```json
{ "retention": { "keepDays": 90, "keepUndelivered": true } }
```

`keepDays` removes anything older, `keepCount` keeps only the newest N, and
`keepUndelivered` protects recordings whose notes never went anywhere. **It is
off unless configured**, because deleting somebody's meetings without being
asked is worse than using their disk. `minutes prune --dry-run` says what would
go. Nothing runs it for you; a cron line does that if you want one.

`rm` refuses to delete a recording whose notes were never delivered, because
that loses the only copy of a meeting nobody has read — pass `--undelivered`
when that is what you want. It refuses to touch a recording that is still
running, shows what it is about to remove and how much that frees, and asks
before doing it unless given `--force`.

At **1.33 GB/hour**, this is not housekeeping you can put off indefinitely.

### While it runs

```
$ minutes list
● 2026-08-25-104604-sprint-planning  recording        2.5s  sprint planning
  2026-08-25-093012-standup          stopped         11.0s  standup
```

The `●` marks a recording that is actually running. `status` gives the detail:

```
$ minutes status
  id:    2026-08-25-104604-sprint-planning
  state: recording (supervisor pid 3869780)
  since: 2026-08-25T10:46:04-04:00
```

`status` reconciles the manifest against reality. A directory that claims to be
recording with no supervisor alive reports as **interrupted**, not as
`recording` — trusting the file alone would report a meeting as still being
captured hours after the thing capturing it went away.

---

## Transcribing

```
$ minutes transcribe
  local-whisper:small — audio stays on this machine.

  transcribing 4 file(s) with small on cuda (audio stays on this machine)

  5 lines in 17s — 0 you, 5 everyone else
  recordings/2026-08-25-123708-two-track-proof/transcript.txt
```

Both tracks are transcribed separately and merged on the shared clock, so every
line arrives already attributed. *Illustrative* — a two-sided exchange, which no
recording here contains yet (see [gaps.md](gaps.md#4-claimed-but-not-verified)):

```
[00:04:12] You:    I think Friday is more realistic given the migration.
[00:04:19] Others: That works. Karyn, can you own the notes?
```

Your track is you; the other track is everyone else. No diarization model is
involved and there is nothing for one to get wrong.

### Audio does not leave this machine unless you say so

The default backend is local whisper, and there is deliberately **no fallback
that reaches the network** when it fails — the failure mode of such a fallback
is that a confidential meeting is uploaded on the day the GPU driver breaks.

Configure at `~/.config/minutes/config.json` (override the path with
`$MINUTES_CONFIG`):

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

| Field | Meaning |
|---|---|
| `backend` | `local-whisper` (default) or `openai`. Naming a hosted backend is the act that lets audio leave. |
| `model` | Whisper size locally (`tiny`…`large-v3`), or an API model name. |
| `language` | Skips language detection. Leave empty to detect. |
| `device` | `cuda` or `cpu`, local only. |
| `afterStop` | Transcribe automatically in the background when a recording stops. Default `true`. |

Delivery lives under `delivery`:

| Field | Meaning |
|---|---|
| `to` | Default destination. Defaults to this machine's own session. |
| `coreSession` | The destination allowed to receive a meeting automatically. |
| `auto` | Deliver to the core session once transcribed. Default `true`. |

Retention lives under `retention`: `keepDays`, `keepCount`, `keepUndelivered`.
All off by default.
| `baseUrl` | Points a hosted backend at anything OpenAI-compatible. |
| `apiKeyEnv` | *Name of* the environment variable holding the key — never the key itself, because a config file gets copied, committed and pasted into bug reports. |

Override per run with `--backend` and `--model`. An unknown backend name is an
error, not a quiet substitution to something that works.

Which backend ran, and whether the audio left, is printed before the run and
written into both the manifest and the transcript. It is a question somebody may
have to answer later, possibly to somebody else.

### Choosing a model

On this machine (RTX 2080 SUPER, 8.6 GB), `tiny` and `small` were compared on
the same 16-second clip with known ground truth:

| Model | Download | Result |
|---|---|---|
| `tiny` | 39 MB | Words mostly right, and it hallucinated a sentence nobody said |
| `small` | 244 MB | Essentially verbatim — one name misspelled. **The default** |
| `medium` / `large-v3` | 769 MB / 1.5 GB | Not tested here. Both fit the available VRAM |

The first run with a given model downloads it, which is slow and silent. Pull it
before a meeting rather than after one.

---

## Delivering

```
$ minutes deliver --to homelab
  notes requested from homelab
```

The transcript goes to that project's session inbox with a brief asking for
decisions, action items with owners, and open questions. The human gets a
notification.

**It refuses if the transcript has stretches where the far end was silent**,
because those may be the room rather than the meeting — on a real call that was
thirteen minutes of somebody's family. Read it, then either write it up:

```
$ minutes deliver --to homelab --notes notes.md
  notes delivered to homelab (no transcript sent)
```

which sends your notes and nothing else — no transcript, no path to one — or
pass `--include-flagged` if the whole thing is genuinely fine to hand over.

**`--to` is required.** Which project a meeting's notes belong to is a judgment
call, and this program does not make it — nor does it write the notes. It
assembles the material and states the ask; a session driven by a person does the
rest.

If the agent is unreachable, the brief is written to `delivery.md` in the
recording directory, the command says so, and it exits zero. A recorder that
lost a meeting because a coordinator blipped would be worse than one that never
integrated at all.

No credential is involved anywhere. The socket is
`~/.config/shabadoo/agent.sock`, mode 0600 in your own directory, so being able
to open it means already being you.

---

## A whole meeting, start to finish

```
minutes preflight                        # before it starts
minutes start --name "vendor call"       # when it starts
minutes stop                             # when it ends — transcription starts itself
minutes list                             # until it stops saying "transcribing"
minutes deliver --to homelab             # where the notes belong is still your call
```

---

## What ends up on disk

```
recordings/2026-08-25-104604-sprint-planning/
  manifest.json      what everything beside it is
  mic-000.wav        your track, 5-minute chunks
  mic-001.wav
  system-000.wav     everyone else's track
  system-001.wav
  transcript.txt     the merged conversation, readable
  transcript.json    the same, structured, with per-line times and tracks
  recorder.log       the detached supervisor's output
  recorder.pid       removed when it stops cleanly
```

Audio is plain WAV on disk and metadata is in the manifest beside it — never
blobs in a database, because a database gets copied by every backup.

Useful manifest fields:

| Field | Why you would look at it |
|---|---|
| `state` | `recording`, `stopped`, or `failed`. Cross-check with `minutes status`. |
| `tracks[].segments[].peakDBFS` | How loud each chunk was. `-999` means silent. |
| `tracks[].segments[].paddedFrames` | Gap-fill — how long nothing was playing. |
| `tracks[].segments[].complete` | `false` means the recording was interrupted mid-chunk. |
| `tracks[].reanchors` | Non-zero means the device clock and wall clock disagreed. |
| `transcript.audioLeftMachine` | Whether this meeting was sent to a third party. |
| `epochQPC100ns` | The instant both tracks call zero. |

---

## When something is wrong

**"Refusing to record: …"** — a pre-start check found a problem and named it:
an endpoint that will not capture, another recording already running, or not
enough disk. Nothing was recorded, deliberately.

**A recording came out `failed`.** The device went away mid-meeting — unplugged,
disabled, or the default endpoint changed. The manifest's `error` says which.
Everything captured before that point is intact and still listed; the recording
is simply shorter than the meeting was.

**A track came out SILENT.** `record` and `stop` exit non-zero and say so. For
the system track it usually means nothing was playing, or the meeting is on an
output that is not the Windows default endpoint. Check `minutes preflight`
against the device the meeting is actually on.

**`status` says `interrupted` while transcribing.** The supervisor died partway
through the transcript. The audio is complete and safe — only the transcript is
missing, and `minutes transcribe` will produce it.

**`status` says `interrupted`.** The supervisor died without stopping cleanly.
Every completed segment is intact, the manifest is valid, and the in-progress
segment is playable up to the last sync — at most five seconds, or a quarter of
a segment when segments are shorter.

**A stretch is marked "the other side was silent".** The far end said nothing
for over two minutes, so the call may have dropped or the other party stepped
away. **What the microphone picked up there may be the room, not the meeting** —
on a real call this caught thirteen minutes of private household conversation
sitting in the middle of a work transcript. Read those stretches before sending
a transcript anywhere.

**"dropped N microphone line(s) that were echoes"** — the meeting was on speakers,
so the microphone also heard the far end and the same words were transcribed on
both tracks. The echoes are removed from your track. **Headphones avoid this
entirely** and are worth preferring: attribution is exact with them.

**"whisper failed on device cuda"** — set `device` to `cpu` in the config, or fix
the GPU. It will not silently fall back, because a silent fallback to the
network is how confidential audio gets uploaded.

**"the shabadoo agent is not reachable"** — expected when no agent is running.
The brief is on disk at `delivery.md`; nothing was lost.

**A `429` on delivery** — the coordinator is throttling. Notes go out once per
meeting, so reaching that limit means something is sending in a loop. Do not
retry; find the loop.

---

## Command reference

| Command | What it does |
|---|---|
| `minutes preflight` | Report whether both tracks could be captured now. Non-zero if not. |
| `minutes start [--name N] [--to PROJECT] [--segment 5m] [--force]` | Begin recording and return. `--to` defaults to this machine's own session. |
| `minutes stop [ID] [--root DIR]` | Stop and report. Defaults to the running one. |
| `minutes status [ID] [--root DIR]` | Show a recording's state and segments. |
| `minutes list [--root DIR]` | List recordings with size, transcript and delivery. `●` marks a live one. |
| `minutes rm [ID...] [--older-than D] [--undelivered] [--force]` | Remove recordings. Refuses undelivered ones by default. |
| `minutes prune [--dry-run] [--force]` | Apply the retention policy. Off unless configured. |
| `minutes record [--duration D] [--name N] [--segment 5m]` | Record in the foreground. |
| `minutes transcribe [ID] [--backend B] [--model M]` | Transcribe and merge both tracks. |
| `minutes deliver [ID] --to PROJECT [--notes FILE] [--include-flagged]` | Hand it to a session; tell the human. |

| Environment | Effect |
|---|---|
| `MINUTES_ROOT` | Where recordings are kept. Default `~/minutes`. |
| `MINUTES_MARKER` | The recording marker file. Default `~/.config/minutes/recording`. |
| `MINUTES_HELPER` | Path to the Windows capture helper. Default: beside the `minutes` binary. |
| `MINUTES_CONFIG` | Path to the config file. |
| `MINUTES_WHISPER` | Path to the whisper binary. |
| `SHABADOO_SOCKET` | Path to the agent socket. |

See [gaps.md](gaps.md) for what this does not do yet, and
[status.md](status.md) for what is built and what is still assumed.
