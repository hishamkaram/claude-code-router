# Releasing CCR

CCR releases are created from an approved `main` branch after a semantic-version
tag such as `v0.3.0` is pushed. The tag workflow creates a draft release and a
signed, version-tagged managed-browser image only. Homebrew publication and the
browser image `:latest` tag happen after a maintainer manually promotes a
verified stable draft.

## One-Time Setup

1. Ensure `hishamkaram/claude-code-router` and
   `hishamkaram/homebrew-tap` are public.
2. Create a fine-grained personal access token limited to **Contents: Read and
   write** on `hishamkaram/homebrew-tap` only.
3. Add it as the `HOMEBREW_TAP_TOKEN` Actions secret in the CCR repository.
4. Do not reuse the token for GitHub Release publishing. The workflow's
   repository-scoped `GITHUB_TOKEN` publishes release assets and the signed
   GHCR browser image; the tap token only updates
   `Formula/claude-code-router.rb` during promotion.

## Pre-Release Gate

From a clean, synchronized `main` branch:

```bash
make check
make test-cua-docker-fixture
make test-cua-macos-fixture
make test-live-fixture
CCR_LIVE_REAL_MATRIX=1 make test-live-real
CCR_LIVE_REAL_MATRIX=1 make test-live-matrix
go test -tags=live -count=1 -p 1 ./...
goreleaser check
goreleaser release --snapshot --clean
```

When release claims include real vision, Anthropic CUA, OpenAI Responses CUA, or
executor behavior, configure the required environment and run the opt-in full
real aggregates as well:

```bash
CCR_LIVE_REAL_MATRIX=1 make test-live-real-full
CCR_LIVE_REAL_MATRIX=1 make test-live-matrix-full
```

On macOS, also run `make build-cua-macos`. The target intentionally fails on
other operating systems.

The fixture target requires an installed Claude Code CLI and no external
provider credential. It must pass without skips for all three fixture protocols:
`openai-chat`, `anthropic-native`, and `openai-responses`. A separate Linux job
builds the release Docker browser image and exercises its published loopback
CDP endpoint through a real screenshot. The default real target uses first-party
Anthropic authentication and every configured non-blocked routable alias in the
selected database; set `CCR_LIVE_CONFIGURED_DB` when the release matrix is not
in the default data directory. Set
`CCR_LIVE_REAL_CLAUDE_ACCOUNT=<registered-account>` when the matrix must use an
exact subscription-pool account instead of the currently logged-in Claude
identity. The named account must be available from the active OS keychain.
Opt-in local real targets cover vision,
Anthropic client-managed CUA, OpenAI Responses managed CUA, and CUA executors.
If a required live target skips or cannot run, do not claim that the routing
change is verified.

Pull-request CI also runs the fixture target through a 12-job matrix: Linux and
macOS, pinned Claude Code 2.1.209 and the current npm release, and
`openai-chat`, `anthropic-native`, and `openai-responses` protocols. The
workflow also has a separate macOS helper job that runs its portable protocol
fixtures and compiles the source-built preview. It runs on pull requests,
`main`, and a daily schedule.

The full live release gate also runs the complete live-tagged suite on Linux
with a ready systemd user manager, for Claude Code 2.1.209, 2.1.310, and the
current npm release. It repeats the interactive model-picker test ten times.
The JSON evidence checker rejects unexpected skips and missing mandatory test
results. Its explicit optional-test policy covers credential-dependent and
protected-deployment tests; those capabilities still require their separate
opt-in real-provider verification.

Tag releases reuse the CI, platform fixture matrix, and full live workflows.
Any failure, including a pre-existing failure, blocks draft publication.
Promotion also requires the exact tag commit's entire release workflow to have
completed successfully, including native archive smoke tests.

For each release, record every gate in `docs/acceptance/vX.Y.Z.md`. A checked
item requires reproducible PR evidence.

Complete the required review before committing the release-related change.
The lead reviews the full diff, and independent read-only subagents review the
runtime changes, live-test harness, and release gates. Record their findings and
resolution in the acceptance receipt. If an external review harness is selected,
use the configured Delegate integration.

Resolve all findings, rerun affected checks and reviews, then commit, push, and
merge the pull request only after all CI gates pass.

## Publish

After `main` contains the approved release commit:

```bash
git switch main
git pull --ff-only origin main
git tag -a vX.Y.Z -m 'Release vX.Y.Z'
git push origin vX.Y.Z
```

The tag workflow runs deterministic checks, creates macOS and Linux release
archives for `amd64` and `arm64`, publishes a draft GitHub Release with
`checksums.txt` and the checksum-verifying Linux `install.sh`, creates GitHub
provenance attestations, builds the managed browser image, pushes its version
tag to GHCR, and signs it with keyless cosign.
It does not move `browser:latest` or update Homebrew. Pre-release tags therefore
remain version-tagged images and cannot replace the stable default.

## Verify the Draft Release

1. Download an archive and verify it against `checksums.txt`.
2. Verify provenance with:

   ```bash
   gh attestation verify <downloaded-archive> \
     --repo hishamkaram/claude-code-router \
     --signer-workflow hishamkaram/claude-code-router/.github/workflows/release.yml
   ```

3. Verify the signed browser image:

   ```bash
   cosign verify \
     --certificate-identity-regexp 'https://github.com/hishamkaram/claude-code-router/.github/workflows/release.yml@refs/tags/vX.Y.Z' \
     --certificate-oidc-issuer 'https://token.actions.githubusercontent.com' \
     ghcr.io/hishamkaram/claude-code-router/browser:vX.Y.Z
   ```

4. Smoke-test at least one Linux archive and one macOS archive. The macOS
   computer-use helper is source-built only and is not included in Homebrew or
   GoReleaser archives. Do not describe it as shipped, signed, notarized, or
   production-hardened.
5. Download `install.sh` from the draft release and run `sh -n install.sh`.

## Promote

After draft verification, run the `Release` workflow manually with the stable
tag and `publish_homebrew=true`. The promotion job verifies the draft release,
checks the browser-image signature, moves `browser:latest` to the verified
version image, publishes the GitHub Release, then updates the Homebrew formula.
Re-running promotion is safe: the formula script exits cleanly when the tap
already points to the requested tag and SHA-256.

After promotion on macOS, update the tap and install the tagged formula:

```bash
brew update
brew install hishamkaram/tap/claude-code-router
ccr version
```

Confirm the generated formula points to the tagged source archive and its
SHA-256 matches the GitHub source archive.
