#!/bin/sh
# Check MISSION.md against the limits that are enforced by silent truncation.
#
# Over-length rows and a seventh entry are dropped by the parser with no
# warning, and the file still reads correctly on disk — so the blocker is gone
# from the dashboard and present in the repo, and nothing tells anybody. That is
# the same failure as a denied microphone reporting ok, arriving in the file
# whose entire purpose is advertising blockers.
#
# It caught both sessions working on this file twice inside an hour, each while
# specifically thinking about it, and once immediately after a check printed the
# warning. So the answer is not care: a limit enforced by silent truncation
# cannot be complied with by attention. This runs from `make test`.
set -e
F="${1:-MISSION.md}"
[ -f "$F" ] || exit 0

fail=0
say() { echo "$F: $1" >&2; fail=1; }

status=$(sed -n 's/^status: *//p' "$F" | head -1)
case "$status" in
active|blocked|paused|done) ;;
"") say "no status: line — the dashboard cannot tell paused from blocked" ;;
*)  say "status \"$status\" is not one of active, blocked, paused, done — anything else is dropped" ;;
esac

# owner: names the session that writes this file, for a project whose checkouts
# span machines. Absent means nobody declared it, which is not the same as this
# node owning it — on a single-machine project absence is normal.
#
# Checked here only for shape. Whether an owner is *needed* depends on how many
# checkouts exist, and the dashboard is the only place both are visible at once.
owner=$(sed -n 's/^owner: *//p' "$F" | head -1)
case "$owner" in
"") ;;
*[!a-zA-Z0-9._-]*) say "owner \"$owner\" is not a session name — it is the value a dashboard groups by" ;;
esac

rows=$(sed -n '/^## Waiting on/,/^## /p' "$F" | grep '^- ' || true)
n=$(printf '%s\n' "$rows" | grep -c '^- ' || true)
[ "$n" -gt 6 ] && say "$n Waiting on rows; the parser keeps 6 and drops the rest silently"

printf '%s\n' "$rows" | while IFS= read -r line; do
	[ -n "$line" ] || continue
	text=${line#- }
	runes=$(printf '%s' "$text" | wc -m)
	[ "$runes" -gt 120 ] && echo "$F: $runes runes (limit 120), dropped silently: $text" >&2
	case "$text" in
	*:\ *) ;;
	*) echo "$F: no owner before the colon — a blocker nobody owns is a complaint: $text" >&2 ;;
	esac
done

# The subshell above cannot set fail, so re-run the two per-row checks for the
# exit status. Cheap, and an exit code that disagrees with the output is its own
# version of this bug.
over=$(printf '%s\n' "$rows" | while IFS= read -r line; do
	[ -n "$line" ] || continue
	text=${line#- }
	r=$(printf '%s' "$text" | wc -m)
	if [ "$r" -gt 120 ]; then echo x; fi
	case "$text" in (*:\ *) ;; (*) echo x ;; esac
done | wc -l)
[ "$over" -gt 0 ] && fail=1

exit $fail
