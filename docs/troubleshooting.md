# Troubleshooting

Start with local diagnostics:

```bash
ccr doctor
ccr status
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

## The Desired Model Is Missing from `/model`

Run:

```bash
ccr model list
ccr status
ccr launch
```

A normal no-model launch preserves Claude Code's startup model and subscription
authentication, then prints direct commands for configured, non-blocked aliases
that are safe for a tools-enabled session:

```text
/model claude-ccr-<alias>
```

Current Claude Code does not auto-populate gateway aliases in the visual picker
while the saved claude.ai login remains active. Direct selection still works and
you can switch back to `opus`, `sonnet`, or another subscription model in the
same session. If a direct alias fails, check that it is not `blocked`, that its
provider still exists, and that the provider protocol is Anthropic-compatible or
OpenAI-compatible. If it is `chat-only` or its provider mode disables tools,
start directly with `ccr launch --model <alias>`.

Claude Code organization policy can still restrict the model picker. CCR cannot
bypass that policy; use an allowed default model or ask the organization
administrator to permit the needed model option.

`--auth-mode gateway-token` requires `--model <alias>` and lets Claude Code
authenticate to CCR's `/v1/models` endpoint, so aliases can appear in the visual
picker. That mode intentionally disables the original subscription and API-key
authentication; do not use it when first-party subscription routes must remain
available.

## CCR Starts on an Unexpected Model

Without `--model`, CCR intentionally leaves Claude Code on its configured
startup model. Use `/model` to switch after launch, or pass an explicit startup
alias:

```bash
ccr launch --model <alias>
```

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
