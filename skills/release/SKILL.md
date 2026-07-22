---
name: release
description: "Use when preparing, validating, publishing, promoting, or recovering claude-code-router releases, including CI readiness, live E2E evidence, changelog/release-note updates, draft GitHub releases, GHCR browser images, and Homebrew promotion."
---

# CCR Release

Use this skill for version-neutral CCR release work. Keep claims tied to
evidence; a skipped live gate is not a release pass.

## Required Reading

Read these repo files before non-trivial release work:

- `AGENTS.md`
- `agents/_data/ai-agent-quality-contract.md`
- `agents/_data/engineering-quality-gate.md`
- `agents/_data/live-e2e-contract.md`
- `agents/_data/router-product-invariants.md`
- `agents/_data/cli-contract.md`
- `agents/docs-updater.md`

When changing release mechanics, also read:

- `docs/releasing.md`
- `.github/workflows/release.yml`
- `.goreleaser.yaml`
- `scripts/publish-homebrew-formula.sh`

## Workflow

1. Confirm the target version and scope. Use `vX.Y.Z` placeholders in reusable
   docs and the exact tag only in versioned release notes or acceptance files.
2. Update `CHANGELOG.md`, `docs/releases/vX.Y.Z.md`, and
   `docs/acceptance/vX.Y.Z.md`. Do not document unimplemented behavior as
   shipped. Wording may describe expected release behavior only when the
   acceptance checklist still requires evidence before publication.
3. Run local gates in the order described in `docs/releasing.md`. Read
   `references/gates.md` when deciding whether evidence is sufficient.
4. Tag only after the acceptance file records reproducible pre-tag evidence.
5. Let the tag workflow create a draft GitHub Release, checksums, provenance
   attestations, and signed GHCR browser image. Homebrew must wait.
6. Verify the draft release assets and image, then manually promote the release
   workflow. Promotion publishes GitHub Release and updates Homebrew.
7. Keep macOS helper wording source-built and unsigned; do not claim it is
   included in Homebrew, GoReleaser archives, signed, notarized, or hardened.
8. Append immutable post-publication evidence to the acceptance file.

## Recovery

For partial draft/public release failures, read `references/recovery.md` before
changing tags, releases, or Homebrew. Prefer idempotent reruns over manual
mutation.

## Templates

Use these assets when drafting release artifacts:

- `assets/release-notes-template.md`
- `assets/acceptance-template.md`
- `assets/pr-body-template.md`
