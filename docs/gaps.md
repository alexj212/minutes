# What this does not do yet

Written down because the failures worth fearing here are the quiet ones — a
recording that looks successful and is not. Anything below that could produce
one is listed first.

Ordered by what it costs you, not by how hard it is to fix. For what *is* built
and proven, see [status.md](status.md).

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

`minutes prune` applies a policy from the config — `keepDays`, `keepCount`,
`keepUndelivered` — and `minutes rm` removes things by hand.

**Off unless configured, deliberately.** Deleting somebody's meetings without
being asked is worse than using their disk. **And nothing runs it for you**: a
cron line does that if you want one.

### ~~Two recordings can run at once, silently~~ — fixed

`start` and `record` now refuse when something is already recording, listing what
it is and its pid, and `--force` overrides. Two supervisors would otherwise
capture the same meeting twice at twice the CPU and disk, and make a bare
`minutes stop` ambiguous.

### ~~The recorder keeps running when the call does not~~ — flagged, and delivery now refuses

Marked now, but not solved. When the far end drops — a reboot, a dropped call,
somebody stepping away — the recorder carries on and everything the microphone
hears lands in the transcript attributed to you, indistinguishable from
something you said in the meeting.

On the first real two-hour call this captured **thirteen minutes of private
household conversation, including a child**, sitting in the middle of a work
transcript. It was caught only because a human read it before it was sent
anywhere.

Stretches where the far end has been silent for over two minutes are now flagged
in the readable transcript and in the JSON. Validated against that recording: the
two-minute threshold found exactly the two private windows and nothing else
across 118 minutes.

**Not solved**, because flagging is not preventing:

- Nothing stops the recording when a call drops, and nothing can reliably tell
  "the call dropped" from "somebody is presenting for ten minutes".
- Marked stretches are still in the transcript. Deleting them automatically
  would lose you thinking aloud, which is worth keeping.
- Background family audio *during* the meeting proper is not caught by this at
  all — the far end is talking, so nothing is flagged.

The honest position: **read a transcript before sending it anywhere.**

### ~~`deliver` can only send the transcript~~ — fixed

`minutes deliver` sends the brief with the transcript inlined or its path
attached. For a meeting containing anything private that is exactly wrong, and
the only way to hand over notes without the raw material is to write them by
hand and send them out of band — which is what had to be done for the meeting
above.

`minutes deliver --notes FILE` sends notes and nothing else — no transcript, no
path to one. And plain `deliver` now **refuses** a transcript carrying
far-end-silent stretches, naming them, rather than sending it and hoping.

### ~~A speech model given silence invents, and the invention is attributed~~ — fixed

Found by the `wsl` core session reading a recording delivered to it, not by any
test here. A microphone that captured nothing — peak −55.7 dBFS, about 52 dB
below the other track — was transcribed as `"Department of Education."`,
attributed to the operator, spanning nine of the recording's ten seconds. One
long span over inaudible audio is the textbook hallucination signature.

Whisper had said so. It reports `no_speech_prob` for every span and gave that
one **0.908**; real speech on the same machine reports **0.001**. The field was
parsed out and thrown away. Spans flagged above 0.6 — whisper's own default,
sitting in the middle of that gap — are now dropped and counted.

A track peaking below −40 dBFS is also reported as carrying no speech, because
−55.7 is not digital silence and so sailed past the −60 floor meant to catch
exactly this.

The framing came from the reviewing session and is worth keeping: **a missing
disclosure gets noticed, a fabricated quote gets believed.** Putting words in
somebody's mouth is a worse trust failure than the ones this project already
takes seriously, and it carried no uncertainty marker at all.

### ~~A threshold decided cases the echo detector was built for~~ — reordered

The model's doubt used to be applied before echo detection. That put a threshold
on a continuous value in charge of borderline cases: measured on a real
recording, an echo was withheld on a no-speech probability of
**0.6001495718955994** against a **0.6** cutoff. A margin of 0.00015. The
outcome was right and it was right by luck — a slightly different room or
microphone gain flips it and the far end is published as the operator.

Echo detection now decides first, because it compares two recordings of the same
room rather than thresholding one number. The model's doubt catches what
remains: chiefly invention over silence, where there is no far end to compare
against and nothing else can catch it.

Found by a reviewing session that measured the margin rather than accepting the
outcome. The number that made it visible only existed because the same session
had earlier asked why a probability printed as exactly `0.600`.

### ~~A one-source recording labelled everything as the operator~~ — fixed

Found by `shabadu-ios` reasoning from the design, without reading the code, while
asking whether a phone recording could ever be a first-class input. It could not
capture system audio at all, so it would be one source — and every guard here
assumed two.

Checked, and the hole already existed on the desktop. With no far-end track the
attribution gate returned without complaint and every line was labelled `You`.
Reachable whenever the render endpoint stays idle for a whole recording, which
is a meeting where the far end never made a sound. Observed on real hardware:
`track system ended after 0 audio frames`.

A microphone track means *the operator* only because the other track holds
everyone else. Remove the other track and it holds whoever was audible in the
room. Such a recording now carries **no speaker labels at all** — not a warning
header over labels that stay, because a summariser reads the lines and can skim
the header; it cannot skim a label that is not there.

### ~~"Not you" was read as "the far end" in four separate places~~ — fixed

Closed, and recorded because the shape recurred four times before anyone
counted. Each site independently wrote `if speaker == You { you++ } else
{ others++ }`, and that else-branch is the assertion *anything that is not you
is the other party*.

