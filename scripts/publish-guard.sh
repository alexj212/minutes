#!/bin/sh
# Refuse to publish something a node could not be told the truth about.
#
# Two ways a release set lies. A dirty tree stamps "-dirty", which names no
# commit — a node running it cannot be told what it is running. And an
# incomplete set installs fine, starts fine, and fails at the moment somebody
# tries to record, which is the worst time to discover a missing helper.
#
# Both are refused here rather than by the coordinator, because only the tool
# knows what a complete set of itself looks like.
set -e
BIN="$1"
[ -n "$BIN" ] || { echo "publish-guard: needs the path to the built binary" >&2; exit 2; }

json=$("$BIN" version --json)

case "$json" in
*-dirty*)
	if [ -z "$ALLOW_DIRTY" ]; then
		echo "refusing to publish from a dirty tree:" >&2
		git status --short >&2
		echo >&2
		echo "the stamp would say -dirty, which names no commit, so a node running it" >&2
		echo "could not be told what it is running. Commit, or ALLOW_DIRTY=1." >&2
		exit 1
	fi
	echo "publishing a dirty tree because ALLOW_DIRTY is set — the stamp names no commit" >&2
	;;
esac

# A component can be present, correct and complete and still be useless to the
# machine that receives it — a macOS helper signed ad-hoc carries a designated
# requirement that is a hash of its own bytes, so the consent grant it earns
# here does not survive the trip. That set installs cleanly, reports the right
# version, and refuses to record. It does not fail; it just does not work.
if printf '%s' "$json" | grep -q '"degraded"'; then
	if [ -z "$ALLOW_DEGRADED" ]; then
		echo "refusing to publish a degraded set:" >&2
		printf '%s\n' "$json" | grep -A1 '"degraded"' >&2
		echo >&2
		echo "every component is present, and one of them will not work properly on the" >&2
		echo "machine that installs it. Fix it, or ALLOW_DEGRADED=1 to publish anyway —" >&2
		echo "the reason is carried to whoever receives it either way." >&2
		exit 1
	fi
	echo "publishing a degraded set because ALLOW_DEGRADED is set" >&2
fi

if printf '%s' "$json" | grep -q '"present": *false'; then
	echo "refusing to publish an incomplete set:" >&2
	printf '%s\n' "$json" >&2
	echo >&2
	echo "a set missing a component installs fine and then refuses to record." >&2
	exit 1
fi
