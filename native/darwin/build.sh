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

# Sign it, and sign it with something stable.
#
# An unsigned binary gets an ad-hoc signature whose designated requirement is a
# bare cdhash — a hash of the bytes. TCC attaches its audio-capture decision to
# that, so every rebuild is a new stranger and the operator is asked for
# permission again. Measured: consent granted, helper rebuilt, asked again.
#
# A real signing identity gives a requirement based on the certificate and the
# identifier instead, which does not move when the code does. The grant then
# survives a rebuild, which is the difference between asking somebody once and
# asking them every time they change a line.
#
# The identity is discovered rather than hardcoded, because this repo is shared
# with machines that do not have one. Falling back to ad-hoc keeps the build
# working there; it just does not get a durable grant.
# The identifier is user-facing and permanent, in that order of importance.
#
# It is the string System Settings shows under Privacy & Security, and the one
# in the designated requirement anybody inspecting the binary reads. On a
# project arguing that an active recording should be obvious rather than quiet,
# the name identifying the recorder has to be one a stranger can resolve. The
# old one named a private host nobody outside this machine could look up.
#
# Renamed on 2026-08-29. The rename was predicted to cost one consent prompt and
# measured to cost none — TCC keys its grant on the responsible process, which
# is the launcher, so the helper's identifier does not gate consent at all. The
# urgency argument for renaming was wrong; the reason above was not, and should
# have carried it alone.
#
# Keep it stable anyway, for a smaller reason than the one first written here: it
# is the name a person reads in System Settings, and a name that moves is a name
# that tells them nothing.
identifier="com.github.alexj212.minutes-capture"

identity="${MINUTES_CODESIGN_IDENTITY:-}"
if [ -z "$identity" ]; then
    identity="$(/usr/bin/security find-identity -v -p codesigning 2>/dev/null \
        | /usr/bin/awk 'NR==1 && /\)/ { print $2 }')"
fi

if [ -n "$identity" ]; then
    /usr/bin/codesign --force --sign "$identity" \
        --identifier "$identifier" \
        --options runtime \
        --timestamp=none \
        "$out"
    echo "signed with $identity"
else
    /usr/bin/codesign --force --sign - \
        --identifier "$identifier" \
        "$out"
    echo "signed ad-hoc: no codesigning identity found. The binary works here," >&2
    echo "but names no author anyone can resolve, and a Mac that downloads it" >&2
    echo "may refuse to run it. It does not affect recording permission." >&2
fi

echo "built $out"
