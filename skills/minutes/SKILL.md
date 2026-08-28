---
name: minutes
description: Use this skill for the meeting recorder on this desktop — "record this meeting", "start recording the standup", "am I recording?", "stop the recording", "transcribe that call", "send the notes to homelab", "what meetings do I have", "clean up old recordings", "why did that track come out silent". ALSO use it when a meeting transcript or delivery brief arrives in this session's inbox asking for decisions, action items and open questions — that brief comes from this tool and this skill says how to answer it. Covers preflight/apps/start/stop/status/list/transcribe/deliver/rm/prune, the refusals that must never be forced past, and the rules about audio leaving the machine.
version: 1.0.0
---

# minutes

Records a meeting from this desktop — **both sides of it** — transcribes it, and
hands the material to a session that writes the notes. `minutes` and
`minutes-capture.exe` are on `PATH` in `~/bin`; the source is `/c/projects/minutes`.

The name is the deliverable. Capturing audio is the mechanism; a meeting that
produces no durable record of what was decided is the problem being solved.

## When to use

- Starting, stopping or checking a recording of a live meeting.
- Listing, transcribing, delivering, or cleaning up past recordings.
- Diagnosing a recording that came out silent, failed or interrupted.
- **Writing the notes** when a delivery brief lands in this session's inbox.

Do **not** use for:
- Recording on **mac** — the CoreAudio capture helper (R5) is not built yet.
  `preflight` will say so. Do not improvise with `ffmpeg`.
- Recording **inside WSL** as the audio source. `RDPSink.monitor` only carries
  audio from Linux apps inside WSL, so a Teams/Zoom/browser meeting never
  touches it. The tool captures Windows audio through interop and refuses
  otherwise — see Platform below.
- Summarising an arbitrary audio file. This records meetings; it is not a
  general transcription service.

## What is installed

```bash
minutes version          # build, and whether the capture helper is beside it
minutes version --json   # same, machine-readable
```

A release is **two files** — the orchestrator and a capture helper built for
whichever OS owns the audio hardware. `version` lists both and says which is
missing, which is the answer when recording fails and blames the helper.

## Before the meeting

```bash
minutes preflight        # exit 0 means a recording started now captures both sides
```

**Run this before a meeting that matters, not after one.** Preflight opens and
*starts* both endpoints rather than listing them, because a device that
enumerates and then refuses to start is exactly the failure worth catching
early — the alternative is a file of the right length, with a waveform, missing
half the conversation.

Say this to the user once, when they set up a call: **prefer headphones.** On
speakers the microphone also hears the far end, the same sentence is
transcribed on both tracks, and short fragments can evade echo removal and be
misattributed to them. With headphones the problem does not exist and
attribution is exact.

To capture one application instead of the whole machine:

```bash
minutes apps                                  # ♪ marks processes actually playing
minutes start --name "vendor call" --app Zoom
```

System-wide is the default and always works; `--app` avoids a video in another
window becoming dialogue in the transcript. `--app` **refuses** a name matching
nothing or matching two things — do not widen it back to system-wide to get
past that, and do not guess a process name.

## Recording

```bash
minutes start --name "sprint planning"        # returns immediately; outlives the shell
minutes start --name "vendor call" --to homelab
minutes status                                # what is running, since when
minutes list                                  # ● marks a live one
minutes stop                                  # stops the running one; transcription starts itself
```

`--name` at start is worth insisting on: it is the only moment anyone knows what
the meeting is about, and it becomes the recording id and the notes' title.

`stop` is a request, not a kill — the helper finishes the packet in hand and
writes the final manifest. **Never Ctrl-C or `kill` the capture helper** to end
a recording: a recording that was captured perfectly gets reported as failed
that way. Stopping is closing its stdin, which only `minutes stop` does.

Transcription then runs **in the background**, at roughly 7x real time — a
two-hour call took 30 minutes. `minutes list` shows `transcribing` until done.
Do not block a turn waiting on it; check back.

There is also a foreground form, which runs until the duration elapses or the
terminal is interrupted:

```bash
minutes record --duration 15m --name "standup"
```

Prefer `start` for a real meeting. `record` holds a terminal for the length of
the call, and anything that closes that terminal takes the recording with it —
it is for watching the pipeline work, not for a meeting that matters.

`--segment` sets the chunk length, default five minutes. Leave it alone unless
testing: it is what bounds the damage from an interrupted recording, and shorter
chunks mean more files rather than more safety.

Whether a recording is live, without asking the tool:

```bash
[ -f ~/.config/minutes/recording ] && echo "● REC"
```

## Delivering — this is a judgment call, and not the tool's

A recording bound for **this machine's own core session** is delivered
automatically once transcribed. Anything else is stored and waits, because
sending a meeting to another project is *publishing* rather than filing.

```bash
minutes deliver --to homelab                  # names the project whose session writes it up
minutes deliver --to homelab --notes notes.md # sends your write-up, and no transcript at all
```

The destination is what `--to` says, else what the recording was *started* with,
else `delivery.to` from the config. If none of those named one it **errors**
rather than picking — every one of those three was named by a person at some
point, and which project a meeting belongs to is not a guess this program makes.
Do not make it on the user's behalf either: ask which project.

Run `deliver` from a durable directory. It refuses from `/tmp` and friends,
because the message it sends names paths that a reader — possibly days later,
since mail waits for a session to start — would find gone.

If the shabadoo agent is unreachable the brief is written to `delivery.md` in
the recording directory and the command exits zero. That is expected, not a
failure — say so and move on. A `429` is the coordinator's loop guard; notes go
out once per meeting, so hitting it means something is sending in a loop. Do not
retry — find the loop.

