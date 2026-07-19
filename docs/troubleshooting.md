# Troubleshooting

Start with local diagnostics:

```bash
ccr doctor
ccr status
ccr trace --since 30m
ccr provider list
ccr model list
```

## Claude Code Is Unavailable

`ccr doctor` reports whether the `claude` binary is available. Install Claude
Code, ensure it is on your `PATH`, then sign in before using first-party routes.

## A Provider Cannot Connect

Validate the configured provider and model separately:

```bash
ccr provider test <provider>
ccr model test <alias>
```

Check that the API-key environment variable is present in the shell that starts
CCR, or that the configured key file is a regular file with mode `0600`. Do not
print the key while debugging.

Use bounded live diagnostics when configuration checks pass but routing fails:

```bash
ccr doctor --live
ccr doctor --live --all
ccr conformance run <alias>
ccr conformance run --all
```

## The Desired Model Is Missing from `/model`

Run:

```bash
ccr model list
ccr status
ccr launch
```

A normal no-model launch preserves Claude Code's startup model and subscription
authentication, then adds configured, non-blocked aliases that are safe for a
tools-enabled session to the visual picker. It also prints their model IDs:

```text
/model anthropic.ccr.<alias>
```

An alias with a one-million-token effective context window is printed as:

```text
/model anthropic.ccr.<alias>[1m]
```

You can switch back to `opus`, `sonnet`, or another subscription model in the
same session. If an alias is absent, check that it is not `blocked`, that its
provider still exists, and that the provider protocol is Anthropic-compatible
or OpenAI-compatible. If it is `chat-only` or its provider mode disables tools,
start directly with `ccr launch --model <alias>`.

Claude Code organization policy can still restrict the model picker. CCR cannot
bypass that policy; use an allowed default model or ask the organization
administrator to permit the needed model option.

`--auth-mode gateway-token` requires `--model <alias>` and lets Claude Code
authenticate to CCR's `/v1/models` endpoint for discovery metadata. That mode
intentionally disables the original subscription and API-key authentication;
do not use it when first-party subscription routes must remain available.
This includes current Claude Code auto-mode safety classification for some
Agent and Workflow actions. Use `--auth-mode preserve`; CCR does not reroute or
bypass a safety classifier that cannot reach its required Anthropic model.

## The Picker Shows the Wrong Context Window

Inspect the effective value and where it came from:

```bash
ccr model show <alias> --json
ccr model refresh <alias>
```

If discovery is unavailable or incorrect, set a reviewed local override:

```bash
ccr model update <alias> --context-window 1000000
```

Relaunch Claude Code after changing capabilities because the picker allowlist is
created once per launch. Context below one million tokens intentionally has no
`[1m]` suffix. Use `--context-window 0` to clear the override.

## Doctor Reports a Live Failure

Doctor now prints the failed check, failure kind, safe gateway/provider HTTP
statuses, and an `action:` command. A provider control row such as
`all-proxy-models` is not a chat model; remove the alias using the exact command
Doctor prints. Authentication and provider HTTP failures point to
`ccr provider test <provider>`, while missing models point to fresh discovery.
Provider response bodies and credentials are never included in the diagnosis.

## CCR Starts on an Unexpected Model

Without `--model`, CCR intentionally leaves Claude Code on its configured
startup model. Use `/model` to switch after launch, or pass an explicit startup
alias:

```bash
ccr launch --model <alias>
```

Confirm the actual route from another terminal with `ccr status` or
`ccr trace --follow`. Generated text claiming a model identity is not routing
evidence.

## Sessions or Agents Are Missing

Run `ccr sessions` and inspect the launch's observation state. CCR injects
launch-only lifecycle hooks while preserving existing hooks, but managed policy
may block the injected HTTP callbacks. Such a launch is reported as unobserved.
It is not represented as an observed session with zero agents.

`--no-lifecycle` deliberately disables this state. `--no-history` only disables
route history; it does not disable lifecycle observation. An unfinished session,
agent, or task is marked abandoned when Claude Code exits abruptly.

## The CCR Status Line Is Missing

CCR does not replace an existing Claude Code status line. Run `ccr status` to
inspect the same route state outside Claude Code. Also check that the launch was
not started with `--no-statusline`.

## Remove Runtime History

Route and lifecycle history is redacted and bounded, but can be removed
explicitly:

```bash
ccr trace purge --all --yes
```

Start a one-off launch with `--no-history` when no route events should be
persisted. Prompts, responses, tool arguments, transcript paths, raw hook bodies,
authorization headers, and provider secret values are never part of history.

## `/compact` Does Not Reduce Context on an OpenAI-Compatible Alias

Claude Code compaction uses Anthropic `context_management` edits. CCR rejects
compaction edits on OpenAI-compatible routes instead of silently forwarding the
pre-compact transcript, because that would make the selected provider keep
seeing stale context. Use a first-party or Anthropic-compatible route for
sessions where `/compact` must work.

## First-Party Subscription Authentication Fails

Use the default `--auth-mode preserve` and verify the ordinary `claude` CLI is
signed in. `gateway-token` intentionally disables original Anthropic
subscription and API-key authentication, so it cannot use a first-party route.

## Web Search Reports No Results

`WebSearch` and `WebFetch` are run by the Claude Code host, not by CCR or the
selected model provider. A zero-result search or a site fetch failure is usually
a host-service or target-site result. Configure a dedicated MCP search provider
when you need separate search behavior.

## Reset or Isolate Local State

`ccr init` prints the database path. Use `--db` to isolate a test or a separate
configuration without deleting your normal state:

```bash
ccr --db /tmp/ccr-test.db init
ccr --db /tmp/ccr-test.db provider list
```

Back up a database before manually removing it. The database contains provider
and session metadata, but not raw provider credentials.
