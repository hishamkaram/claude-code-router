# Changelog

All notable CCR release changes are recorded here. Release acceptance evidence
lives under `docs/acceptance/`.

## Unreleased

## v0.4.0

- Add local Claude subscription account pools with keychain-backed account
  management, process-bound selection, visible 429 cooldown/relaunch behavior,
  launch observability, and fixture/live verification.
- Add strict account import, inspection, refresh, enable/disable, test, and
  removal commands with redacted human and JSON output.
- Keep pool exhaustion safe and visible: eligible interactive launches restart
  with the next account, while unsupported launch shapes fail without silently
  falling back to another identity or provider.

## v0.3.0

- Clarifies capability truth sources: explicit overrides, provider discovery,
  and recognized provider-model hints.
- Preserves the provider Responses capability in team-profile schema v3.
- Documents registered model picker behavior, vision gating, computer-use
  boundaries, no-silent-fallback handling, local approval/audit privacy, and
  30-day metadata retention.
- Adds user-facing distinctions for Docker browser image, trusted host browser,
  external computer-use executor, and source-built unsigned macOS helper
  preview.
- Clarifies that managed CUA requires an OpenAI Responses-capable provider and
  a route with effective Responses plus computer-use support, while direct
  first-party Anthropic CUA remains client-managed.
- Rejects ambiguous OpenAI Responses tool sets that combine native computer use
  with a function tool also named `computer`.
- Updates release automation to publish draft GitHub Releases first, attach
  checksums and provenance, publish and sign a GHCR browser image, and require
  manual promotion before Homebrew updates.

## v0.2.1

- Registered CCR aliases appear in Claude Code's `/model` picker beside allowed
  first-party models.
- Added normalized model capability discovery, manual overrides, refresh/show
  commands, all-model conformance, and all-model live Doctor.
- Preserved no-silent-fallback behavior for malformed picker IDs and unsupported
  capabilities.

## v0.2.0

- Added runtime route visibility, lifecycle tracking, redacted trace history,
  conformance checks, team profiles, and bounded local metadata retention.

Older release notes are available on GitHub Releases.
