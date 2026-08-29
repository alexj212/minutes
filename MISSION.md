# Records both sides of a desktop meeting, transcribes it, and hands the material to a session that writes the notes.
status: active
updated: 2026-08-29

## Now
Hardening the gates rather than adding features. The last four defects were all
one shape — a check that reported success without having established anything —
and each was found by a peer or by real audio, never by the test suite.

## Waiting on
- you: Mac microphone grant — every darwin measurement is blocked on it; one click in System Settings
- you: Windows smoke test — today's wiring is unit-tested but never integrated; ~30s of recording
- you: devops half-meeting — a filed transcript is missing your side and does not say so; re-send or amend
- you: retention — 5.2 GB of meeting audio with no expiry anyone chose; one config command
- minutes-mac: darwin attribution — the two-track premise is unproven there; needs the grant, then speech
- nobody: background room audio — `--app` removes what the machine plays, not what the room says

## Log
- 2026-08-29 preflight now refuses a denied microphone. The first version could not fire: the helper stops on stdin EOF and `os/exec` hands it a closed one, so it passed everywhere. A check that cannot fire is indistinguishable from one that passes.
- 2026-08-29 a denied macOS microphone opens, starts, returns success and delivers zeros. Caught before a meeting for the first time; the counterfactual is a 60-second recording of nothing, reported as a success.
- 2026-08-29 echo suppression now compares contiguous word runs rather than whole-line similarity. Whisper segments the two tracks differently, so a complete sentence was being published in the operator's mouth at a measured 1-in-10.
- 2026-08-28 tray indicator on Windows: the only disclosure that is continuously true rather than periodically asserted.
- 2026-08-28 a 44-minute standup recorded zero microphone frames and was delivered as a complete meeting. Now disclosed afterwards, and caught within 60 seconds during.
