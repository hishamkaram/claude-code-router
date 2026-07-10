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

`ccr doctor` checks the local SQLite state, secret backend, and the installed
Claude Code binary. Install and sign in to Claude Code before launching a
first-party route.

## First External Provider

This example uses OpenRouter. It stores only the environment-variable reference
in CCR's database, not the value of `OPENROUTER_API_KEY`.

```bash
export OPENROUTER_API_KEY='replace-with-your-key'

ccr init
ccr provider add openrouter --api-key-env OPENROUTER_API_KEY
ccr provider test openrouter
ccr provider discover-models openrouter
ccr provider import-models openrouter --all
ccr model list
```

`discover-models` shows what the provider currently exposes. `import-models
--all` creates conflict-safe CCR aliases for every discovered model. You can
instead add one explicit alias:

```bash
ccr model add coding-model --provider openrouter --model <provider-model-id>
ccr model test coding-model
```

## Launch Claude Code

Start on a known alias:

```bash
ccr launch --model coding-model
```

CCR passes ordinary Claude Code options and prompts through unchanged. For
example, enable Claude in Chrome while starting on a CCR model:

```bash
ccr launch --model coding-model --chrome
```

CCR reserves `--model`, `--auth-mode`, `--permission-mode`, `--print`/`-p`, and
`--db`. Use `ccr launch --help` for CCR help, or `ccr launch -- --help` for
underlying Claude Code help without starting CCR. CCR also rejects options that
would override its selected model, generated model allowlist, or tool-safety
restrictions. It also rejects `--fallback-model` and `--bg`/`--background`,
which would bypass the selected route or outlive CCR's local gateway.
For a tool-disabled route, CCR also rejects `--tools`, `--mcp-config`,
`--plugin-dir`, and `--plugin-url`.

If exactly one routable alias exists, `ccr launch` selects it automatically.
With zero or multiple aliases, Claude Code starts on its configured default model
until you select another option.

Inside the running Claude Code session, use `/model` and choose a `CCR <alias>`
entry to route future work in that session. New agents and workflows use the
active route where it is safe; existing workers can remain on their spawn-time
model.

Some organizations restrict the Claude Code model picker. CCR cannot override
that policy. Use a permitted default model or launch with a configured CCR alias
when the policy allows it.

## First-Party Anthropic Routes

Use the default authentication mode to preserve a Claude Code subscription login
or Anthropic API-key authentication for ordinary first-party Claude model names:

```bash
ccr launch --auth-mode preserve
```

The local gateway adds a temporary CCR session header but does not replace the
original first-party credentials in this mode. See
[routing and authentication](routing.md#authentication-modes) for the
third-party-only `gateway-token` mode.

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
