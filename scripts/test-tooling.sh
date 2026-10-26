#!/bin/sh
set -eu

project_dir=$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)
scratch=$(mktemp -d "${TMPDIR:-/tmp}/ccr-tooling-test.XXXXXX")
trap 'rm -rf "$scratch"' EXIT HUP INT TERM
mkdir -p "$scratch/tools with spaces"
lint_binary="$scratch/tools with spaces/golangci-lint"
scanner_binary="$scratch/tools with spaces/govulncheck"
export CCR_TOOL_FIXTURE_CALLS="$scratch/calls"
export CCR_LINT_FIXTURE_VERSION=9.8.7
export CCR_SCANNER_FIXTURE_VERSION=v6.5.4
export CCR_TOOL_FIXTURE_SCAN_EXIT=0

cat > "$lint_binary" <<'EOF'
#!/bin/sh
if [ "$1" = version ]; then
    printf 'golangci-lint has version %s built with go1.27.3\n' "$CCR_LINT_FIXTURE_VERSION"
    exit 0
fi
printf '%s\n' "$*" >> "$CCR_TOOL_FIXTURE_CALLS"
exit "$CCR_TOOL_FIXTURE_SCAN_EXIT"
EOF
cat > "$scanner_binary" <<'EOF'
#!/bin/sh
if [ "$1" = -version ]; then
    printf 'Scanner: govulncheck@%s\n' "$CCR_SCANNER_FIXTURE_VERSION"
    exit 0
fi
printf '%s\n' "$*" >> "$CCR_TOOL_FIXTURE_CALLS"
exit "$CCR_TOOL_FIXTURE_SCAN_EXIT"
EOF
chmod +x "$lint_binary" "$scanner_binary"

run_make() {
    make --no-print-directory -C "$project_dir" \
        GOLANGCI_LINT="$lint_binary" GOLANGCI_LINT_VERSION=v9.8.7 \
        GOVULNCHECK="$scanner_binary" GOVULNCHECK_VERSION=v6.5.4 \
        "$@" > "$scratch/result" 2>&1
}

run_make -s print-tool-versions
expected=$(printf 'golangci_lint=v9.8.7\ngovulncheck=v6.5.4')
test "$(cat "$scratch/result")" = "$expected"

CCR_LINT_FIXTURE_VERSION=9.8.6
if run_make lint; then
    echo 'stale linter unexpectedly passed' >&2
    exit 1
fi
test ! -e "$CCR_TOOL_FIXTURE_CALLS"
grep -F 'need v9.8.7' "$scratch/result" > /dev/null

CCR_LINT_FIXTURE_VERSION=9.8.7
run_make lint
test "$(cat "$CCR_TOOL_FIXTURE_CALLS")" = 'run ./...'
rm "$CCR_TOOL_FIXTURE_CALLS"

CCR_SCANNER_FIXTURE_VERSION=v6.5.3
if run_make govulncheck; then
    echo 'stale vulnerability scanner unexpectedly passed' >&2
    exit 1
fi
test ! -e "$CCR_TOOL_FIXTURE_CALLS"
grep -F 'need v6.5.4' "$scratch/result" > /dev/null

CCR_SCANNER_FIXTURE_VERSION=v6.5.4
run_make govulncheck
test "$(cat "$CCR_TOOL_FIXTURE_CALLS")" = './...'
rm "$CCR_TOOL_FIXTURE_CALLS"

CCR_TOOL_FIXTURE_SCAN_EXIT=7
for target in lint govulncheck; do
    if run_make "$target"; then
        echo "$target hid a scanner failure" >&2
        exit 1
    fi
    test -s "$CCR_TOOL_FIXTURE_CALLS"
    rm "$CCR_TOOL_FIXTURE_CALLS"
done

if run_make lint GOLANGCI_LINT="$scratch/missing"; then
    echo 'missing linter unexpectedly passed' >&2
    exit 1
fi
grep -F 'is required' "$scratch/result" > /dev/null
test ! -e "$CCR_TOOL_FIXTURE_CALLS"

echo 'tooling tests passed'
