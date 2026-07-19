# Releasing CCR

CCR releases are created from an approved `main` branch after a semantic-version
tag such as `v0.2.0` is pushed.

## One-Time Setup

1. Ensure `hishamkaram/claude-code-router` and
   `hishamkaram/homebrew-tap` are public.
2. Create a fine-grained personal access token limited to **Contents: Read and
   write** on `hishamkaram/homebrew-tap` only.
3. Add it as the `HOMEBREW_TAP_TOKEN` Actions secret in the CCR repository.
4. Do not reuse the token for GitHub Release publishing. The workflow's
   repository-scoped `GITHUB_TOKEN` publishes release assets; the tap token only
   updates `Formula/claude-code-router.rb`.

## Pre-Release Gate

From a clean, synchronized `main` branch:

```bash
make check
make test-live-fixture
CCR_LIVE_REAL_MATRIX=1 make test-live-real
CCR_LIVE_REAL_MATRIX=1 make test-live-matrix
go test -tags=live -count=1 -p 1 ./...
goreleaser check
goreleaser release --snapshot --clean
```

The fixture target requires an installed Claude Code CLI and no external
provider credential. It must pass without skips for both fixture protocols. The
real target uses first-party Anthropic authentication and every configured
non-blocked alias in the selected database; set `CCR_LIVE_CONFIGURED_DB` when
the release matrix is not in the default data directory. If a required live
target skips or cannot run, do not claim that the routing change is verified.

Pull-request CI also runs the fixture target through an eight-job matrix: Linux
and macOS, OpenAI-compatible and Anthropic-compatible protocols, and pinned
Claude Code 2.1.209 and the current npm release. The workflow runs on pull
requests, `main`, and a daily schedule.

For v0.2.1, record every gate in
[`docs/acceptance/v0.2.1.md`](acceptance/v0.2.1.md). A checked item requires
reproducible PR evidence.

Run the required review gate before committing the release-related change:

```bash
codex review -c 'sandbox_mode="danger-full-access"' \
  -c 'approval_policy="never"' \
  -c 'mcp_servers.playwright.enabled=false' \
  -c 'mcp_servers.chrome-devtools.enabled=false' \
  --uncommitted
```

Resolve all findings, rerun the review, commit, push, and merge the pull request.

## Publish

After `main` contains the approved release commit:

```bash
git switch main
git pull --ff-only origin main
git tag -a v0.2.1 -m 'Release v0.2.1'
git push origin v0.2.1
```

The tag workflow runs deterministic checks, creates macOS and Linux release
archives for `amd64` and `arm64`, publishes `checksums.txt`, and creates GitHub
provenance attestations. Stable tags also update the Homebrew source formula.

## Verify the Published Release

1. Download an archive and verify it against `checksums.txt`.
2. Verify provenance with:

   ```bash
   gh attestation verify <downloaded-archive> \
     --repo hishamkaram/claude-code-router \
     --signer-workflow hishamkaram/claude-code-router/.github/workflows/release.yml
   ```

3. On macOS, update the tap and install the tagged formula:

   ```bash
   brew update
   brew install hishamkaram/tap/claude-code-router
   ccr version
   ```

4. Confirm the generated formula points to the tagged source archive and its
   SHA-256 matches the GitHub source archive.
