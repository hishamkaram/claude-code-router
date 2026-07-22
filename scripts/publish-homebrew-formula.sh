#!/usr/bin/env bash
set -euo pipefail

tag=${1:?usage: publish-homebrew-formula.sh vX.Y.Z}
tap_repository=${TAP_REPOSITORY:-hishamkaram/homebrew-tap}
formula_path=Formula/claude-code-router.rb
owner_repo=${CCR_REPOSITORY:-hishamkaram/claude-code-router}

if [[ ! "$tag" =~ ^v[0-9]+(\.[0-9]+){2}$ ]]; then
  printf 'stable semantic version tag required, got %q\n' "$tag" >&2
  exit 1
fi
if [[ -z ${HOMEBREW_TAP_TOKEN:-} ]]; then
  printf 'HOMEBREW_TAP_TOKEN is required\n' >&2
  exit 1
fi

archive=$(mktemp)
formula=$(mktemp)
trap 'rm -f "$archive" "$formula"' EXIT

source_url="https://codeload.github.com/${owner_repo}/tar.gz/refs/tags/${tag}"
curl --fail --location --retry 3 --silent --show-error "$source_url" --output "$archive"
sha256=$(sha256sum "$archive" | awk '{print $1}')

cat >"$formula" <<EOF
class ClaudeCodeRouter < Formula
  desc "Route Claude Code sessions to configured model providers"
  homepage "https://github.com/hishamkaram/claude-code-router"
  url "${source_url}"
  sha256 "${sha256}"
  license "MIT"

  depends_on "go" => :build

  def install
    ldflags = "-s -w -X github.com/hishamkaram/claude-code-router/internal/buildinfo.Version=#{version} -X github.com/hishamkaram/claude-code-router/internal/buildinfo.Commit=homebrew -X github.com/hishamkaram/claude-code-router/internal/buildinfo.BuiltBy=homebrew"
    system "go", "build", "-trimpath", "-ldflags=#{ldflags}", "-o", bin/"ccr", "./cmd/ccr"
  end

  test do
    assert_match "ccr #{version}", shell_output("#{bin}/ccr version")
  end
end
EOF

encoded=$(base64 --wrap=0 "$formula")
current_sha=$(GH_TOKEN="$HOMEBREW_TAP_TOKEN" gh api "repos/${tap_repository}/contents/${formula_path}" --jq .sha 2>/dev/null || true)
current_content=$(GH_TOKEN="$HOMEBREW_TAP_TOKEN" gh api "repos/${tap_repository}/contents/${formula_path}" --jq .content 2>/dev/null | base64 --decode 2>/dev/null || true)
if [[ "$current_content" == "$(cat "$formula")" ]]; then
  printf 'Homebrew formula already up to date for %s\n' "$tag"
  exit 0
fi
arguments=(api --method PUT "repos/${tap_repository}/contents/${formula_path}" --raw-field "message=brew: update claude-code-router to ${tag}" --raw-field "content=${encoded}")
if [[ -n "$current_sha" ]]; then
  arguments+=(--raw-field "sha=${current_sha}")
fi
GH_TOKEN="$HOMEBREW_TAP_TOKEN" gh "${arguments[@]}" >/dev/null
