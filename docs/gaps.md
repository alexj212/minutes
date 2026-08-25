# What this does not do yet

Written down because the failures worth fearing here are the quiet ones — a
recording that looks successful and is not. Anything below that could produce
one is listed first.

Ordered by what it costs you, not by how hard it is to fix.

---

## 1. Could cost you a meeting

### A device that fails mid-recording is reported as a clean stop

**This is the worst one, and it is the failure this project was built to
prevent.**

If the default endpoint changes while recording — headphones unplugged, output
switched, device removed — WASAPI returns `AUDCLNT_E_DEVICE_INVALIDATED` from
`GetBuffer`. The helper logs it, stops both tracks, and **exits zero**. The
manifest records `stopped`, exactly as if somebody had asked it to stop.

So a meeting that was cut in half at minute six looks, in `minutes list`, like a
meeting that ended at minute six. The evidence is a line in `recorder.log` that
nobody reads.

Only failures *opening* or *starting* an endpoint currently mark the recording
failed. Mid-stream failures do not.

**Fix:** set the failure flag on a mid-stream `GetBuffer`/`ReleaseBuffer` error
so the helper exits non-zero and the manifest records `failed` with the reason.
Roughly ten lines in `native/windows/capture.cpp` plus a manifest field. This
should be done before the tool is trusted with a meeting that matters.

### Disk is not checked, and it fills at 1.33 GB/hour

Measured: 0.69 GB/hour for the microphone at 48 kHz and 0.64 for the system
track at 44.1 kHz, both stereo 16-bit. A two-hour meeting is 2.7 GB.

Nothing checks free space before or during a recording, and there is no
retention or prune. A disk that fills mid-meeting produces write errors whose
handling has never been exercised.

**Fix:** a preflight free-space check against the expected rate, and a
`minutes prune` with a keep-N or keep-days policy.

### Two recordings can run at once, silently

`minutes start` twice starts two supervisors, each holding its own microphone
and loopback client. Both record the same audio to two directories at twice the
CPU and disk. It is almost always a mistake — somebody forgot to stop the last
one — and nothing says so.

It also makes a bare `minutes stop` ambiguous: it stops the most recent live
recording, which may not be the one you meant.

**Fix:** warn (or refuse without `--force`) when starting while another
recording is live.

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

## 4. Claimed but not verified

Listed separately because an unverified claim and a known gap are different
things, and this project's own rule is to verify against a real device before
believing a design.

- **Microphone-side attribution, with a live human voice.** Every proof recording
  so far has had speech only on the system track. Attribution is covered by unit
  tests, and echo suppression was checked against the raw untrimmed microphone
  transcript to confirm it removed only genuine echoes — but no recording yet
  contains a real person speaking on the mic track while somebody else speaks on
  the system track. **This is the headline claim of the whole design and it is
  the one thing not proven on real audio.**
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

## What to fix first

1. **Mid-stream device failure reported as a clean stop.** It is the only gap
   here that can lose half a meeting and tell you nothing.
2. **Prove microphone-side attribution on a real recording.** One thirty-second
   take with two people, or one person and a real call.
3. **Warn on concurrent `start`, and check free disk.** Cheap, and both prevent
   a bad afternoon.
