#!/bin/sh
set -eu

: "${DEVELOPER_DIR:?DEVELOPER_DIR must select a supported Xcode installation}"
: "${GITHUB_ENV:?GITHUB_ENV must name the workflow environment file}"
if [ ! -d "$DEVELOPER_DIR" ]; then
    echo "Xcode not found at $DEVELOPER_DIR; select a supported Xcode 26.x from the macos-26 runner image." >&2
    exit 1
fi
if ! swift_version=$(xcrun swiftc --version); then
    echo 'Unable to read the pinned Swift compiler version.' >&2
    exit 1
fi
printf '%s\n' "$swift_version"
if ! printf '%s\n' "$swift_version" | grep -Eq '(^|[[:space:]])Apple Swift version 6\.[0-3]([.[:space:]]|$)'; then
    echo 'CodeQL requires a supported Apple Swift compiler (6.0 through 6.3).' >&2
    exit 1
fi
printf 'DEVELOPER_DIR=%s\n' "$DEVELOPER_DIR" >> "$GITHUB_ENV"
