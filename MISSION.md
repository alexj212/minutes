# Records both sides of a desktop meeting, transcribes it, and hands a session the material to write notes from.
status: active
owner: minutes-mac
updated: 2026-08-30

## Now
Windows is in daily use. macOS capture is built and proven end to end — segment
rotation, crash safety and delivery all measured there — and the two-track
attribution premise is the one claim that platform has never demonstrated. Work
has moved from features to gates: the last five defects were one shape, a check
reporting success without having established anything, and every one was found
by a peer or by real audio rather than by the test suite.

## Waiting on
- shabadoo: hardened runtime, no audio-input entitlement · macOS will not prompt, no mic on any Mac · one entitlement
- you: 5.4 GB of recordings across two machines · no retention has ever run · one command
- you: the 2026-08-27 standup · a filed transcript is missing your side and does not say so · re-send or amend it
- you: Windows smoke test · this week's wiring is unit-tested but never integrated · ~30s of recording
- nobody: macOS recording indicator · the consent dialog names the launcher, not us · NSStatusBar + signing
- nobody: room audio during a meeting · --app removes what plays, not what the room says · unsolved

## Log
- 2026-08-30 the mic is not denied, it is unaskable. shabadoo v0.4.65 (built 14:14) signs with hardened runtime and no `com.apple.security.device.audio-input`; TCC logs "Policy disallows prompt" and refuses to raise a dialog at all. Signing adopted to make grants durable removed the capability instead, while every call kept returning success.
- 2026-08-30 the helper's signature is not the lever: three variants — hardened+entitlement, no-hardened, and shipping — all returned 96256 samples of 1 distinct value. Responsibility is the variable, as this file already said and we had drifted from believing.
- 2026-08-30 a denied mic never prompts again: macOS remembers a deny as it remembers a grant, so no dialog appears and every call returns success. "No dialog" is not evidence of a grant, and `waiting` cannot catch this — only the constant-signal probe separates told-no from waiting-for-a-human.
- 2026-08-30 the two TCC services are independent in practice: the system tap delivers 44100 Hz while the mic is denied on the same machine. Granting audio capture does not grant the microphone.
- 2026-08-29 preflight refuses a denied microphone. The first version could not fire — os/exec hands a closed stdin, so the helper stopped before capturing. A check that cannot fire is indistinguishable from one that passes.
- 2026-08-29 a denied mic opens, starts and returns zeros; preflight now refuses on a constant signal rather than a quiet one.
- 2026-08-29 echo escapes are segmentation divergence, not short fragments; fixed by longest shared word run, threshold measured across 700+ real lines rather than chosen.
- 2026-08-29 helper signing never bought the durable grant; TCC keys consent on the launcher, and the earlier before/after was confounded.
- 2026-08-28 tray indicator on Windows — the only disclosure that is continuously true rather than periodically asserted.
- 2026-08-28 R5 done — CoreAudio tap through a private aggregate; the platform reports a sample rate it does not deliver at.
