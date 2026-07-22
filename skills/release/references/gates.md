# Release Gates

## Evidence Rules

- A checked acceptance item needs a command, run URL, or reproducible artifact.
- Skipped live tests do not verify router behavior.
- Fixture live E2E proves Claude Code integration without provider secrets.
- Local real matrix proves configured providers and first-party Anthropic.
- Do not call behavior shipped until the release artifact or post-release
  evidence proves it.

## Required Pre-Tag Gates

Run the affected subset first, then the full release gate:

```bash
go test ./...
go test -race -count=1 -p 4 ./...
go vet ./...
golangci-lint run ./...
govulncheck ./...
make test-cua-macos-fixture
make test-live-fixture
CCR_LIVE_REAL_MATRIX=1 make test-live-real
CCR_LIVE_REAL_MATRIX=1 make test-live-matrix
go test -tags=live -count=1 -p 1 ./...
goreleaser check
goreleaser release --snapshot --clean
```

When release claims include real vision, Anthropic CUA, OpenAI Responses CUA, or
executor behavior, configure the required environment and also run:

```bash
CCR_LIVE_REAL_MATRIX=1 make test-live-real-full
CCR_LIVE_REAL_MATRIX=1 make test-live-matrix-full
```

Also record pull-request CI and the 12-job fixture matrix:

- Linux and macOS
- Claude Code pinned and latest
- `openai-chat`, `anthropic-native`, and `openai-responses` fixtures
- Separate macOS helper protocol-fixture and source-build job

## Required Draft Verification

- Release archives exist for Linux/macOS amd64/arm64.
- `checksums.txt` verifies downloaded archives.
- GitHub provenance attestation verifies against `release.yml`.
- The GHCR browser image tag exists and its keyless cosign signature verifies.
- Linux and macOS archive smoke tests pass.
- macOS helper text remains source-built unsigned preview wording and does not
  claim Homebrew or archive packaging.

## Promotion Gate

Promote only stable `vX.Y.Z` tags. Manual promotion publishes the GitHub Release
and then updates Homebrew. The Homebrew formula must point at the promoted tag
and match an independently downloaded source archive SHA-256.