On the first real macOS recording — one source, so nothing could be attributed —
it produced `0 you, 3 everyone else` in the console and `0 yours, 3 everyone
else's` in the brief, both directly contradicting a disclosure saying the
recording could not say who spoke. `speakerFor` had the same reflex for
unrecognised track names, and `unattribute` rewrote the readable lines while
leaving the withheld ones credited.

Counting now happens once, in `transcript.Count`. The lesson is not that four
authors were careless: it is that "who spoke" is a question with three answers
and an `if/else` only has room for two, so every site that reached for one was
structurally unable to say *unattributed* no matter how carefully it was
written.

The brief was the worst of the four and the last found. It is prose sent to the
session that writes the notes, so a false split there has no field beside it a
reader could check against — unlike the JSON, which at least carries
`unattributed: true`.

### ~~A track that captured nothing was treated as a track that was silent~~ — fixed

The worst defect found in this project, and it was found in a real recording
that had already been delivered.

`hasFarEndTrack` was careful from the beginning — *"a track declared and never
written to is the same thing as no track"* — and it was only ever asked about
the **far end**. Nothing asked it about the microphone, because the microphone
is the track you assume is there.

On 2026-08-27 it was not there. A 44-minute standup with contractors captured
8 segments of system audio and **zero microphone frames**. Every one of the 436
lines was labelled `Others` — correctly, since they all came from the system
track — and the transcript was delivered to another project reading as a
complete record of a meeting in which the operator never spoke.

The helper had already said so. `recorder.log` line 5:

    track mic ended after 0 audio frames

Nothing consumed it.

Fixed in two halves, because the two matter at different times. **During the
meeting**, a declared track that has delivered nothing for 60 seconds raises a
desktop notification naming it — the only moment anybody can still fix it. That
threshold is not a diagnosis: a loopback stream delivers nothing while the
render endpoint is idle, and it is idle at the start of every recording, so the
wording reports what was measured rather than concluding from it.

**Afterwards**, the check is asked of both tracks, and a lost microphone gets
its own banner above the withheld notice and its own line in the brief — because unlike every other disclosure here, **the labels are not
wrong**. What is wrong is that half the conversation is missing, and correct
labels on half a meeting look exactly like a complete one.

### ~~The `degraded` string told a stranger something untrue~~ — corrected

The reason an ad-hoc helper was called degraded was that macOS would ask for
recording permission on the installing machine and after every rebuild.
**Measured false**: a properly signed helper and an ad-hoc one both cost zero
prompts, same session, same responsible process, back to back. TCC keys the
grant on the responsible process — the launcher — and the helper is only ever
the accessing process.

The place it was wrong is what makes it worth a section. That string is carried
by shabadoo's distribution and shown to whoever installs the set, which is
somebody with no way to check it and no reason to doubt it. A confident sentence
in a place nobody can verify is the same failure as a fabricated transcript
line, and this project spent a week building disclosure machinery on exactly
that argument.

It now says what signing actually buys: an identity a recipient can resolve,
integrity that is checkable because it is signed, and not being stopped by
Gatekeeper on a Mac that downloaded the file. A durable consent grant belongs to
the launcher, which is not this project's binary.

### ~~A denied microphone passed preflight and recorded silence~~ — fixed

The worst instance of this project's recurring shape, because it arrived
*through the gate built to prevent it*.

On macOS a denied microphone **opens**. The audio unit starts, the stream format
reads back, every call returns `noErr`, and the samples are all zero. So
`minutes preflight` reported `mic ok` on a machine where the system log said
`access to kTCCServiceMicrophone denied`, the recording ran, and the operator's
side was empty. Found by `minutes-mac` with the operator sitting at the machine — the
one window a human was there, spent on this instead of the attribution
measurement it was booked for.

Preflight's stated job was catching "a device that enumerates but refuses to
start". This one enumerates, starts, and delivers nothing.

**The discriminator is "is the signal constant", never "is it quiet".** A real
microphone in a silent room gives a mean far below its max because a room is not
constant — preamp noise, thermal noise, room tone. A loudness threshold would
refuse a genuinely quiet room and put us back to guessing; constant is
categorical.

Two refinements from other sessions, both better than the first version:

- The `mac` core session found the signature **in the WAV alone** — mean equals
  max — where the original needed `tccd` output on the machine while it
  happened. It survives the recording and can be checked afterwards on another
  host, so it is stored per segment in the manifest as `constant`.
- Constant rather than *zero*, which also catches a device pinned at a non-zero
  DC offset, a dead cable, and an interface that vanished. A zero-check passes
  all three. Mutation-tested: replacing the constant test with a zero-peak test
  fails on the pinned case.

`mac` found this by reading the brief it was auto-delivered — the first
end-to-end exercise of the delivery path on darwin returned a defect report
rather than notes, which is the delivery path working.

**Re-measured on `c759b43`, and the check earned its keep.** `minutes-mac` ran
the shipped probe against the still-denied microphone and then refused to take
its word for it — running the helper directly for two seconds and counting the
samples rather than trusting the verdict:

    helper exit code        0          <- it reports success
    frames                  TRACK_INFO 1, AUDIO 188, END 1
    samples                 96256
    distinct sample values  1
    min 0, max 0

96,256 samples and one distinct value. Not a quiet room: a quiet room has a
noise floor. The device is not broken either — OS input volume 36, built-in
microphone, 48000 Hz, unmuted. It is present, working, and told no.

**The finding underneath it: a denial is remembered exactly the way a grant
is.** No dialog appeared. macOS does not re-ask a question it has already been
answered, so *"nothing prompted"* is **not** evidence of a grant — and the
`waiting` outcome preflight was carefully given cannot separate the two cases.
Waiting-for-a-human and already-told-no are identical from inside the helper:
both are silent, both return `noErr`, neither shows a dialog. The only thing
that tells them apart is the samples, which is why the constant-signal probe is
not a redundant second opinion on the timeout — it is the sole discriminator for
a case the timeout is structurally blind to.

