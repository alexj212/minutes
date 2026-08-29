#!/bin/sh
# Warn when the installed binary predates the source you just changed.
#
# minutes-mac hit this three times in two days: tested against a stale install,
# briefly believed a fixed thing was broken, and nearly reported a working check
# as returning the wrong answer. `make install` already warns about a dirty
# tree; nothing warned that the binary under test was older than the code.
#
# It compares the installed build's revision against HEAD, and its build time
# against the newest source file. Either alone is not enough: a rebuild without
# a commit leaves the revision matching while the code has moved, and a commit
# without a rebuild leaves the timestamp fresh while the revision has not.
set -e
BIN="${1:-$HOME/bin/minutes}"
[ -x "$BIN" ] || exit 0

json=$("$BIN" version --json 2>/dev/null) || exit 0
installed=$(printf '%s' "$json" | sed -n 's/.*"version": *"\([^"]*\)".*/\1/p' | head -1)
built=$(printf '%s' "$json" | sed -n 's/.*"built": *"\([^"]*\)".*/\1/p' | head -1)
head=$(git rev-parse --short HEAD 2>/dev/null || echo "")

stale=""
case "$installed" in
*-dirty) ;;   # already warned about at install time
*)
	if [ -n "$head" ] && [ -n "$installed" ] && [ "$installed" != "$head" ]; then
		stale="installed $installed, HEAD is $head"
	fi
	;;
esac

# Newest source file against the build timestamp. Catches the case the revision
# cannot: edited, not committed, not rebuilt.
if [ -n "$built" ]; then
	builtsec=$(date -d "$built" +%s 2>/dev/null || echo 0)
	if [ "$builtsec" -gt 0 ]; then
		newest=$(find cmd internal native -type f \( -name '*.go' -o -name '*.cpp' -o -name '*.swift' \) \
			-newermt "@$builtsec" -print 2>/dev/null | head -3)
		if [ -n "$newest" ]; then
			stale="${stale:+$stale; }source newer than the build:"
			for f in $newest; do stale="$stale $f"; done
		fi
	fi
fi

[ -z "$stale" ] && exit 0

echo "WARNING: $BIN is not this working tree." >&2
echo "         $stale" >&2
echo "         Testing against it will attribute your last change to a binary that" >&2
echo "         does not contain it. Run: make install" >&2
