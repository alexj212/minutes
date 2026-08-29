# Records both sides of a desktop meeting, transcribes it, and hands a session the material to write notes from.
status: active
updated: 2026-08-29

## Now
Windows is in daily use; macOS capture is built and proven end to end. Work has
moved from features to gates: the last five defects were one shape — a check
reporting success without having established anything — and every one was found
by a peer or by real audio, never by the test suite. A macOS status item is the
next build, deferred until the consent story stops moving.

## Waiting on
- you: mic grant on the Mac · every darwin measurement is blocked on it · System Settings toggle, one click
- you: retention · 5.2 GB here and 224 MB there, recordings of you and your home, no expiry anyone chose · one command
- you: devops half-meeting · a filed transcript is missing your side and does not say so · re-send or amend it
- you: Windows smoke test · this week's wiring is unit-tested but never integrated · ~30s of recording
- minutes-mac: darwin attribution · the two-track premise has never been demonstrated there · the grant, then speech
- nobody: background room audio · --app removes what the machine plays, not what the room says · unsolved

## Log
- 2026-08-29 preflight refuses a denied microphone. The first version could not fire — os/exec hands a closed stdin, so the helper stopped before capturing. A check that cannot fire is indistinguishable from one that passes.
- 2026-08-29 a denied mic opens, starts, returns success and delivers zeros. Caught before a meeting for the first time; the counterfactual is 60 seconds of nothing recorded as a success.
- 2026-08-29 echo escapes are segmentation divergence, not short fragments. Fixed by longest shared word run, threshold measured across 700+ real lines rather than chosen.
- 2026-08-29 helper signing never bought the durable grant; TCC keys consent on the launcher, and the earlier before/after was confounded.
- 2026-08-28 tray indicator on Windows — the only disclosure that is continuously true rather than periodically asserted.
- 2026-08-28 R5 done — CoreAudio tap through a private aggregate; the platform reports a sample rate it does not deliver at.