**And the two TCC services are genuinely independent.** Measured on one machine
at one moment: the system tap granted and delivering 44100 Hz while the
microphone was denied. `kTCCServiceAudioCapture` and `kTCCServiceMicrophone` are
separate decisions and granting one does not grant the other, so a recording can
have a perfectly good far end and no operator at all — which is exactly the
half-meeting this project has now shipped twice.

### ~~The check for a denied microphone could not fire~~ — fixed, and it had shipped

The constant-signal check above was published in a state where it **could never
refuse anything**, and preflight kept passing, and that looked identical to the
check working.

The helper stops on stdin EOF *or* its duration, whichever comes first — the
contract that lets a recording end cleanly. `os/exec` gives a command with no
`Stdin` an already-closed `/dev/null`, so the helper saw EOF immediately, wrote
`TRACK_INFO`, and exited before capturing a sample. Measured on the real Windows
helper: **117 bytes with no stdin, 182101 bytes with it held open.**

The reason it passed rather than failed is the interesting half. "No packets"
was being read as "nothing to report", on the stated grounds that an empty
stream is a different fault caught elsewhere. That reasoning is sound for a
recording and wrong for a preflight, and it turned a broken probe into a silent
success.

Two things follow, and the second is the general one:

- **A microphone has three states, not two**, and this is the fifth place that
  distinction has been got wrong here. (A fourth state was briefly believed —
  a denied macOS microphone delivering *nothing* rather than zeros. That was
  two people independently tripping the same stdin contract from different
  directions and each reading the symptom as a platform behaviour. It does not
  exist. The `micNoPackets` verdict is still right, on the strength of the
  44-minute Windows standup, and is simply unwitnessed on darwin — which is not
  the same as impossible.) Delivered varying audio, delivered a
  constant signal, delivered nothing at all. The third is not a milder second —
  a track that declares its format and then never writes to it is arguably the
  *stronger* signal, since a constant signal at least requires arguing that
  rooms are not constant. The sentence that settles it was already in this
  codebase, about the far end: *a track declared and never written to is the
  same thing as no track.* It had only ever been asked about the far end.
- **A check that cannot fire is indistinguishable from a check that passes.**
  The test suite could not have caught this: every fake helper in it emits its
  frames and exits, so none exercised the stdin contract at all. There is now
  one that emits audio *only while its stdin is open*, so the probe cannot pass
  by accident.

Found by `minutes-mac` pointing `90864db` at the one machine with a provably
denied microphone and reporting that it still passed.

### ~~`system ok` meant "the device opened", never "audio comes out of it"~~ — fixed

The sixth instance of the shape, and the first one found in the *fix* for an
earlier instance. The microphone got a live probe because opening a device
proves nothing about it; the system track was left with the word `ok` doing
both jobs.

`minutes-mac` found it by accident and reported it against themselves. They had
told four sessions "the system tap is delivering 44100 Hz right now" on the
strength of `system ok` — the exact move they had just caught me making about
the microphone — then went and established it:

    with a 440 Hz tone playing:   AUDIO 183, samples 187392, distinct 49753, peak -43.0 dBFS
    with nothing playing:         AUDIO 0

**Both exit zero. Both produce `system ok`.** The only difference between the
runs is whether something happened to be playing, which is not a property of
the capture path at all. So:

    tap working, endpoint idle  ->  0 audio frames  ->  "system ok"
    tap dead, never started     ->  0 audio frames  ->  "system ok"

**The fix is not to copy the microphone's probe across, and this is the useful
part.** Silence on the system track is legitimate and common — the render
endpoint is idle before every meeting, which this repo documents — so a
constant-signal refusal there would refuse valid recordings. Same measurement,
opposite policy, and the costs run in opposite directions: a silent microphone
is a meeting recorded without the operator in it, discovered afterwards; a
silent system track is Tuesday.

So the system track is probed and the result is **reported and never
enforced**, while the word carries what was established:

    mic     ok  ...  — carrying signal
    system  ok  ...  — opened; no audio observed

There is a test whose only job is to stop somebody wiring the system probe to
the microphone's refusal, because that is the obvious next change to that file.

**And the honest label needed one more thing than honesty.** `opened; no audio
observed` is genuinely ambiguous — it is what a working idle endpoint looks
like *and* what a dead one looks like, and preflight cannot separate them. A
label that stops there hands the operator an unresolved state and no way to
resolve it. It now says what settles it: play something for a second and run it
again. **The state is resolvable in two seconds, and saying so is what makes
the honest answer useful rather than merely correct.**

The type caught its own defect on the way in. `Signal` was first written as a
string, which makes its zero value `""` — so a track nobody probed would
serialise an empty field, indistinguishable from a measured nothing, which is
the precise ambiguity the type exists to remove. The test asserting the zero
value *is* unknown failed on the first version and the type became an integer.

Not a delivery gap, and that was checked rather than assumed: a recording whose
system track wrote `segments: null` is already disclosed at delivery — the
transcript header and the brief both say the recording captured one source and
cannot say who spoke. What was missing was the chance to know *before* the
meeting.

**And the fix shipped with the same defect it was fixing.** Both microphone
refusals `return` as soon as they decide, and the system probe sat after them —
so `carrying signal` was **unreachable whenever the microphone was broken**,
which is the one situation where an operator most wants to know whether
anything still works. Found by minutes-mac within an hour, and only because
their microphone is denied: from a machine with a working one the branch looks
covered, and every test I wrote exercised it that way.

