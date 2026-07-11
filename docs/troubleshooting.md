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

A normal no-model launch should expose configured, non-blocked aliases that are
safe for a tools-enabled session in `/model` as `CCR <alias>` while preserving
Claude Code's normal startup model. If an alias is missing, check that it is not
`blocked`, that its provider still exists, and that the provider protocol is
Anthropic-compatible or OpenAI-compatible. If the alias is `chat-only` or its
provider mode disables tools, start directly with `ccr launch --model <alias>`.

Claude Code organization policy can still restrict the model picker. CCR cannot
bypass that policy; use an allowed default model or ask the organization
administrator to permit the needed model option.

`--auth-mode gateway-token` requires `--model <alias>`, so it is not the right
mode for checking alias visibility on a default-startup launch.

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
