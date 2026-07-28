# Changelog

All notable CCR release changes are recorded here. Release acceptance evidence
lives under `docs/acceptance/`.

## Unreleased

## v0.4.3

- Preserve existing Claude Code status-line behavior in subscription-pool
  launches through a launch-only credential-isolation wrapper, exposing only
  the selected local label through `CCR_CLAUDE_ACCOUNT`; Windows uses a visible
  CCR status-line fallback.
- Keep generated settings and existing status-line commands out of process
  arguments through a private temporary settings file, and honor explicit
  higher-precedence `statusLine: null` overrides.
- Move subscription account auth into gateway memory; Claude receives only a
  generated loopback credential and never inherits a pooled OAuth token.
- Transparently retry confirmed account-wide quota rejections with the next
  eligible account while preserving the same Claude process, PID, session,
  pending request, tools, browser connection, and gateway.
- Coalesce concurrent stale quota responses by account generation, close
  rejected response bodies before retry, and preserve Anthropic's original 429
  when no replacement is usable.
- Re-admit accounts when their persisted cooldown is cleared or expires during
  a long-running Claude session.
- Keep model-specific, unknown, token-count, and ambiguous 429 responses out of
  account cooldown and rotation.
- Make injected and preserved status lines resolve the active account from the
  gateway after rotation, while keeping OAuth and observer credentials isolated.
- Enforce provider-secret namespaces before keychain resolution so a corrupted
  provider reference cannot resolve a Claude account OAuth credential.
- Avoid issuing observer capabilities when lifecycle and status observation are
  disabled, and keep account-transition warnings visible under notice bursts.
- Add deterministic fixture and real Claude PTY coverage for same-process
  rotation and all-accounts-limited continuity.

## v0.4.2

- Make subscription-pool status lines account-aware and report
  `limits=unknown` instead of presenting shared-profile quota as selected-account
  data; existing user settings remain unchanged.
- Classify rejected Anthropic limits by representative quota claim and fallback
  availability, retaining long cooldowns only for account-wide exhaustion and
  bounding model or unclassified cooldowns to five minutes.
- Add `ccr claude-account test --all --live` for advisory per-account quota
  windows and non-reversible identity fingerprints, including duplicate-login
  detection.
- Clear legacy unclassified `rate_limited` cooldowns automatically during the
  schema v8 migration so the next request records the precise failure class.
- Let the required real provider matrix use an exact registered account through
  `CCR_LIVE_REAL_CLAUDE_ACCOUNT`.
- Support automatic confirmed-quota rotation for interactive
  `--resume <session-id>` launches, optionally with a named `--worktree`, while
  preserving the original continuity arguments.
- Print whether automatic account rotation is enabled for each pool launch and
  why unsupported launch shapes do not rotate.

## v0.4.1

- Distinguish confirmed Anthropic unified quota rejection from temporary or
  ambiguous HTTP 429 responses before cooling and rotating subscription
  accounts.
- Prefer Anthropic's unified reset timestamp for confirmed quota cooldowns and
  exclude token-count throttles from account exhaustion handling.
- Add `ccr claude-account clear-cooldown <name>` and `--all` recovery commands
  that preserve account credentials, expiry, and enablement.

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