Both probes now run before anything decides. It costs about two seconds on a
path that is already refusing, which is the right trade — *a refusal that names
what does still work is a different message from one that only says no*, and
the refusal text now carries the far end's state.

The generalisation, since this is twice in two days: **a branch is only as
tested as the machine the tests run on.** Mine could not reach the interesting
state, and no amount of care in writing them would have changed that. It took a
second machine in a different condition.

And it is symmetric, which is the part that makes it an argument rather than an
apology. Neither node can write a test for a state it cannot enter: this one
cannot reach a denied microphone, that one cannot reach a working one. **The
pair is the test rig.** minutes-mac put the reciprocity plainly — they could not
demonstrate `carrying signal` until the mic-failing path stopped short-circuiting,
so *the fix is what made their machine able to verify the fix*. Neither of us
could have closed it alone, and that is a reason for cross-node briefing to be
routine rather than exceptional.

### ~~The advice for a dead microphone named a remedy that does not always exist~~ — fixed

Preflight told every operator with a constant microphone to open System
Settings > Privacy & Security > Microphone and enable it. On at least one
machine that is a dead end, and the text was confident.

**macOS can refuse to *ask*.** When the responsible process is built with the
hardened runtime and without `com.apple.security.device.audio-input`, no dialog
is ever raised — so there is nothing to enable, and toggling the entry that is
there changes nothing. minutes-mac went to TCC's own log rather than reasoning
about it:

    Prompting policy for hardened runtime; service: kTCCServiceMicrophone
      requires entitlement com.apple.security.device.audio-input
      but it is missing for responsible={... shabadoo}
    Policy disallows prompt ... access to kTCCServiceMicrophone denied

They ruled out our side first, which is what makes it usable: the same helper
binary signed three ways — hardened with the entitlement, unhardened without,
and hardened without — delivered 96256 samples, 1 distinct value, all zeros
every time. **The accessing process's signature is not the lever.**

**It was first reported as a regression in that day's build, and it is not** —
minutes-mac retracted that themselves. The hardened runtime arrived with
signing itself, twenty-five releases earlier; the correlation was between a
file's mtime and a symptom.

The real mechanism is better than either guess, and it is in the log's own
wording: *failed to match **existing** code requirement.* **A grant was already
there.** The launcher's first real signature invalidated the requirement that
grant had been recorded against, so re-consent became necessary — which is
exactly the moment the missing entitlement forbids a prompt. Signing was adopted
on both sides of this fleet to make TCC grants durable. Here it is what turned a
recoverable grant into an unaskable one, and every API call kept returning
success throughout.

**That is the shape worth keeping: the fix for the first problem created the
conditions for the second.** Neither change was wrong on its own terms, and
nothing in either project could see the interaction — it is visible only in a
log belonging to neither.

Nothing in the capture path separates an answered "no" from a forbidden prompt:
both arrive as a constant signal. So the advice enumerates what it cannot
distinguish and hands over the `log show` predicate that does distinguish them,
rather than picking one. **A wrong remedy stated confidently costs more than an
honest list, because it gets followed.**


### ~~Two zero values rendered as blanks where a name was defined~~ — fixed

The rule this project sent to the fleet, asked of this project. shabadoo took
*"make the zero value the unknown one"* into the payload, audited their own repo
against it first, and found a live instance in code they had shipped an hour
earlier. So the same question was asked here, and it found two.

**A line with no speaker printed a bare colon.** `Line.Speaker` is a string, so
its zero value is `""` — not one of the three labels — and the renderer wrote
`l.Speaker + ":"`, producing an unlabelled line indistinguishable from a
formatting fault, in a document whose entire claim is who said what.

Reachable rather than theoretical, and that was traced rather than assumed:
`transcript.Load` reads `transcript.json` back from disk and `minutes deliver`
renders it, so a file written by any build that left the field unset — or one
somebody edited — arrives at that line.

**The sharper half is that `Count` already got it right and the renderer did
not.** `Count` defaulted an unrecognised speaker to `Unattributed`; the
renderer printed nothing. So a brief saying *"2 unattributed"* could sit over a
transcript body showing two blank labels — **a disclosure disagreeing with the
document it describes**, which a reader resolves as a glitch rather than as a
claim. Both now go through one `SpeakerLabel`, which is the same fix `Count`
itself was: decide once, in one place, so two call sites cannot drift.

The column was widened while there. `%-6s` against a 13-character
`Unattributed:` had been pushing the text out of alignment on every recording
that could not attribute its lines — which is to say, on exactly the recordings
whose formatting most needed to look deliberate.

**And a manifest with no state listed as an empty cell.** `StateLabel` returned
`string(s.State)`, so a manifest carrying no state at all — an older file, a
partial write — rendered as a blank column in `minutes list`, reading as a
column that failed to draw rather than as a recording whose state is unknown.
It says `unknown` now.

Neither was found by review, and neither is a new defect: both predate the rule
and were invisible until somebody asked the type what it says when nobody has
set it. **The default-construction path is a case, and it is the one no test
constructs** — every test builds the struct deliberately and sets the field.

### ~~The comment named the fatal state and the else branch entered it~~ — fixed, and unbuilt here

`audio.swift` says, in the paragraph immediately above the code:

    // A tap-only aggregate creates and starts without error and then
    // delivers nothing, which is the enumerate-versus-start failure this
    // project exists to refuse.

and then twenty lines later built exactly that, silently:

    let outUID = deviceUID(defaultDevice(input: false)) ?? ""
    ...
    } else {
        aggDesc[kAudioAggregateDeviceSubDeviceListKey] = []
    }

