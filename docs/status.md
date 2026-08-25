# Where this stands

Last updated 2026-08-25, after R4.

The plan is in [CLAUDE.md](../CLAUDE.md); this is what became of it. It exists
so that the difference between *built*, *proven*, and *assumed* stays visible,
because on this project that distinction has repeatedly been the thing that
mattered.

---

## The plan, and what became of it

| Phase | Status | What it delivers |
|---|---|---|
| **R1** — Windows capture | **done** | Two tracks, framed and timestamped, from MSVC over WSL interop |
| **R2** — orchestrator | **done** | Segments, manifest, `start`/`stop`/`status`/`list`, storage on disk |
| **R3** — transcription | **done** | Per track, merged on the shared clock, local by default |
| **R4** — summary and delivery | **done, reshaped** | Delivery to a session's inbox; summarising moved out of the worker |
| **R5** — macOS | not started | CoreAudio process taps, same framed-stdout shape |

### Two deliberate departures

**Summarising is not in the worker.** The build order put it there. The machine
has no local model and no API key, and adding either meant a multi-gigabyte
download or a per-meeting cost to do a job the driving session already does. So
the worker assembles the material and states the ask; a session writes the
notes. That is the same argument that already governed *which project* the notes
belong to: it is a judgment, and a session makes it.

**Local transcription is the default, not hosted.** The build order assumed the
default would send audio to a third party and said to make that explicit. This
machine has a usable GPU, so the confidential path is also the convenient one.
Audio leaves only when a hosted backend is named in the config, and there is no
fallback that reaches the network when the local path fails — the failure mode
of such a fallback is a confidential meeting uploaded on the day the GPU driver
breaks.

---

## What is measured

Every number here came off the target machine. Nothing is estimated.

| | | |
|---|---|---|
| Track alignment | **0.37 ms** | Two streams at 48000 and 44100, independently started, cross-correlated on speaker bleed |
| Segment boundaries | **exact** | 176400 frames at 44.1 kHz, 192000 at 48 kHz, joins continuous |
| Device clock vs wall clock | **0.1 ms over 13 s** | Which is why the sample counter, not the wall clock, carries the timeline |
| Transcription | **~1.0–1.4× real time** | whisper `small`, CUDA, RTX 2080 SUPER. CPU is far slower |
| Disk | **1.33 GB/hour** | Both tracks, 16-bit PCM |
| `stop` latency | **~0.1 s** | Returns when audio is safe; transcription continues detached |
| Crash cost | **≤ 5 s** | `SIGKILL` mid-segment: completed segments intact, manifest valid, partial playable |
| Interop byte transparency | **1024/1024 bytes** | All 256 values, no CRLF translation |

---

## Proven against real hardware

- **Both tracks capture non-silent audio**, with a working preflight that
  refuses rather than recording half a meeting.
- **Speaker attribution.** A recording with ten seconds of a live human voice on
  the microphone while the system track held digital silence attributed every
  line correctly. This was the design's headline claim and the last thing about
  it taken on trust.
- **Segment rotation** across boundaries on both tracks at different rates, with
  continuous joins.
- **Crash safety**, with `SIGKILL` mid-segment.
- **Delivery**, end to end against the live coordinator and drained back out of
  the receiving inbox intact.
- **The idle-endpoint gap.** A real standup went idle mid-meeting, producing
  10.7 s of gap-fill and firing the re-anchor guard twice. That guard was
  written for a case that could not be reproduced deliberately, and had never
  fired against real hardware until it happened by itself. It worked.

## Still taken on trust

- **A long meeting.** The longest recording is 73 seconds. Segment rotation,
  timeline drift and manifest growth over 90 minutes are untested. The
  arithmetic is sound and the clocks were measured, but that is not the same as
  an hour.
- **Transcription on real meeting audio.** Verified against a synthetic voice
  with known ground truth. Crosstalk, accents and people talking over each other
  are untested.
- **Anything other than this machine.** One Windows host, through WSL, with one
  microphone and one output device.

---

## What has been learned the hard way

Each of these cost something to find and is written into the code or the docs so
it does not have to be found twice.

1. **A loopback stream delivers nothing while the render endpoint is *idle*** —
   not whenever the machine is silent. Once an application has opened it, silence
   arrives as real silent packets. Corrected after an earlier, stronger claim
   turned out to be wrong.
2. **Wall-clock jitter accumulates in one direction.** A packet behind the write
   head is appended, one ahead leaves a gap that is filled; both lengthen the
   file. Four-second segments came out 176403 frames instead of 176400 before
   the sample counter took over.
3. **Never hand a speech model silence.** Whisper invents, confidently, and the
   invention lands in the notes as something somebody said.
4. **Whisper anchors its first utterance at zero** when a file opens with
   silence — which the system track does in every recording.
5. **A capture that dies is not a capture that ended.** Mid-stream device
   failures used to exit zero and be recorded as clean stops.
6. **Ctrl-C signals the whole process group**, including the helper, which then
   died non-zero and reported a perfectly good recording as failed.
7. **Go's `flag` stops parsing at the first positional**, so `transcribe <id>
   --model small` silently used the default model.
8. **Pick an MSVC toolchain by completeness, not version.** The 2022 and 18
   installs here have a `vcvars64.bat` but not the `vcvarsall.bat` it calls.

---

## Testing

Every assertion in this project has been checked by deleting the code it tests
and confirming the test fails. That has caught four tests that passed for
reasons unrelated to the code they named — a length bound satisfied by a short
buffer, an absolute-path check whose fixture was already absolute, and two
others. It also caught a test that terminated the test runner by signalling its
own process.

The rule: **if the test setup could satisfy the assertion on its own, the test
proves nothing.**

---

## What is next

[gaps.md](gaps.md) has the full list with reasoning. In order:

1. **Short bleed fragments misattributed to you** — the design's own worst
   failure in miniature, needing a signal other than word overlap.
2. **An active recording is only visible where it was started** — thin, for a
   tool whose own documentation calls recording a legal matter.
3. **Automatic retention** — `minutes rm --older-than` exists; nothing runs it.
4. **R5, macOS.**
