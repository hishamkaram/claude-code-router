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
aliases before saving. For non-discoverable profiles, CCR validates the provider
config and credential resolution, then offers a manual repeating model form.

New imported aliases default to `degraded`. CCR never promotes compatibility
automatically.

## Launch Claude Code

Launch once through CCR:

```bash
ccr launch
```

Without `--model`, Claude Code starts on its normal configured model. CCR passes
an ephemeral allowlist that adds configured, non-blocked aliases to the visual
`/model` picker beside the permitted Anthropic models. Subscription or API-key
authentication remains available for first-party routes. Launch output also
prints each `/model anthropic.ccr.<alias>` ID for scripted selection.

Pass ordinary Claude Code options after `launch`:

```bash
ccr launch --chrome
```

Start directly on one CCR alias only when you want that alias to be the startup
model:

```bash
ccr launch --model coding-model
```

CCR reserves `--model`, `--auth-mode`, `--permission-mode`, `--print`/`-p`, and
`--db`. Use `ccr launch --help` for CCR help, or `ccr launch -- --help` for
underlying Claude Code help without starting CCR. CCR rejects options that would
override its selected model, generated model allowlist, or tool-safety
restrictions.

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
```

## Multiple Providers

You can configure several providers and many aliases before launching. A normal
`ccr launch` preserves Claude Code's default startup model and adds every
non-blocked, tool-compatible alias to `/model`. Use the picker or the printed
`/model anthropic.ccr.<alias>` IDs to switch providers in the same session.
Start directly with `--model <alias>` when an alias is `chat-only` or otherwise
requires tools to be disabled for the launch.

New agents and workflows use the active route where Claude Code permits it.
Existing workers can remain on their spawn-time model.

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

## Inspect Local State

```bash
ccr provider list
ccr model list
ccr status
ccr sessions
ccr agents
```

CCR uses a local SQLite database. `ccr init` prints its exact path. Override it
for an isolated test or separate workspace with `ccr --db /path/to/ccr.db ...`.