The `?? ""` collapsed **three outcomes into one**: the default output device
could not be found, the device was found and its UID could not be read, and the
UID read fine. The first two took the else branch into the aggregate the
sentence above calls unacceptable — created, started, exit 0, `TRACK_INFO`
emitted, sample rate correct, and **no audio packets ever**. Indistinguishable
from a working track until somebody counts samples, and sticky, because
whatever broke the lookup persists.

Found by minutes-mac reading the file while hunting an unrelated fault. What
makes the report usable is that they then established it was **not** that fault
rather than letting a plausible mechanism stand: a small Swift probe doing the
identical two lookups against the live machine returned `OSStatus 0, id 78,
outUID "BuiltInSpeakerDevice"`. The correct branch is being taken. *"I wanted
it to be the answer. It is not, and saying so is cheaper than you finding out
after changing it."*

It now refuses, with the two causes kept apart and each reaching the report, so
preflight can say which. **Refusing costs nothing real** — a Mac with no usable
default output device has no system audio to capture — while opening anyway
costs a meeting, discovered afterwards.

**And this repo cannot verify its own fix.** There is no Swift toolchain on the
WSL node: `make helper` there builds the Windows `.exe`, `make test` is Go only,
and every guard stayed green over an edit to a file none of them can read. So
the same shape holds at the level of the repository — *a check that cannot fire
looks exactly like a check that passes* — and the only thing standing behind a
darwin change is the other node building it. That is the pair-as-test-rig
arrangement working, and it is not automation.

### ~~A sentinel was printed in the units of a measurement~~ — fixed

`-999` means "there was no signal to measure" and was printed as
`peak -999.0 dBFS`. That reads as *very quiet*. A reader files the microphone
under "turned down" and carries on — which is exactly what a brief describing a
null microphone did when it reached a session.

Same shape as `waiting` existing rather than collapsing into `error`, and as
`Reanchors` always being written so absent and zero stay distinguishable. **A
value meaning "not measured" must not wear the clothes of a measurement.** Found
by the `mac` core session.

### There is no honest track name for a device that records a room

Raised by `minutes-mac` while scoping what an iOS recorder would need. A phone
cannot capture system audio, so it produces one source — and `manifest.Track`
has no name for that. Calling it `mic` asserts the operator, which is the
failure everyone has already agreed on.

Half of it is closed: an unrecognised track name is now attributed to nobody
rather than to the far end. `speakerFor` used to return `Others` for anything
that was not the microphone, so a track called `room` would have been published
as the other party on the basis of a name not matching.

**Still open:** what such a track should be called, and whether the manifest
should carry a track *kind* separate from its name. Not decided, because nothing
emits one yet and a name invented ahead of its first use is a guess with a
version number. When a device does record a room, the answer has to arrive with
it.

### ~~An echo escaped because whisper cut the two tracks differently~~ — fixed

`gaps.md` used to say a *short fragment* could evade the text comparison. That
was wrong about the mechanism, and `minutes-mac` proved it by accident: their
far-end audio was one script looped ten times, so the same sentence was
presented ten times under identical conditions. **Nine were caught and one
escaped** — a complete, confident 84-character sentence, which is the most
credible thing in a transcript to attribute to the wrong person.

Whisper's segmentation is not stable across two recordings of the same audio.
The far-end line absorbed the tail of the previous sentence and cut early, so
containment came out 9/17 = 0.529 against a 0.6 cutoff. **Length was
incidental.** Any test on whole-line similarity is really measuring where the
model chose to cut.

A contiguous shared run is not. Five words is measured rather than chosen:
across 700+ microphone lines of real meetings on speakers, lines with no
textual relationship to the far end reached a five-word shared run **zero
times**, while confirmed echoes clustered at four to eight and beyond.

Two other fixes were measured and rejected, and both are worth recording
because both sounded right:

- **The acoustic level test does not separate these at all.** On both
  recordings the confirmed echoes were as loud as the operator — 177 and 313 of
  them — because the speakers were loud enough that an echo arrives at nearly
  full level. The level pass only ever fires on genuinely faint fragments.
- **A pure time-overlap test would have deleted 182 lines** of the operator
  genuinely talking over the far end, in a single meeting. Reasoning from one
  escaped case suggested it; the distribution refused it.

### Speaker labels are only as good as the far-end capture

Found by the `wsl` session reviewing two app-scoped recordings. When the far-end
track does not hold the far end — the wrong application was targeted, or it was
muted — the microphone's acoustic pickup of the far end has nothing to be
compared against, so echo suppression has no reference and the room is published
**as the operator**. Real words, wrong mouth, and the operator's is the worst
one to get wrong.

A capture failure and an attribution failure turn out to be the same event.

The transcript now measures the far-end track's peak-to-average ratio, which
separates "captured the meeting quietly" from "captured something steady at a
healthy level" in a way that level alone cannot. Speech runs 15 to 20 dB; a pure
tone is 3.01; steady noise about 10. Below 12 the transcript is headed:

```
# ⚠ SPEAKER LABELS BELOW ARE NOT RELIABLE
```

with the measurement, the reference range, and how many lines carry the doubtful
label. The lines are kept: the recorder knows the labels are unreliable, not
what the truth is, and deleting real speech would be the worse trade.

**Not solved:** this catches a far-end track that captured *nothing like speech*.
It cannot catch one that captured the wrong speech.

### A delivered message points at files that can disappear

The brief inlines a short transcript, but the audio and manifest paths it names
are only paths. Nothing preserves them, and a session reading the mail later may
find nothing there.

Demonstrated the hard way: a test recording was delivered to the `wsl` session
and deleted two minutes afterwards — by me, as test cleanup, not by the `/tmp`
sweep the reviewer reasonably assumed. The receiving session had the transcript
because it is inlined, which is the right answer for what a project session
actually needs, but could not re-measure the audio.

