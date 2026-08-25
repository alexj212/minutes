# What this does not do yet

Written down because the failures worth fearing here are the quiet ones — a
recording that looks successful and is not. Anything below that could produce
one is listed first.

Ordered by what it costs you, not by how hard it is to fix.

---

## 1. Could cost you a meeting

### ~~A device that fails mid-recording is reported as a clean stop~~ — fixed

*Was the worst one here.* A mid-stream `GetBuffer` failure — what an unplugged
headset or a changed default endpoint produces — used to log and exit zero, so a
meeting cut in half was recorded as a meeting that ended there.

All four mid-stream error paths now mark the track failed, name the `HRESULT`
where it is recognisable (`AUDCLNT_E_DEVICE_INVALIDATED` and friends), and exit
non-zero. The orchestrator carries the helper's last message into the manifest,
so a failed recording says *"the audio device was removed, disabled, or the
default endpoint changed"* rather than *"exit status 1"*. Audio captured before
the failure is kept and still listed.

### ~~Disk is not checked~~ — fixed, but still no retention

Measured: 1.33 GB/hour total — 0.69 for the microphone at 48 kHz, 0.64 for the
system track at 44.1 kHz, both stereo 16-bit. A two-hour meeting is 2.7 GB.

`start` and `record` now estimate headroom from the rates preflight reports,
refuse below fifteen minutes of room, and warn below two hours.

**Still missing:** any retention or prune. Recordings accumulate until somebody
deletes them, and at 1.33 GB/hour that adds up faster than it looks. A
`minutes prune` with a keep-N or keep-days policy is the obvious next step.

### ~~Two recordings can run at once, silently~~ — fixed

`start` and `record` now refuse when something is already recording, listing what
it is and its pid, and `--force` overrides. Two supervisors would otherwise
capture the same meeting twice at twice the CPU and disk, and make a bare
`minutes stop` ambiguous.

### An active recording is only obvious where it was started

`start` prints a banner, but then returns. After that, the only way to know this
machine is recording is to run `minutes list`. There is no tray icon, no
system-wide indicator, nothing on the desktop.

For a tool whose own documentation says recording is a trust matter and in some
places a legal one, "you can check if you think to check" is thin. It is worse
for the detached path than the foreground one, and the detached path is the one
meant for real meetings.

**Fix:** at minimum, a notification on start as well as on delivery.

---

## 2. Will annoy you

### The first transcription silently downloads a model

`small` is 244 MB, `large-v3` is 1.5 GB. The first run with a given model
fetches it with no warning and no progress, so it looks like a hang. Pull the
model before the meeting.

**Fix:** check for the model and say what is about to happen.

### There is no `minutes config`

The config file must be hand-written at `~/.config/minutes/config.json` with no
example to copy, no validation beyond "is it JSON", and no way to see what is
currently in effect. A typo in `backend` is caught, but a typo in `model` is not
found until whisper fails.

**Fix:** `minutes config` to print the effective config, and `--init` to write a
commented default.

### Every step is a separate command

`record` → `transcribe` → `deliver` are three invocations. For the common case
that is three things to remember and two to forget.

**Fix:** `minutes stop --then transcribe,deliver --to <project>`, or a
`minutes wrap` that does all three.

### Short bleed fragments evade suppression and are misattributed to you

Found by the recording that finally proved attribution. The last line of it was:

```
[00:00:26] Others: The open question is whether we keep the old end
[00:00:28] You:    all
```

`all` is the tail of *"...the old endpoint alive"* arriving at the microphone
through the air. It should have been suppressed as an echo and was not, because
the system track's own transcription was cut at *"the old end"* — so the word
never appears in the line it is an echo of, and word containment scored zero.

The result is one of the failures this design most wants to avoid, in miniature:
**somebody else's word attributed to you**, with nothing about it looking wrong.

It is not obvious how to fix without breaking something else. Dropping every
short microphone line that overlaps system audio would catch it, and would also
delete genuine interjections — the "yes", "agreed", "no, Thursday" that carry
real meaning in a meeting. That trades a rare misattribution for a routine
omission, and it is not clearly the better trade.

