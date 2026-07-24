# Getting Started

CCR keeps one Claude Code session connected to a local gateway and lets you use
first-party Anthropic models or configured external model aliases. The gateway
only listens on loopback and exits when the launched Claude Code process exits.

## Install and Verify

Choose one installation method from the [README](../README.md#install), then
confirm that CCR and Claude Code are available:

```bash
ccr version
ccr doctor
```

`ccr doctor` checks the local SQLite state, secret backend, and installed Claude
Code binary. Install and sign in to Claude Code before launching a first-party
Anthropic route.

## First Provider

Use the guided wizard first:

```bash
ccr init
ccr provider add --interactive
```

The wizard starts with a searchable provider profile picker. It then asks for an
editable connection name, base URL when needed, and one credential choice:
hidden OS keychain entry, environment-variable reference, `0600` key file, or
explicit no-key mode.

For OpenAI-compatible profiles that support discovery, CCR verifies connectivity
with `/v1/models`, shows a searchable model multi-select, and lets you review
aliases before saving. Safe capability metadata is retained when supplied;
LiteLLM can augment it from `/model/info`. If LiteLLM returns HTTP 403 or
another optional `/model/info` failure, CCR reports a warning and may still
complete the refresh using `/v1/models`, retained discovery, and local
overrides. That warning is not, by itself, a failed refresh. For
non-discoverable profiles, CCR validates the provider config and credential
resolution, then offers a manual repeating model form.

New imported aliases default to `degraded`. CCR never promotes compatibility
automatically.

Capability facts are visible before launch:

```bash
ccr model show <alias> --json
```

The effective value for each capability is derived from explicit local
overrides first, then provider discovery, then recognized provider-model hints.
Unknown values remain unknown so you can decide whether to add a reviewed
override or keep the route degraded.

## Launch Claude Code

Launch once through CCR:

```bash
ccr launch
```

Without `--model`, Claude Code starts on its normal configured model. CCR passes
an ephemeral allowlist that adds configured, non-blocked routable aliases to the visual
`/model` picker beside the permitted Anthropic models. Subscription or API-key
authentication remains available for first-party routes. Launch output also
prints each `/model anthropic.ccr.<alias>` ID for scripted selection.
An effective context window of at least one million tokens is printed and shown
with a terminal `[1m]` marker. The picker allowlist is generated once per
launch; after changing aliases or capabilities, relaunch to refresh what Claude
Code shows in `/model`.

Pass ordinary Claude Code options after `launch`:

```bash
ccr launch --chrome
```

The launch adds redacted route history, lifecycle observation, and a compact CCR
status line without changing your settings files. Disable any one independently
for a sensitive or policy-constrained session:

```bash
ccr launch --no-history
ccr launch --no-lifecycle
ccr launch --no-statusline
```

CCR preserves existing Claude hooks and an existing status line. If managed
policy prevents lifecycle hooks, runtime commands report the launch as
unobserved rather than pretending that no agents or tasks ran.

Start directly on one CCR alias only when you want that alias to be the startup
model:

```bash
ccr launch --model coding-model
```

CCR reserves `--model`, `--auth-mode`, `--claude-account`,
`--permission-mode`, `--print`/`-p`, and `--db`. Use `ccr launch --help` for CCR
help, or `ccr launch -- --help` for underlying Claude Code help without starting
CCR. CCR rejects options that would override its selected model, generated model
allowlist, or tool-safety restrictions.

## Scripted Alternatives

For automation, add a provider and import all discoverable models without
prompts:

```bash
export OPENROUTER_API_KEY='replace-with-your-key'

ccr provider add openrouter --api-key-env OPENROUTER_API_KEY
ccr provider test openrouter
ccr provider import-models openrouter --all
```

For a guided model import on an existing provider:

```bash
ccr provider import-models openrouter
```

For manual aliases:

```bash
ccr model add coding-model --provider openrouter --model <provider-model-id>
ccr model test coding-model
ccr model refresh coding-model
ccr model show coding-model --json
```

For a provider that implements the OpenAI Responses API, configure the provider
explicitly with `--responses`, then set the alias facts with `ccr model update
<alias> --model-kind responses --responses true`. See
[provider configuration](providers.md#openai-responses-api) for managed
computer-use requirements.

## Multiple Providers

You can configure several providers and many aliases before launching. A normal
`ccr launch` preserves Claude Code's default startup model and adds every
non-blocked, routable, tool-compatible alias to `/model`. Use the picker or the printed
`/model anthropic.ccr.<alias>` IDs to switch providers in the same session.
Use the exact printed ID when it includes `[1m]` or selective family-name
escaping.
Start directly with `--model <alias>` when an alias is `chat-only` or otherwise
requires tools to be disabled for the launch.

New agents and workflows use the active route where Claude Code permits it.
Existing workers can remain on their spawn-time model.

Vision and computer-use requests are capability-gated like tools and thinking.
If a selected alias does not effectively support image input or computer use,
CCR returns a visible rejection instead of sending a partial request or falling
back to Claude. OpenAI Responses computer use also requires a launch with
`--ccr-cua-mode managed` and a supported `--ccr-cua-executor`; without one, CCR
rejects the request before provider submission. Direct first-party Anthropic CUA
remains client-managed by Claude Code.

## Authentication

Use the default authentication mode to preserve a Claude Code subscription login
or Anthropic API-key authentication for ordinary first-party Claude model names:

```bash
ccr launch --auth-mode preserve
```

`--auth-mode gateway-token` disables the original Anthropic credentials,
requires an explicit startup CCR alias, and enables authenticated `/v1/models`
discovery metadata:

```bash
ccr launch --auth-mode gateway-token --model coding-model
```

`--auth-mode subscription-pool` selects a registered local Claude subscription
account and injects that account's OAuth token into the Claude Code process:

```bash
ccr launch --auth-mode subscription-pool
ccr launch --auth-mode subscription-pool --claude-account personal
```

Register accounts with `ccr claude-account`. Each command exists as a supported
CLI surface:

```bash
ccr claude-account import personal --from current
claude setup-token
ccr claude-account import work --oauth-token-stdin
ccr claude-account list
ccr claude-account show personal
ccr claude-account test personal
ccr claude-account refresh personal --from current
ccr claude-account disable work
ccr claude-account enable work
ccr claude-account remove work --yes
```

Import and refresh require exactly one source: `--from current` or
`--oauth-token-stdin`. `--from current` reads the current Claude login on Linux
and Windows. On macOS, current-login import is unsupported because Claude keeps
that login in Keychain; use `claude setup-token` with `--oauth-token-stdin`
instead. `ccr claude-account test <name>` verifies that the keychain credential
resolves locally and does not make a network request. Terminal input for
`--oauth-token-stdin` is read without echo.

Automatic pool selection atomically selects and stamps the least recently used
enabled, unexpired, non-cooling account. The timestamp provides load balancing,
not an exclusive lifetime lease; overlapping launches may reuse accounts.
`--claude-account <name>` selects only that account. Account identity is fixed
for the Claude Code process; there is no in-process identity swap. If a plain
interactive pool launch hits a first-party Anthropic HTTP 429, CCR marks the
account cooling down, stops Claude Code, and relaunches with the next usable
account using `--continue`. This automatic
relaunch applies only when the launch has no `--print`, no `--claude-account`,
no managed CUA options, and no extra Claude Code arguments. If every account is
disabled, expired, cooling down, or has an unavailable credential, CCR fails
visibly instead of falling back to the default Claude login.

Claude subscription account pools are for local individual use. Teams,
automation, hosted tools, and third-party products should use Anthropic's
official API authentication instead of sharing or pooling personal subscription
logins.

## Inspect Local State

```bash
ccr provider list
ccr model list
ccr status
ccr trace --since 30m
ccr sessions --active
ccr agents --active
ccr claude-account list
```

Add `--json` to these inspection commands for stable, schema-versioned output.
With `ccr trace --follow --json`, each event is emitted as one versioned JSON
document. `ccr status` shows the latest observed route and launch auth mode,
including the selected Claude account for subscription-pool launches, while
`ccr trace --follow` follows new redacted route and lifecycle events. Use
`ccr trace purge --all --yes` when retained history is no longer needed.
Redacted route and lifecycle metadata is retained for 30 days and at most
10,000 combined events. Prompts, responses, tool arguments, screenshots,
transcript paths, raw hook payloads, authorization headers, and provider secret
values are not stored.

CCR uses a local SQLite database. `ccr init` prints its exact path. Override it
for an isolated test or separate workspace with `ccr --db /path/to/ccr.db ...`.

## Share Routing Configuration

Team profiles move provider and model definitions without moving credentials:

```bash
ccr profile export team.json
ccr profile import team.json --dry-run
ccr profile import team.json --credential openrouter=OPENROUTER_API_KEY
```

Import validates the complete profile before writing and fails atomically on a
name conflict. Credential bindings are always local to the importing machine.
