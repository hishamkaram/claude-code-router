#!/bin/sh
set -eu

valid_tag() {
    case "$1" in
        *[!v0-9.]*) return 1 ;;
    esac
    printf '%s\n' "$1" | LC_ALL=C grep -Eq '^v[0-9]+(\.[0-9]+){2}$'
}
if [ "$#" -ne 1 ] || ! valid_tag "$1"; then
    echo 'usage: require-release-checks.sh vMAJOR.MINOR.PATCH' >&2
    exit 2
fi
: "${GITHUB_REPOSITORY:?GITHUB_REPOSITORY is required}"
tag=$1
commit=$(gh api "repos/$GITHUB_REPOSITORY/commits/$tag" --jq .sha)
case "$commit" in
    ''|*[!0-9a-f]*) echo 'GitHub returned an invalid tag commit.' >&2; exit 1 ;;
esac
case "${#commit}" in
    40|64) ;;
    *) echo 'GitHub returned an invalid tag commit.' >&2; exit 1 ;;
esac
passed=$(gh run list --repo "$GITHUB_REPOSITORY" --workflow release.yml \
    --event push --branch "$tag" --commit "$commit" --limit 1 \
    --json status,conclusion --jq '.[0] | .status == "completed" and .conclusion == "success"')
if [ "$passed" != true ]; then
    echo "Release $tag requires a successful tag workflow, including the full live suite and archive smoke tests." >&2
    exit 1
fi