Better signals exist and are not yet used: bleed arrives at the microphone
quieter than direct speech, and it always coincides with audio on the system
track, which the recorder can measure rather than infer from text.

**Headphones remove the problem entirely.** They remain the right answer.

### Thresholds are fixed, and two of them can drop real speech

- **Silence floor, −60 dBFS.** Segments quieter than this are never transcribed.
  A very quiet participant on a bad connection could fall below it and vanish
  with no indication in the transcript.
- **Echo suppression, 0.6 word containment within 2 seconds.** A short genuine
  agreement that repeats the other side — "yes, Thursday" right after somebody
  says "Thursday" — matches the echo test and is dropped from your track. The
  count is reported, but not which lines.

Neither is configurable, and the dropped lines are not recoverable from the
transcript.

**Fix:** make both thresholds configurable, and record dropped lines in
`transcript.json` rather than only counting them.

---

## 3. Not built yet

These are known scope, not oversights.

- **macOS** (R5). The design is settled — CoreAudio process taps, same
  framed-stdout shape — and the headers are present on the target machine.
- **Native Linux.** The PulseAudio path (a source, plus the sink's `.monitor`)
  is described but not implemented. `preflight` refuses there rather than
  pretending.
- **Process-specific loopback.** System-wide capture takes everything, so
  notification sounds and music land on the meeting track. `ActivateAudioInterfaceAsync`
  with `AUDIOCLIENT_ACTIVATION_PARAMS` would record only the meeting
  application. Deferred for sequencing: system-wide always works and needs no
  process discovery, and mis-targeting a process records silence.
- **Summarising in the worker.** Deliberately moved to the driving session. See
  [usage.md](usage.md#delivering).

---

## 3b. Since proven

**Microphone-side attribution, with a live human voice.** Verified on a
27-second recording containing ten seconds of a real person speaking into the
microphone while the system track held exact digital silence, followed by a
synthetic voice on the system track. Every line landed on the right side:

```
[00:00:03] Others: Please talk now, until I speak again.
[00:00:07] You:   The rain in Spain falls mainly on the plane...
[00:00:17] Others: Right, let us start the stand-up.
```

Timing was correct to the second against the measured onset of speech. Three
microphone echoes of the system track were suppressed, and — as in all four
recordings checked this way — every suppressed line was a genuine echo. The one
misattribution it produced is the gap above.

This was the design's headline claim and the last thing about it taken on trust.

## 4. Claimed but not verified

Listed separately because an unverified claim and a known gap are different
things, and this project's own rule is to verify against a real device before
believing a design.

*Microphone-side attribution was the entry here for three attempts. It is now
proven — see below — and what it exposed is listed as a gap of its own.*
- **A long meeting.** The longest run is 32 seconds. Segment rotation, timeline
  drift and manifest growth over 90 minutes are untested. The drift arithmetic
  is sound and the sample counter measured 0.1 ms against wall-clock over 13
  seconds, but that is not the same as an hour.
- **An idle gap mid-recording.** Could not be reproduced on this machine —
  Windows keeps the render endpoint active well after a stream closes. The
  timeline has a guard that falls back to wall-clock if the device counter
  stalls, and the guard is unit-tested, but it has never fired against real
  hardware.
- **Transcription on real meeting audio.** Proven against a synthetic voice with
  known ground truth. Crosstalk, accents, and people talking over each other are
  untested.

---

## What to fix next

The three highest-cost items are done: mid-stream device failure is now recorded
as a failure with its reason, concurrent starts are refused, and the disk is
checked before recording.

What is left, in order:

1. **Short bleed fragments misattributed to you.** Rare, but it is the design's
   own worst failure in miniature. Needs a signal other than word overlap —
   level, or system-track energy — rather than a blunter text rule that would
   delete real interjections.
2. **Make an active recording visible outside the terminal that started it.**
3. **`minutes prune`.** Nothing deletes anything, at 1.33 GB/hour.