Delivery from a volatile directory is now refused — on **both** paths. The guard
was on `minutes deliver` and not on automatic delivery, so every automatic
delivery from a temporary directory went out unchecked. That cost the reviewing
session its verification on two separate runs, including the one whose entire
purpose was proving a fix, and it is the difference between four measured
write-ups and one taken on trust.

**Still not solved:** nothing warns that a delivered recording is about to be
removed, and `minutes rm` does not refuse one whose files a delivered message
names. The inlined transcript is what has been carrying the feature: the paths
were dead on arrival every time, and the message body was not.

### ~~Loopback captures everything, and it lands in the transcript~~ — fixed by `--app`

Not new, but auto-transcription makes it visible on every meeting rather than
only when you ask. System-wide loopback takes whatever the machine is playing,
so a video in another window becomes dialogue in the notes, attributed to
`Others` and indistinguishable from a participant.

Observed: a nine-second test recording produced four lines, two of which came
from unrelated audio playing at the time. There is nothing in the transcript to
say which is which.

`minutes start --app Zoom` now captures one process and its children instead of
the render endpoint, so nothing else the machine plays reaches the recording.
Proven by playing a 440 Hz tone from one process while capturing another: the
tone sits 20.7 dB below the target's speech, which is spectral leakage rather
than capture.

**Still the default is system-wide**, because it always works and needs no
process discovery, and because naming the wrong process records silence. `--app`
refuses a name that matches nothing or matches two things, rather than quietly
widening.

### ~~An active recording is only obvious where it was started~~ — fixed, and now on screen

`start` prints a banner, but then returns. After that, the only way to know this
machine is recording is to run `minutes list`. There is no tray icon, no
system-wide indicator, nothing on the desktop.

For a tool whose own documentation says recording is a trust matter and in some
places a legal one, "you can check if you think to check" is thin. It is worse
for the detached path than the foreground one, and the detached path is the one
meant for real meetings.

A marker file at `~/.config/minutes/recording` holds the current recording for
anything to read — a shell prompt, a status bar, a person with `cat` — and a
notification goes out on start and stop. A marker whose process has died is
ignored and removed, so the machine never claims to record forever.

**Still thin:** there is no desktop indicator on Windows, where the meeting
actually happens.

---

## 2. Will annoy you

### ~~The first transcription silently downloads a model~~ — fixed

`small` is 244 MB, `large-v3` is 1.5 GB. The first run with a given model
fetches it with no warning and no progress, so it looks like a hang. Pull the
model before the meeting.

**Fixed.** The first run with an uncached model says what is being fetched and
roughly how big it is. Silent otherwise, because a warning on every run is one
nobody reads.

### There is no `minutes config`

The config file must be hand-written at `~/.config/minutes/config.json` with no
example to copy, no validation beyond "is it JSON", and no way to see what is
currently in effect. A typo in `backend` is caught, but a typo in `model` is not
found until whisper fails.

**Fix:** `minutes config` to print the effective config, and `--init` to write a
commented default.

### ~~Every step is a separate command~~ — mostly fixed

Transcription now starts by itself when a recording stops, in the background, so
`stop` still returns in about a tenth of a second. Delivery is deliberately
still manual: which project a meeting's notes belong to is a judgment call.

**Still true:** transcription is slow. Whisper `small` on this machine's GPU runs
at roughly **real time**, and both tracks are transcribed, so a 30-minute
meeting occupies the GPU for about 30–45 minutes afterwards. Nothing queues
this — stopping two meetings close together will have them competing.

### ~~Short bleed fragments evade suppression~~ — a second pass catches them by level, and a shared-run test catches the long ones

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

### Thresholds are fixed, and one of them can still drop real speech

- ~~**Silence floor, −60 dBFS.**~~ **Disclosed.** Segments quieter than this are
  never sent to the model, so they produce no line *and no withheld line* —
  the stretch is simply absent, and nothing could tell a missing part of a
  meeting from a quiet one. The count now reaches `transcript.json` and the
  header rather than only the log. Measured across 106 segments of real
  meetings it has never fired once, which is what makes it worth reading when
  it does.
- **Echo suppression, 0.6 word containment within 2 seconds.** A short genuine
  agreement that repeats the other side — "yes, Thursday" right after somebody
  says "Thursday" — matches the echo test and is dropped from your track. The
  line is kept in `Withheld` with its reason, so it is recoverable; what is not
  recoverable is knowing it was yours.

**Not fixed by making them configurable, which is what this entry used to say.**
A knob requires guessing the right value before the meeting, and somebody who
knew to do that would not have had the problem. Disclosure is the better answer
in every case here: it tells the person afterwards that something went, which is
when they can actually judge whether it mattered. The one remaining case is
already disclosed as a count; what it lacks is attribution, not a setting.

---

## 3. Not built yet

These are known scope, not oversights.

