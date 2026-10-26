#!/bin/sh
set -eu

project_dir=$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)
scratch=$(mktemp -d "${TMPDIR:-/tmp}/ccr-release-gate-test.XXXXXX")
trap 'rm -rf "$scratch"' EXIT HUP INT TERM
mkdir "$scratch/tools"
export GITHUB_REPOSITORY=fixture/ccr
export CCR_RELEASE_FIXTURE_CALLS="$scratch/calls"
export CCR_RELEASE_FIXTURE_RESULT=true
export CCR_RELEASE_FIXTURE_API_EXIT=0
export CCR_RELEASE_FIXTURE_LIST_EXIT=0
export CCR_RELEASE_FIXTURE_COMMIT=1111111111111111111111111111111111111111
cat > "$scratch/tools/gh" <<'EOF'
#!/bin/sh
set -eu
printf '%s\n' "$1" >> "$CCR_RELEASE_FIXTURE_CALLS"
if [ "$1" = api ]; then
    test "$2" = repos/fixture/ccr/commits/v0.6.13
    printf '%s\n' "$CCR_RELEASE_FIXTURE_COMMIT"
    exit "$CCR_RELEASE_FIXTURE_API_EXIT"
fi
test "$1" = run
test "$2" = list
shift 2
while [ "$#" -gt 0 ]; do
    case "$1" in
        --repo) test "$2" = fixture/ccr ;;
        --workflow) test "$2" = release.yml ;;
        --event) test "$2" = push ;;
        --branch) test "$2" = v0.6.13 ;;
        --commit) test "$2" = 1111111111111111111111111111111111111111 ;;
        --limit) test "$2" = 1 ;;
        --json) test "$2" = status,conclusion ;;
        --jq) test "$2" = '.[0] | .status == "completed" and .conclusion == "success"' ;;
        *) exit 9 ;;
    esac
    shift 2
done
printf '%s\n' "$CCR_RELEASE_FIXTURE_RESULT"
exit "$CCR_RELEASE_FIXTURE_LIST_EXIT"
EOF
chmod +x "$scratch/tools/gh"
export PATH="$scratch/tools:$PATH"
check_gate() {
    sh "$project_dir/scripts/require-release-checks.sh" "$@" > "$scratch/result" 2>&1
}
check_gate v0.6.13
for CCR_RELEASE_FIXTURE_RESULT in false null ''; do
    if check_gate v0.6.13; then
        echo 'incomplete or missing release checks unexpectedly passed' >&2
        exit 1
    fi
    grep -F 'requires a successful tag workflow' "$scratch/result" > /dev/null
done
CCR_RELEASE_FIXTURE_RESULT=true
CCR_RELEASE_FIXTURE_API_EXIT=7
if check_gate v0.6.13; then
    echo 'failed commit lookup unexpectedly passed' >&2
    exit 1
fi
CCR_RELEASE_FIXTURE_API_EXIT=0
for CCR_RELEASE_FIXTURE_COMMIT in '' null 123; do
    if check_gate v0.6.13; then
        echo 'invalid commit evidence unexpectedly passed' >&2
        exit 1
    fi
done
CCR_RELEASE_FIXTURE_COMMIT=1111111111111111111111111111111111111111
CCR_RELEASE_FIXTURE_LIST_EXIT=7
if check_gate v0.6.13; then
    echo 'failed workflow lookup unexpectedly passed' >&2
    exit 1
fi
rm "$CCR_RELEASE_FIXTURE_CALLS"
if check_gate invalid-tag; then
    echo 'invalid release tag unexpectedly passed' >&2
    exit 1
fi
test ! -e "$CCR_RELEASE_FIXTURE_CALLS"
invalid_multiline='v0.6.13
invalid'
if check_gate "$invalid_multiline"; then
    echo 'multiline release tag unexpectedly passed' >&2
    exit 1
fi
test ! -e "$CCR_RELEASE_FIXTURE_CALLS"
echo 'release gate tests passed'
