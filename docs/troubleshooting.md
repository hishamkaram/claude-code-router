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
authentication, then adds configured, non-blocked routable aliases that are safe for a
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

For LiteLLM, a warning such as `LiteLLM capability metadata unavailable: HTTP
403 Forbidden` means optional `/model/info` metadata was unavailable. If the
alias line says `refreshed`, the refresh still completed using discovery,
retained facts, and local overrides.

## Doctor Reports a Live Failure

Doctor now prints the failed check, failure kind, safe gateway/provider HTTP
statuses, and an `action:` command. Aliases excluded from routing, such as a
provider control row or a Responses-only alias whose provider lacks
`--responses`, are marked skipped and are not sent to the provider. They are
also excluded from `/model` and aggregate conformance. Authentication and
provider HTTP failures point to `ccr provider test <provider>`, while missing
models point to fresh discovery. Provider response bodies and credentials are
never included in the diagnosis.

## Conformance Fails on Forced Tool Choice

When model metadata leaves tool choice unknown, `ccr conformance run` probes it
instead of assuming support. A provider that ignores a forced tool request
causes the `forced_tool` check to fail and CCR does not silently change the
model's compatibility or capabilities.

Review the effective facts first:

```bash
ccr model show <alias> --json
```

If you have verified that the provider does not support forced tool choice,
record that limitation explicitly, rerun conformance, and relaunch Claude Code
before relying on the alias in `/model`:

```bash
ccr model update <alias> --tool-choice false
ccr conformance run <alias>
ccr launch
```

Use `--tool-choice auto` to clear the override after provider metadata or
capability support changes.

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
The default metadata retention window is 30 days and 10,000 combined route and
lifecycle events.

## Vision or Computer Use Is Rejected

Inspect the selected alias:

```bash
ccr model show <alias> --json
```

If the effective facts do not show image input or computer-use support, CCR
rejects the request before provider submission. Add a reviewed override only
when you have provider evidence. For managed computer use, also verify the
provider is OpenAI-compatible and Responses-capable, the model has effective
Responses and computer-use support, and the launch selected the intended
executor. OpenAI Responses computer use always requires that managed executor;
it is not delegated to Claude Code's client-managed tool loop. External managed
CUA requires a public HTTPS base URL with no
credentials, query, fragment, or redirects, plus
`--ccr-cua-external-token-env`. The macOS helper is source-built only, unsigned,
not packaged in Homebrew or release archives, and must be on `PATH` as
`ccr-cua-macos`. It requires Accessibility and Screen Recording permission for
the launching process; restart it after granting permissions. Prefer Docker for
reproducible local runs.

If a Responses provider returns `pending_safety_checks`, CCR returns a visible
rejection before running the executor. This prevents silent
`acknowledged_safety_checks` until those checks can be displayed in the approval
flow.

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

## Claude Subscription Pool Has No Usable Account

Inspect local account state:

```bash
ccr claude-account list
ccr claude-account show <name>
ccr claude-account test <name>
```

`list` and `show` report redacted metadata only. A status of `disabled`,
`expired`, or `cooldown` makes the account ineligible for automatic selection.
Enable or replace credentials explicitly:

```bash
ccr claude-account enable <name>
ccr claude-account refresh <name> --from current
claude setup-token
ccr claude-account refresh <name> --oauth-token-stdin
```

`--from current` works on Linux and Windows when the current Claude credentials
file exists and has safe permissions. On macOS it is unsupported because Claude
stores the active login in Keychain; use `claude setup-token` and
`--oauth-token-stdin`.

If every account is disabled, expired, cooling down, or has an unavailable
keychain credential, `ccr launch --auth-mode subscription-pool` fails visibly.
CCR does not silently fall back to your default Claude login or an Anthropic API
key.

## Subscription Pool Relaunch Did Not Happen

Automatic relaunch after a first-party Anthropic HTTP 429 only applies to a
plain interactive launch:

```bash
ccr launch --auth-mode subscription-pool
```

CCR does not automatically relaunch when you use `--print`, pass
`--claude-account`, configure managed CUA, or pass extra Claude Code arguments
or prompts. In those cases the selected account is marked cooling down and the
command returns a visible rate-limit error. Rerun the launch after choosing
another account or waiting for the cooldown:

```bash
ccr launch --auth-mode subscription-pool --claude-account other
```

When automatic relaunch does run, CCR stops the current Claude Code process and
starts a new one with `--continue`. It cannot swap identities inside an existing
Claude Code process.

## Remove a Claude Subscription Account

Removal deletes the account's OS keychain credentials before deleting its
SQLite metadata:

```bash
ccr claude-account remove <name> --yes
```

The `--yes` confirmation is required. If keychain cleanup fails, CCR retains the
metadata and refs so the cleanup can be retried.

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
