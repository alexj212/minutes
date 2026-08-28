#!/bin/bash
# Build the macOS capture helper.
#
# Absolute paths throughout: Homebrew, Go and the native `claude` install are
# not on a non-interactive PATH on this machine, so a script that works when
# typed can fail when run from make, ssh or a hook. xcrun is in /usr/bin and is
# the one thing that can be relied on.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
root="$(cd "$here/../.." && pwd)"
out="$root/dist/minutes-capture"

if [ ! -x /usr/bin/xcrun ]; then
    echo "build.sh: /usr/bin/xcrun is missing — install the Xcode command line tools" >&2
    exit 1
fi

# CoreAudio process taps arrived in macOS 14.2 and the bundleIDs/process-restore
# properties in 26.0. Targeting the running system rather than the newest SDK
# keeps the failure at build time instead of at a meeting.
target="$(/usr/bin/sw_vers -productVersion | /usr/bin/cut -d. -f1).0"

mkdir -p "$root/dist"

/usr/bin/xcrun swiftc \
    -O \
    -target "arm64-apple-macosx${target}" \
    -framework CoreAudio \
    -framework AudioToolbox \
    -framework Foundation \
    -o "$out" \
    "$here/protocol.swift" \
    "$here/audio.swift" \
    "$here/main.swift"

echo "built $out"
