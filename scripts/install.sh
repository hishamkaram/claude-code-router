#!/bin/sh

set -eu

repository=hishamkaram/claude-code-router
home=${HOME:-}
install_dir=
version=latest

usage() {
    cat <<'EOF'
Install the latest CCR release for Linux.

Usage:
  install.sh [--version VERSION] [--dir DIRECTORY]

Options:
  --version VERSION  install a specific release tag (with or without the v prefix)
  --dir DIRECTORY    install ccr into DIRECTORY (default: ~/.local/bin)
  -h, --help         show this help

Environment:
  None
EOF
}

fail() {
    printf 'ccr installer: %s\n' "$1" >&2
    exit 1
}

require_value() {
    [ "$#" -ge 2 ] || fail "$1 requires a value"
    [ -n "$2" ] || fail "$1 requires a non-empty value"
}

while [ "$#" -gt 0 ]; do
    case "$1" in
        --version)
            require_value "$1" "${2-}"
            version=$2
            shift 2
            ;;
        --dir)
            require_value "$1" "${2-}"
            install_dir=$2
            shift 2
            ;;
        -h|--help)
            usage
            exit 0
            ;;
        *)
            fail "unknown option: $1 (use --help for usage)"
            ;;
    esac
done

[ -n "$home" ] || fail 'HOME is not set'
[ -n "$install_dir" ] || install_dir=$home/.local/bin

case "$version" in
    latest)
        release_base="https://github.com/$repository/releases/latest/download"
        ;;
    *[!A-Za-z0-9._-]*)
        fail 'version may contain only letters, numbers, dots, underscores, and hyphens'
        ;;
    *)
        case "$version" in
            v*) tag=$version ;;
            *) tag=v$version ;;
        esac
        release_base="https://github.com/$repository/releases/download/$tag"
        ;;
esac

case "$(uname -s)" in
    Linux) ;;
    *) fail "unsupported operating system: $(uname -s) (this installer supports Linux)" ;;
esac

case "$(uname -m)" in
    x86_64|amd64) architecture=amd64 ;;
    aarch64|arm64) architecture=arm64 ;;
    *) fail "unsupported architecture: $(uname -m) (supported: amd64, arm64)" ;;
esac

command -v tar >/dev/null 2>&1 || fail 'tar is required'
command -v install >/dev/null 2>&1 || fail 'install is required'
command -v mkdir >/dev/null 2>&1 || fail 'mkdir is required'
command -v mktemp >/dev/null 2>&1 || fail 'mktemp is required'
command -v mv >/dev/null 2>&1 || fail 'mv is required'

download() {
    url=$1
    destination=$2
    if command -v curl >/dev/null 2>&1; then
        curl --fail --location --retry 3 --silent --show-error "$url" --output "$destination" || \
            fail "download failed: $url"
    elif command -v wget >/dev/null 2>&1; then
        wget --quiet --output-document="$destination" "$url" || \
            fail "download failed: $url"
    else
        fail 'curl or wget is required'
    fi
}

sha256() {
    file=$1
    if command -v sha256sum >/dev/null 2>&1; then
        sha256sum "$file" | awk '{print $1}'
    elif command -v shasum >/dev/null 2>&1; then
        shasum -a 256 "$file" | awk '{print $1}'
    else
        fail 'sha256sum or shasum is required to verify the release'
    fi
}

temporary_directory=$(mktemp -d "${TMPDIR:-/tmp}/ccr-install.XXXXXX") || fail 'could not create a temporary directory'
temporary_binary=
cleanup() {
    rm -rf "$temporary_directory"
    if [ -n "$temporary_binary" ]; then
        rm -f "$temporary_binary"
    fi
}
trap cleanup EXIT HUP INT TERM

archive_name="claude-code-router_linux_$architecture.tar.gz"
archive_path="$temporary_directory/$archive_name"
checksums_path="$temporary_directory/checksums.txt"

printf 'Downloading CCR (%s, %s)...\n' "$architecture" "$version"
download "$release_base/$archive_name" "$archive_path"
download "$release_base/checksums.txt" "$checksums_path"

expected_checksum=$(awk -v name="$archive_name" '$2 == name { print $1; exit }' "$checksums_path")
[ -n "$expected_checksum" ] || fail "checksums.txt does not contain $archive_name"
case "$expected_checksum" in
    *[!0-9A-Fa-f]*) fail "invalid checksum for $archive_name" ;;
esac

actual_checksum=$(sha256 "$archive_path")
[ "$actual_checksum" = "$expected_checksum" ] || fail "checksum verification failed for $archive_name"

tar -xzf "$archive_path" -C "$temporary_directory" ccr || fail 'release archive could not extract ccr'
[ -f "$temporary_directory/ccr" ] || fail 'release archive does not contain ccr'

mkdir -p "$install_dir" || fail "cannot create installation directory: $install_dir"
temporary_binary=$(mktemp "$install_dir/.ccr-install.XXXXXX") || \
    fail "cannot create a temporary file in $install_dir (try --dir with a writable directory)"
install -m 0755 "$temporary_directory/ccr" "$temporary_binary" || \
    fail "cannot install to $install_dir (try --dir with a writable directory)"
mv -f "$temporary_binary" "$install_dir/ccr" || \
    fail "cannot replace $install_dir/ccr"
temporary_binary=

printf 'Installed ccr at %s/ccr\n' "$install_dir"
"$install_dir/ccr" version

case ":${PATH:-}:" in
    *:"$install_dir":*) ;;
    *)
        printf '\n%s is not on PATH. Add it to your shell profile:\n' "$install_dir"
        printf "  export PATH=\"%s:\$PATH\"\n" "$install_dir"
        ;;
esac
