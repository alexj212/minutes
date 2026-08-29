# Records both sides of a desktop meeting, transcribes it, and hands a session the material to write notes from.
status: active
updated: 2026-08-29

## Now
Windows is in daily use. macOS capture is built and proven end to end — segment
rotation, crash safety and delivery all measured there — and the two-track
attribution premise is the one claim that platform has never demonstrated. Work
has moved from features to gates: the last five defects were one shape, a check
reporting success without having established anything, and every one was found
by a peer or by real audio rather than by the test suite.

## Waiting on
- you: mic grant on the Mac · attribution unproven on darwin · one click in System Settings
- you: 5.2 GB here and 224 MB there, recordings of you and your home · no retention has ever run · one command
- you: devops half-meeting · a filed transcript is missing your side and does not say so · re-send or amend it
- you: Windows smoke test · this week's wiring is unit-tested but never integrated · ~30s of recording
- nobody: macOS recording indicator · the consent dialog names the launcher, so nothing discloses · NSStatusBar plus signing
- nobody: room audio during a meeting · --app removes what plays, not what the room says · unsolved

## Log
- 2026-08-29 preflight refuses a denied microphone. The first version could not fire — os/exec hands a closed stdin, so the helper stopped before capturing. A check that cannot fire is indistinguishable from one that passes.
- 2026-08-29 a denied mic opens, starts and returns zeros; preflight now refuses on a constant signal rather than a quiet one.
- 2026-08-29 echo escapes are segmentation divergence, not short fragments; fixed by longest shared word run, threshold measured across 700+ real lines rather than chosen.
- 2026-08-29 helper signing never bought the durable grant; TCC keys consent on the launcher, and the earlier before/after was confounded.
- 2026-08-28 tray indicator on Windows — the only disclosure that is continuously true rather than periodically asserted.
- 2026-08-28 R5 done — CoreAudio tap through a private aggregate; the platform reports a sample rate it does not deliver at.
