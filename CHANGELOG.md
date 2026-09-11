# Changelog

All notable CCR release changes are recorded here. Release acceptance evidence
lives under `docs/acceptance/`.

## Unreleased

## v0.5.1

- Wait for killed process-group members to exit before reporting job cleanup
  survivors on the native fallback path. Preserve the latest observed survivors
  when cleanup reaches its deadline; coverage remains explicitly partial.

## v0.5.0

- Add durable detached print-mode jobs with `launch --detach --prompt-file`,
  job-ID status and cancellation, private output logs, and explicit cleanup coverage.
- Use systemd user scopes on supported Linux hosts, with owner-held cancellation
  authority and a degraded native process-group path on macOS or unavailable systemd.
- Retire gateway-owned idle upstream connections before HTTP/2 health-probe
  expiry can fail the next turn or model selection. Preserve active-stream
  pings, shorter caller limits, and the no-replay policy.

## v0.4.14

- Allow Claude Code cache hints on translated Chat Completions and Responses
  routes even when the selected model does not support prompt caching. Preserve
  prompt content and report the omitted cache hints in the ignored-fields header;
  keep Anthropic passthrough capability validation unchanged.
- Cover manual and automatic compaction after switching from subscription-pool
  to a provider in one Claude Code session, including non-caching models.
- Synchronize the native-stream heartbeat regression test with observed events
  instead of relying on fixed sleeps.
- Report empty completed Chat Completions replies as provider errors instead of
  successful blank answers; preserve tool-only and token-limited responses.
- Give compatibility probes model-capped reasoning headroom and require real
  text and named tool calls instead of accepting empty or nominal responses.
- Sequence real-provider model-switch probes by completed turn, reject failed
  model selections, and unblock test input when launch fails or is canceled.

## v0.4.12

- Preserve image results from ordinary tools on Responses routes as multimodal
  function outputs instead of requiring a native computer tool. Mixed text and
  multiple images retain their order and original tool-call association.

## v0.4.11

- Consume the CCR separator in `ccr launch -- <Claude Code options>` so JSON,
  stream-json, and other Claude Code options take effect. Apply launch safety
  validation to these options, including CCR-owned computer-use flags.
- Keep CCR-owned options before the separator. Use a second `--` when passing
  literal prompt text beginning with a dash; help and version use the same
  argument normalization as regular launches.

## v0.4.10

- Stream OpenAI Chat Completions, OpenAI Responses, Anthropic-compatible, and
  managed computer-use provider responses incrementally through CCR instead of
  buffering a completed upstream body before emitting Anthropic SSE.
- Preserve Claude Code compaction accounting with a bounded pre-stream input
  estimate and a unique gateway message ID for every translated response.
- Keep SSE protocol behavior reliable across CRLF framing, delayed native
  `message_start` events, terminal usage chunks, tool-call arguments, and
  in-band provider failures.
- Record stream lifecycle, terminal phase, and token telemetry without logging
  provider secrets or silently falling back to Claude.

## v0.4.8

- Let `ccr launch --model <alias>` use configured providers when Claude
  subscription or API authentication is unavailable.
- Resolve the default launch authentication mode automatically: preserve
  detected Claude authentication, otherwise isolate Claude Code behind CCR's
  generated loopback credential for provider-only routing.
- Fail before gateway or database startup when neither Claude authentication
  nor an explicit CCR startup model is available, with actionable login and
  provider guidance.
- Add explicit `provider-only` launch mode, retain `gateway-token` as a legacy
  compatible spelling, and expose the resolved launch mode through launch
  summaries and `ccr doctor`.
- Detect macOS Keychain-backed Claude login state through structured
  `claude auth status` output so signed-out users receive provider-only routing.
- Remove inherited Anthropic and OAuth credentials from provider-only child
  processes, while preserving current side-by-side subscription and provider
  behavior for authenticated users.
- Add unit, persistence, environment-isolation, and live Claude Code coverage
  for authenticated, provider-only, and missing-auth launch paths.
- Keep live `/model` conformance compatible with Claude Code 2.1.258 display
  labels by requiring an affirmative local-command result and ordered route
  transitions instead of matching the rendered model label.

## v0.4.7

- Route Claude Code auto-mode safety classifier messages and token-count
  requests through the active CCR alias for each Claude session.
- Apply in-session `/model` changes to later classifier requests without
  leaking model selection across concurrent Claude sessions.
- Refuse silent first-party Anthropic fallback when an active alias cannot be
  routed, and expose dedicated classifier trace operations.
- Add direct gateway, protocol-matrix, concurrency, and live Claude Code
  coverage proving selected-provider classifier traffic with zero unintended
  first-party calls.

## v0.4.6

- Translate image-bearing Anthropic `tool_result` blocks on OpenAI-compatible
  Chat Completions routes by keeping tool responses text-only and surfacing
  extracted images in the trailing user message.
- Add direct gateway, live route-fixture, and live Claude Code MCP coverage for
  image tool results after an in-session `/model` switch.

## v0.4.5

- Accept Anthropic assistant text blocks that include `citations` when routing
  through OpenAI-compatible Chat Completions providers.
- Preserve assistant text while dropping unsupported citation metadata, and
  expose the degraded field through `X-CCR-Ignored-Anthropic-Fields`.
- Keep unknown assistant metadata and citations in user, system, and tool
  result content strict so unsupported input remains visible as an error.
- Add a live Claude Code model-switch fixture covering citation-bearing
  assistant history and the provider request shape.

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
