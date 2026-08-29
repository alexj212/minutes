# Records both sides of a desktop meeting, transcribes them, and hands a session the material to write notes from.
status: active
updated: 2026-08-29

## Now
Windows is in daily use. macOS capture is built and proven end to end — segment
rotation, crash safety and delivery all measured there — and the two-track
attribution premise is the one claim the platform has never demonstrated.

## Waiting on
- you: mic grant on the Mac · attribution unproven on darwin · System Settings toggle, one click
- you: 224 MB of recordings of you and your home · no retention has ever run · one word to delete
- minutes-wsl: micNoPackets has no observed instance · may be unreachable · needs a device that declares and never writes
- nobody: macOS recording indicator · the consent dialog names the launcher, so nothing discloses · NSStatusBar plus signing work
- nobody: background room audio during a meeting · --app removes what the machine plays, not what the room says · unsolved

## Log
- 2026-08-29 a denied mic opens, starts and returns zeros; preflight now refuses on a constant signal rather than a quiet one
- 2026-08-29 the check shipped inert on both platforms — os/exec handed the helper a closed stdin, so it stopped before capturing
- 2026-08-29 echo escapes are segmentation divergence, not short fragments; fixed by longest shared word run, threshold measured not chosen
- 2026-08-29 helper signing never bought the durable grant; TCC keys consent on the launcher, and the earlier before/after was confounded
- 2026-08-28 R5 done — CoreAudio tap through a private aggregate; the platform reports a sample rate it does not deliver at
