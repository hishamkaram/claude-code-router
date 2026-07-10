# Releasing CCR

CCR releases are created from an approved `main` branch after a semantic-version
tag such as `v0.1.0` is pushed.

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
make test-live
goreleaser check
goreleaser release --snapshot --clean
```

Live E2E requires an installed and authenticated Claude Code binary. If it skips
or cannot run, do not claim that a routing change is live-verified.

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
git tag -a v0.1.0 -m 'Release v0.1.0'
git push origin v0.1.0
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