- ~~**macOS** (R5).~~ **Built and proven.** A CoreAudio process tap read
  through a private aggregate, same framed-stdout shape, same orchestrator.
  Two non-silent tracks with clocks agreeing to 0.004 ms. Three things were
  learned doing it, all in [CLAUDE.md](../CLAUDE.md#macos-a-tap-and-three-things-the-platform-gets-wrong):
  the platform reports a sample rate it does not deliver at, and believing it
  writes every WAV header 8% fast; consent is `kTCCServiceAudioCapture`,
  checked in the IOProc path rather than at tap creation, and it blocks
  indefinitely rather than failing; and a tap delivers nothing while the render
  endpoint is idle, so `TRACK_INFO` must be emitted when a track starts rather
  than when its first packet arrives, or a quiet machine yields no track at
  all.
- **Native Linux.** The PulseAudio path (a source, plus the sink's `.monitor`)
  is described but not implemented. `preflight` refuses there rather than
  pretending.
- ~~**Process-specific loopback.**~~ **Built.** `--app` captures one process and
  its children. Two things learned doing it: the completion handler must be
  agile or the activation is refused with `E_ILLEGAL_METHOD_CALL`, which reads
  as "you called this wrong" rather than "your callback cannot be reached"; and
  a process-scoped stream's device counter measures delivered frames rather
  than elapsed time, so it must be placed by wall-clock alone — feeding it to
  the usual placer produced 98 re-anchors in ten seconds.
- **Summarising in the worker.** Deliberately moved to the driving session. See
  [usage.md](usage.md#delivering).

---

## 3b. Since proven against real hardware

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
- ~~**A long meeting.**~~ **Proven.** A real 117-minute call: 23 and 24 segments
  rotating cleanly, both tracks finishing 0.4 s apart, 2.5 GB on disk against a
  predicted 1.33 GB/hour. Ctrl-C stopped it cleanly and the far end dropped out
  repeatedly while the other party rebooted, with the timeline placing
  everything correctly around the gaps.
- ~~**An idle gap mid-recording.**~~ **Observed in the wild.** A real standup
  recorded on 2026-08-25 produced 10.7 seconds of gap-fill on the system track
  and fired the re-anchor guard twice — the render endpoint went idle mid-meeting
  and the device counter and wall clock diverged past the tolerance. The guard
  did what it was built for, and the two tracks still finished 14 ms apart over
  73 seconds. It had never fired against real hardware before this.
- ~~**Transcription on real meeting audio.**~~ **Done, with a caveat.** A real
  two-hour call transcribed to 1309 usable lines. What it also showed: 757
  microphone lines were echoes and had to be suppressed, whisper repeats itself
  on unclear audio, and it invents outright on silence. Quality on crosstalk and
  accents is *adequate*, not good — it is a record of what was said, not a
  reliable quote source.

- ~~**Anything but this machine.**~~ **Partly closed.** A second host now
  records: macOS captured, segmented, transcribed and delivered a real meeting
  end to end. What it cost was four bugs no test had caught, two of them
  platform defaults that were correct on the only machine that had ever run
  them — `manifest.Platform` hardcoded `wsl/windows`, and a `cuda` device
  default no Mac can satisfy, which fails *after* a meeting is captured. Still
  one Windows host, one Mac, and no Linux desktop.
- ~~**A faster device on macOS.**~~ **Measured, and the answer is no.**
  `torch.backends.mps.is_available()` is true on the Mac, so `--device mps`
  looks available and runs. It does not work: the decoder's logits come back
  all `-inf` on one run and all `nan` on the next, and whisper dies
  constructing a distribution from them. **No transcript is written at all.**

  The reason this is worth a paragraph rather than a line is how it presents.
  Wall-clock for the failing run was **43.8 s against 49.9 s on `cpu`** for the
  same 120 s of speech — so a comparison that timed the two and stopped there
  would have reported MPS as 12% faster and adopted it. The output is the only
  thing that distinguishes a fast device from a broken one, and the timing says
  the wrong thing confidently.

  That is the whole argument for asserting two cases must *differ in time and
  not differ in text*: either half alone proves nothing, and here the half most
  people would measure points the wrong way. `cpu` stays the default on darwin,
  and this is measured rather than assumed.
- ~~**That a message to a session which is not running is delivered when it
  starts.**~~ **Answered, and it was false.** The line was never
  running-or-not. A project the coordinator has *seen* — in its node's
  startable folder list — queues correctly; one it has **never been opened**
  is refused at send time and nothing is kept. `deliver` had no fallback for
  that case, because it was built on the sentence in shabadoo's `CLAUDE.md`
  saying otherwise.

  Worth recording how it was found, because it is this project's own rule
  biting somebody else. shabadoo first verified the claim by sending to a
  closed-but-known project, getting `deferred: true`, and reporting the general
  statement as confirmed — the single-sided fixture, in a check of a claim
  another project had flagged as load-bearing in shipped code. `runner` simply
  tried the other case and it bounced.

  A refusal now falls back to writing `delivery.md`, the same as an unreachable
  agent, and says the destination is unknown rather than reporting a send.

---

### Automatic delivery cannot see background room audio

Delivery to this machine's own session happens by itself once a transcript
exists, and stops if the transcript carries far-end-silent stretches. That guard
catches a **dropped call**. It does not catch somebody in the room while the
meeting is running: the far end is talking, so nothing is flagged.

The mitigation is the destination rather than the check. An automatic delivery
goes only to the local core session, where the transcript stays on the machine
that made it and a session with a person behind it reads it before anything
reaches another project. Anything bound elsewhere waits for `minutes deliver`.

That is a containment, not a solution. Process-specific loopback would be the
solution.

### ~~Nothing said the binary under test was older than the source~~ — fixed

`make install` warned about a dirty tree and nothing warned that the installed
binary predated the code. `minutes-mac` hit it three times in two days, once
nearly reporting a working check as broken.

`make test` now warns, comparing both the installed revision against HEAD and
the build time against the newest source file. Either alone misses a case: a
rebuild without a commit leaves the revision matching while the code has moved,
and a commit without a rebuild leaves the timestamp fresh while the revision has
not.

**It found a real one on its first run.** The installed binary was stamped two
commits behind despite `make install` having run in the same command as the
commit. The root cause is not pinned — a re-run installed correctly — which is
precisely the argument for a check rather than a habit.

### ~~The macOS bundle identifier named a private host~~ — renamed while it was cheap

An identifier built from a private homelab domain was chosen without knowing that the designated
requirement names it, so changing it makes TCC treat the binary as a new subject
and revokes every existing grant — exactly the way an ad-hoc rebuild does.
shabadoo hit the same thing and had to rename for an unrelated reason.

Two arguments, and the second is the one that made it urgent.

**The identifier is user-facing.** It is what System Settings shows under
Privacy & Security and what anybody inspecting the binary reads. A project
arguing that an active recording should be obvious rather than quiet cannot
identify its recorder with a private hostname a stranger cannot resolve.

**The second argument was "one prompt now against N later", and it was wrong.**
Recorded because it is the more useful half. The rename was predicted to cost one
consent prompt and measured to cost none: TCC keys its grant on the *responsible*
process — the launcher — so the helper's identifier does not gate consent at all,
and there was no N. The number was what made the change feel urgent rather than
tidy, and the number was imaginary.

Nothing was spent on it, because the first argument was sufficient and the change
is still right. But it is the second time a correlation on that machine has been
written down as a cause, and the pattern is worth more than either instance.

Now `com.github.alexj212.minutes-capture`, matching shabadoo's convention
because somebody reading System Settings should be able to see the two are
related. Keep it stable — not because moving it revokes anything, which is the
claim that turned out false, but because it is the name a person reads and a
name that moves tells them nothing.

### ~~A guard that could not run reported nothing~~ — fixed

`scripts/mission-check.sh` was inert on macOS for the four hours it existed.
macOS ships bash 3.2 as `/bin/sh`, which misparses `case` inside a command
substitution — it reads a pattern's `)` as the end of the substitution. Fixed by
`minutes-mac` with the POSIX leading-paren form.

The portability bug is not the finding. **`make test` invoked it as
`sh scripts/… || true`, so a guard that could not run looked exactly like a
guard that passed** — the same sentence this project wrote a day earlier about
its own microphone probe, one layer up and about the thing built to catch it.

Two properties made it invisible. The failure only appeared on stderr, and only
to somebody invoking the script directly; and it was found by `minutes-mac`
running it once casually after an unrelated edit, not by any test. That is the
third time this week the thing that caught a defect was somebody idly checking.

So there are three outcomes now rather than two: clean, findings, and the guard
itself broken. Findings stay advisory because they are warnings; **a guard that
will not run is reported loudly, because nothing else will ever mention it.**

The shape worth keeping is the one `minutes-mac` named: two checks whose failure
mode is silence, guarding a file whose failure mode is silence. Neither would
have said anything if the script simply never ran.

## What to fix next

The three highest items are done: one application can be captured instead of the
whole machine, a delivered recording cannot be sent from a directory that will
not survive, and every suppressed line is kept with the reason it went.

What is left:

1. ~~**A live check that each track is still producing audio.**~~ **Done.** A
   declared track that has delivered nothing after 60 seconds raises a desktop
   notification naming the track, while the meeting is still happening and
   somebody can still fix it. It notifies rather than logs, because the log is
   where the helper's own report of this sat unread for two days.
2. ~~**A desktop indicator on Windows**, where the meeting actually happens.~~
   **Done.** A red dot in the tray for the length of the recording, with the
   meeting's name, a running elapsed time, **Stop recording** and **Open
   folder**. Stopping asks the orchestrator rather than killing anything, so
   there is still one path to ending a recording rather than three.
3. ~~**`minutes config`.**~~ **Done.** `minutes config` shows what is in effect
   and says when there is no file; `minutes config set KEY VALUE` validates
   before writing and refuses an unknown key with near-misses. Hand-edited JSON
   silently ignored a mistyped key — the value sat in the file looking set while
   the default ran, which is the worst shape a settings file has.
4. **Background room audio during a meeting.** `--app` removes what the machine
   plays; it cannot remove what the room says while the far end is talking.
5. **The macOS consent dialog names the responsible process, not the
   recorder.** The one moment the OS tells a human "this program wants to
   record you", it names whatever launched the helper. That makes an
   out-of-band indicator carry the disclosure, which is an argument for the
   indicator rather than against it. **That indicator now exists on Windows**,
   which is where the meetings happen; the macOS half is the open question,
   because that is the platform where the dialog names the wrong program.

   Now measured rather than guessed at, and it is not a launch-shape problem.
   Across 24 attribution checks — the helper run directly, run under `setsid`
   as its own session leader, and spawned by the orchestrator as it really
   runs — TCC named `shabadoo` every single time. Responsibility is inherited
   through the process ancestry and is not broken by a new session or process
   group. Signing did not change it either, though it did make the helper
   identifiable: TCC now records `identifier=com.github.alexj212.minutes-capture` as the
   accessing process while still holding the launcher responsible. It knows
   what the recorder is and reports the other name anyway.

   The escape hatch exists and has a price. `responsibility_spawnattrs_setdisclaim`
   is present on the system; a parent that sets it makes the child its own
   responsible process, which would put `minutes-capture` in the dialog and in
   System Settings. But it is a `posix_spawnattr` applied by the parent at
   spawn time, and Go's `os/exec` uses fork/exec with no hook for spawn
   attributes — so it means cgo, in the orchestrator that is shared with
   Windows, for a disclosure improvement rather than a functional one. That
   **Decided: a real cost accepted, not an open question.** Not merely because
   cgo is expensive, but because this particular cgo costs the property the
   architecture is built on — the helpers are native where the platform forces
   it and the orchestrator builds anywhere. cgo in `internal/capture` makes
   that "portable except on darwin", and no single host can produce a full
   release set already. The trigger to revisit is the moment something else
   carries the disclosure instead, because whatever does that job had better
   name the right program.
