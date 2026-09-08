# Records both sides of a desktop meeting, transcribes it, and hands a session the material to write notes from.
status: active
owner: minutes-mac
updated: 2026-08-31

## Now
Paused mid-flight by the usage limit and assessing rather than working. A
recording is live right now with its system track dead since 14:21 and its
microphone still writing, which is the exact failure this project exists to
refuse, happening while nobody was watching.

## Waiting on
- you: system track dead 41 min into a LIVE recording · mic still going, far end not captured · look now
- you: rotate the root password · live, and public in git history at 77265e4 · rotate, then decide on a rewrite
- you: installed binary is 37 commits stale · the pid and preflight fixes are absent · `make install`
- devops: the 97-min Jeff+Jagan meeting is undelivered · 731 lines nobody has read · `minutes deliver`
- nobody: afterStop did not fire on a 97-min recording · silent, looks exactly like success · undiagnosed
- nobody: macOS system tap holds a sticky dead mode · 7 leads eliminated, no trigger · needs the physical Mac

## Log
- 2026-08-31 a LIVE recording has had no system audio since 14:21 while the mic keeps writing: 24 mic segments against 14 system, process and helper both alive, marker present. One side of a meeting, in progress, with nothing saying so. Not diagnosed and not touched — assessment only.
- 2026-08-31 `afterStop` did not fire on the 97-minute Jeff+Jagan recording (wsl). Stopped cleanly, no error, both tracks complete, transcription never started and nothing reported it. Ran by hand afterwards and it worked. Observed on the INSTALLED binary, 17bde87 of 2026-08-29 — 37 commits behind master — so the first diagnostic step is establishing which version it reproduces on.
- 2026-08-31 the staleness guard has been warning for two days and exits 0, so `make test` prints it and passes. It works exactly as built and nobody acted on it, including me: I reported the binary as "one commit behind, on purpose" when it was 37.
- 2026-08-31 a live unrotated root password reached the public repo in `skills/minutes/SKILL.md`, in the section arguing against writing secrets down. Removed from HEAD at 93f0645; still readable at 77265e4. Cleanup is not remediation — the rotation is the remedy.
- 2026-08-30 99f43d9 builds and signs on the Mac — first compile of it anywhere, since WSL has no Swift toolchain and every Go guard passed over a file none of them can read. Its refusal then acted as an instrument: run in the dead mode, it does NOT fire, so the tap opens with a valid clock source and delivers nothing anyway. The clock-source hypothesis is dead across the failing mode rather than in one sample. An instrument that stays silent in the failing state tells you where the fault is not.
- 2026-08-30 my own UID probe ran once and I accepted it as ruling the branch out, having just used bimodality to retire somebody else's test. Caught by minutes-wsl. A single negative sample under a two-mode fault separates nothing, and it is harder to see when the control is your own.
- 2026-08-30 the dead mode is sticky, not oscillating: 90 consecutive captures over ~3 minutes with nothing touched and a tone playing gave 0/90 and logged no transition. Eliminated as triggers: a both-track capture, a preflight run, a mic-only capture, and the render client itself — afplay and `say` are equally invisible, so the tap is not blind to one app. Default output device unchanged throughout. Because the mode does not drift on its own, a single before/after around one change IS informative again, which restores the test withdrawn earlier.
- 2026-08-30 the tap is bimodal, not probabilistic. Twenty consecutive captures delivered, then twenty consecutive delivered nothing, same command and same playing tone minutes apart. It holds a state across a run of attempts rather than failing at random, which is why single runs all afternoon looked like a clean before-and-after. The rate measurement was shabadoo's suggestion and it produced something better than a rate.
- 2026-08-30 the mic works end to end. Consent granted (authValue=2, authReason=2), shipping binary captures 96193 distinct values, preflight passes for the first time on this machine. The entitled-helper variant is indistinguishable, so the responsible-process finding survives its own fix.
- 2026-08-30 "system audio is dead" was wrong; it is INTERMITTENT. Live in two runs (229376 samples, 110178 distinct) and empty in roughly twenty others across two hours, with afplay verified running each time. Duration does not control it, capturing both tracks does not control it, and preflight reported "carrying signal" in the same minute three manual runs reported nothing. Intermittent is a different defect from dead and harder; recorded as unexplained rather than closed.
- 2026-08-30 retracted: "audio capture is broken below both projects". ffmpeg failed to open the mic only because consent was pending. The independent oracle was right; the conclusion drawn from it was not.
- 2026-08-30 the entitlement fix is proven: TCC went from "Policy disallows prompt" to "allow prompt: Allow" and raised a dialog. OSStatus 0x10000004 is what an unanswered consent wait returns — a helper held open blocked 3m25s, nobody answered, it expired. The mic is blocked on a human, not a fault.
- 2026-08-30 system audio is a separate defect, not the consent confound: measured with zero helpers running and no request pending, tone playing, and it still delivers zero frames against a 183-frame baseline. Only untested lead is Splashtop's Core Audio driver, which neither project installed.
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
