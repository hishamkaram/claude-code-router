#!/bin/sh

set -eu

script_directory=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
installer=$script_directory/install.sh
test_root=$(mktemp -d "${TMPDIR:-/tmp}/ccr-install-test.XXXXXX")
trap 'rm -rf "$test_root"' EXIT HUP INT TERM

mock_bin=$test_root/mock-bin
fixtures=$test_root/fixtures
url_log=$test_root/urls.log
base_path=$PATH
mkdir -p "$mock_bin" "$fixtures"
: >"$url_log"

fail() {
    printf 'installer test: %s\n' "$1" >&2
    exit 1
}

assert_equal() {
    expected=$1
    actual=$2
    message=$3
    [ "$expected" = "$actual" ] || fail "$message (expected '$expected', got '$actual')"
}

assert_file_contains() {
    file=$1
    expected=$2
    message=$3
    grep -F "$expected" "$file" >/dev/null 2>&1 || fail "$message (missing '$expected')"
}

checksum() {
    checksum_line=
    if command -v sha256sum >/dev/null 2>&1; then
        checksum_line=$(sha256sum "$1")
    elif command -v shasum >/dev/null 2>&1; then
        checksum_line=$(shasum -a 256 "$1")
    else
        fail 'sha256sum or shasum is required for installer tests'
    fi
    printf '%s\n' "${checksum_line%% *}"
}

cat >"$mock_bin/uname" <<'EOF'
#!/bin/sh
case "$1" in
    -s) printf '%s\n' "${CCR_TEST_OS:-Linux}" ;;
    -m) printf '%s\n' "${CCR_TEST_MACHINE:-x86_64}" ;;
    *) exit 1 ;;
esac
EOF
chmod 0755 "$mock_bin/uname"

cat >"$mock_bin/curl" <<'EOF'
#!/bin/sh
set -eu

url=
destination=
while [ "$#" -gt 0 ]; do
    case "$1" in
        --output)
            [ "$#" -ge 2 ] || exit 2
            destination=$2
            shift 2
            ;;
        http://*|https://*)
            url=$1
            shift
            ;;
        *)
            shift
            ;;
    esac
done

[ -n "$url" ] || exit 2
[ -n "$destination" ] || exit 2
printf '%s\n' "$url" >>"$CCR_TEST_URL_LOG"

case "$url" in
    */checksums.txt) source="$CCR_TEST_FIXTURES/checksums.txt" ;;
    */claude-code-router_linux_amd64.tar.gz) source="$CCR_TEST_FIXTURES/claude-code-router_linux_amd64.tar.gz" ;;
    */claude-code-router_linux_arm64.tar.gz) source="$CCR_TEST_FIXTURES/claude-code-router_linux_arm64.tar.gz" ;;
    *) exit 22 ;;
esac
cp "$source" "$destination"
EOF
chmod 0755 "$mock_bin/curl"

cat >"$mock_bin/awk" <<'EOF'
#!/bin/sh
exit 127
EOF
chmod 0755 "$mock_bin/awk"

create_release() {
    release_text=$1
    for architecture in amd64 arm64; do
        archive_directory=$fixtures/archive-$architecture
        mkdir -p "$archive_directory"
        printf '#!/bin/sh\nprintf "ccr %s\\n"\n' "$release_text" >"$archive_directory/ccr"
        chmod 0755 "$archive_directory/ccr"
        tar -czf "$fixtures/claude-code-router_linux_$architecture.tar.gz" \
            -C "$archive_directory" ccr
    done
    : >"$fixtures/checksums.txt"
    for architecture in amd64 arm64; do
        archive="$fixtures/claude-code-router_linux_$architecture.tar.gz"
        archive_checksum=$(checksum "$archive")
        printf '%s  %s\n' "$archive_checksum" "claude-code-router_linux_$architecture.tar.gz" >>"$fixtures/checksums.txt"
    done
}

run_installer() {
    HOME=$test_home \
    CCR_TEST_FIXTURES="$fixtures" \
    CCR_TEST_URL_LOG="$url_log" \
    PATH="$mock_bin:$base_path" \
    "$installer" "$@"
}

run_installer_without_home() {
    env -u HOME \
    CCR_TEST_FIXTURES="$fixtures" \
    CCR_TEST_URL_LOG="$url_log" \
    PATH="$mock_bin:$base_path" \
    "$installer" "$@"
}

create_release first

