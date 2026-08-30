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
- shabadoo: v0.4.73 not published · v0.4.72 signed by v0.4.65's code, so no entitlement, mic still dead · publish
- nobody: system audio died at the v0.4.72 upgrade · 183 frames before, 0 after, 3 runs · cause unestablished
- wsl: ~5.2 GB of recordings there · the Mac's 254 MB is gone, retention ran and worked · one command
- you: the 2026-08-27 standup · a filed transcript is missing your side and does not say so · re-send or amend it
- you: Windows smoke test · this week's wiring is unit-tested but never integrated · ~30s of recording
- nobody: room audio during a meeting · --app removes what plays, not what the room says · unsolved

## Log
- 2026-08-30 the macOS recording indicator ask is parked here rather than in Waiting on: the list holds 6 rows and the parser drops the 7th silently, and a live regression outranks a disclosure improvement. It is unchanged — the consent dialog names the launcher, not us, and wants an NSStatusBar indicator. Restore it to the list when a row frees up.
- 2026-08-30 shabadoo published the entitled build as darwin/arm64 v0.4.71, verified in `shabadoo releases` rather than taken on the claim. The blocker moves to a node restart, which is Alex's call because it restarts the session that would trigger it.
- 2026-08-30 retention ran for the first time ever, on the Mac: `minutes rm --older-than` removed all 7 recordings and freed 253.9 MB, naming which had undelivered notes before touching them. The command works; nothing had ever run it.
- 2026-08-30 the mic is not denied, it is unaskable. shabadoo signs with hardened runtime and no `com.apple.security.device.audio-input`, so TCC logs "Policy disallows prompt" and refuses to raise a dialog at all. Not a regression in the installed build: hardened runtime arrived with signing itself at v0.4.40, and the log says "failed to match **existing** code requirement" — a grant existed, the first real signature invalidated the requirement it was recorded against, and that is exactly when a prompt is needed and exactly when the missing entitlement forbids one. The fix for the first problem created the conditions for the second. Fix shipping with the entitlement; not yet published.
- 2026-08-30 the helper's signature is not the lever: three variants — hardened+entitlement, no-hardened, and shipping — all returned 96256 samples of 1 distinct value. Responsibility is the variable, as this file already said and we had drifted from believing.
- 2026-08-30 a denied mic never prompts again: macOS remembers a deny as it remembers a grant, so no dialog appears and every call returns success. "No dialog" is not evidence of a grant, and `waiting` cannot catch this — only the constant-signal probe separates told-no from waiting-for-a-human.
- 2026-08-30 the two TCC services are independent in practice: the system tap delivers 44100 Hz while the mic is denied on the same machine. Granting audio capture does not grant the microphone.
- 2026-08-29 preflight refuses a denied microphone. The first version could not fire — os/exec hands a closed stdin, so the helper stopped before capturing. A check that cannot fire is indistinguishable from one that passes.
- 2026-08-29 a denied mic opens, starts and returns zeros; preflight now refuses on a constant signal rather than a quiet one.
- 2026-08-29 echo escapes are segmentation divergence, not short fragments; fixed by longest shared word run, threshold measured across 700+ real lines rather than chosen.
- 2026-08-29 helper signing never bought the durable grant; TCC keys consent on the launcher, and the earlier before/after was confounded.
- 2026-08-28 tray indicator on Windows — the only disclosure that is continuously true rather than periodically asserted.
- 2026-08-28 R5 done — CoreAudio tap through a private aggregate; the platform reports a sample rate it does not deliver at.