## Writing the notes when a brief arrives

The brief states the ask: **decisions**, **action items with owners**, **open
questions**. Beyond that:

1. **Read the flagged stretches before quoting anything.** A brief may carry
   stretches marked *the other side was silent* — the far end said nothing for
   over two minutes. What the microphone picked up there may be the room rather
   than the meeting; on a real call this was thirteen minutes of somebody's
   family. Never paste those into notes, a file, or a message to another project.
2. **Say in the notes that the meeting was recorded.** Recording is a trust
   matter and in some places a legal one. A missing disclosure gets noticed; it
   is cheap now and awkward after the first meeting somebody did not know about.
3. **Attribution has a caveat when echoes were dropped.** If the brief reports
   suppressed microphone lines, the meeting was on speakers — treat single-line
   attributions with suspicion rather than quoting them as verbatim.
4. **File the notes where they belong in the project** and say where you put
   them. That judgment is the reason a session gets the brief at all.

## Refusals that must not be forced past

Each of these exists for a failure discovered after the meeting, when nothing
can be redone. Bring the refusal to the user; do not clear it on their behalf.

| Refusal | The flag that overrides it | Why not to reach for it |
|---|---|---|
| transcript has far-end-silent stretches | `--include-flagged` | may be the room, not the meeting — read it first, or send a write-up with `--notes` |
| `rm` on a recording whose notes never went anywhere | `--undelivered` | that is the only copy of a meeting nobody has read |
| something is already recording | `--force` | records the same meeting twice and makes a bare `stop` ambiguous |
| `--app` matched nothing, or two things | (none) | naming the wrong process records silence |
| preflight says an endpoint will not start | (none) | the recording would be missing half the conversation |

## Audio leaves this machine only when told to

The default backend is **local whisper on the GPU**, and there is deliberately
no fallback that reaches the network — the failure mode of such a fallback is a
confidential meeting uploaded on the day the GPU driver breaks.

**Never set `--backend openai`, or edit `backend` in the config, unless the user
asks for it in those words.** `minutes transcribe --model <size>` overrides the
model for one run without touching the config — `small` is the default and is
right for meetings; `tiny` is faster and hallucinates on quiet audio. Which backend ran, and whether the audio left, is
written into the manifest and the transcript, and `minutes list` marks such a
meeting with `↑`. It is a question somebody may have to answer later.

`~/.config/minutes/config.json` (absent = defaults): `transcription`
(`backend`, `model` — default `small`, `language`, `device`, `afterStop`),
`delivery` (`to`, `coreSession`, `auto`), `retention` (`keepDays`, `keepCount`,
`keepUndelivered` — all off unless set).

## Disk

**1.33 GB/hour** for both tracks, measured. Nothing prunes automatically.
`minutes list` totals the directory; `minutes rm --older-than 720h` and
`minutes prune --dry-run` are the cleanup, and `prune` does nothing unless a
retention policy is configured. Mention the total when it gets large rather than
deleting anything unprompted.

## When something is wrong

| Symptom | What it means |
|---|---|
| `Refusing to record: …` | a pre-start check found a problem and named it. Nothing was recorded, deliberately |
| state `failed` | the device went away mid-meeting. `manifest.json`'s `error` says which; everything before that point is intact |
| a track came out SILENT | usually nothing was playing, or the meeting is on an output that is not the Windows default endpoint. Check `preflight` against the device the meeting is actually on |
| `status` says `interrupted` | the supervisor died. Completed segments are intact, the manifest is valid, at most a few seconds of the in-progress segment is lost |
| `interrupted` while transcribing | audio is complete and safe; only the transcript is missing. `minutes transcribe` produces it |
| `dropped N microphone line(s) that were echoes` | the meeting was on speakers. Headphones avoid it entirely |
| `whisper failed on device cuda` | set `device` to `cpu` in the config or fix the GPU. It will not silently fall back |
| `the shabadoo agent is not reachable` | expected with no agent running. The brief is at `delivery.md`; nothing was lost |

## Platform

| Platform | State — what is **built**, not what is designed |
|---|---|
| **Windows, driven from WSL** | records. WASAPI capture + loopback, helper started over interop |
| **macOS** | records. HAL audio unit for the microphone, a CoreAudio process tap for system audio. Needs an audio-capture permission grant once. `preflight` **blocks until somebody answers the dialog**, so run it before the meeting rather than at it. The grant then persists, including across rebuilds, provided the helper was signed — `build.sh` does that automatically where a signing identity exists |
| **native Linux desktop** | **refuses.** The PulseAudio path (source + `<sink>.monitor`) is designed and not built |
| **WSL as the audio source** | **refuses on purpose** — `RDPSink.monitor` carries only audio from Linux apps inside WSL, so a Teams/Zoom/browser meeting never touches it |

It also refuses when WSL interop is disabled, because the helper cannot be
started — and says so rather than falling back to the PulseAudio device that is
sitting right there looking like it would work.

## On disk

`~/minutes` by default (`$MINUTES_ROOT`, or `--root`). Per recording:
`manifest.json`, `mic-NNN.wav` / `system-NNN.wav` (5-minute chunks),
`transcript.txt`, `transcript.json`, `recorder.log`.

Other environment: `MINUTES_MARKER`, `MINUTES_HELPER`, `MINUTES_CONFIG`,
`MINUTES_WHISPER`, `SHABADOO_SOCKET`.

Depth beyond this: `/c/projects/minutes/docs/usage.md` (full walkthrough),
`docs/gaps.md` (what it does not do yet), `CLAUDE.md` (why it is built this way).