if HOME="$test_root/help-home" PATH="$mock_bin:$base_path" "$installer" --help >/dev/null 2>&1; then
    :
else
    fail '--help failed'
fi

: >"$url_log"
if HOME="$test_root/invalid-home" PATH="$mock_bin:$base_path" "$installer" --unknown >/dev/null 2>&1; then
    fail 'unknown option was accepted'
fi
[ ! -s "$url_log" ] || fail 'unknown option triggered a download'

if HOME="$test_root/missing-value-home" PATH="$mock_bin:$base_path" "$installer" --version >/dev/null 2>&1; then
    fail 'missing --version value was accepted'
fi

: >"$url_log"
if HOME="$test_root/option-value-home" PATH="$mock_bin:$base_path" "$installer" --version --help >/dev/null 2>&1; then
    fail 'option token was accepted as a --version value'
fi
[ ! -s "$url_log" ] || fail 'invalid --version value triggered a download'

test_home=$test_root/latest-home
mkdir -p "$test_home"
: >"$url_log"
run_installer >/dev/null
[ -x "$test_home/.local/bin/ccr" ] || fail 'latest install did not create an executable'
latest_output=$("$test_home/.local/bin/ccr" version 2>/dev/null || true)
assert_equal 'ccr first' "$latest_output" 'latest install produced the wrong version'
assert_file_contains "$url_log" '/releases/latest/download/claude-code-router_linux_amd64.tar.gz' \
    'latest install used the wrong archive URL'

test_home=$test_root/pinned-home
custom_dir=$test_home/custom-bin
mkdir -p "$test_home"
: >"$url_log"
run_installer --version 1.2.3 --dir "$custom_dir" >/dev/null
assert_equal 'ccr first' "$("$custom_dir/ccr" version 2>/dev/null || true)" 'pinned install produced the wrong version'
assert_file_contains "$url_log" '/releases/download/v1.2.3/claude-code-router_linux_amd64.tar.gz' \
    'pinned install used the wrong release URL'

no_home_dir=$test_root/no-home-bin
: >"$url_log"
run_installer_without_home --dir "$no_home_dir" >/dev/null
assert_equal 'ccr first' "$("$no_home_dir/ccr" version 2>/dev/null || true)" \
    'explicit --dir install incorrectly required HOME'

test_home=$test_root/arm-home
mkdir -p "$test_home"
CCR_TEST_MACHINE=aarch64 run_installer >/dev/null
assert_equal 'ccr first' "$("$test_home/.local/bin/ccr" version 2>/dev/null || true)" 'arm64 install produced the wrong version'

test_home=$test_root/update-home
mkdir -p "$test_home"
run_installer >/dev/null
old_checksum=$(checksum "$test_home/.local/bin/ccr")
create_release second
run_installer >/dev/null
new_checksum=$(checksum "$test_home/.local/bin/ccr")
[ "$old_checksum" != "$new_checksum" ] || fail 'update did not replace the existing binary'
assert_equal 'ccr second' "$("$test_home/.local/bin/ccr" version 2>/dev/null || true)" 'update produced the wrong version'

old_checksum=$new_checksum
printf '%064d  %s\n' 0 claude-code-router_linux_amd64.tar.gz >"$fixtures/checksums.txt"
if run_installer >/dev/null 2>&1; then
    fail 'checksum mismatch was accepted'
fi
new_checksum=$(checksum "$test_home/.local/bin/ccr")
assert_equal "$old_checksum" "$new_checksum" 'checksum failure replaced the existing binary'

printf '%s\n' 'not-the-amd64-entry' >"$fixtures/checksums.txt"
if run_installer >/dev/null 2>&1; then
    fail 'missing checksum entry was accepted'
fi
new_checksum=$(checksum "$test_home/.local/bin/ccr")
assert_equal "$old_checksum" "$new_checksum" 'missing checksum replaced the existing binary'

test_home=$test_root/unsupported-home
: >"$url_log"
if CCR_TEST_OS=Darwin run_installer >/dev/null 2>&1; then
    fail 'unsupported operating system was accepted'
fi
[ ! -s "$url_log" ] || fail 'unsupported operating system triggered a download'

if CCR_TEST_MACHINE=ppc64le run_installer >/dev/null 2>&1; then
    fail 'unsupported architecture was accepted'
fi
[ ! -s "$url_log" ] || fail 'unsupported architecture triggered a download'

printf '%s\n' 'installer tests passed'
