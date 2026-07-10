# Troubleshooting

Start with the local diagnostics:

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

Run `ccr model list` and launch with a known alias:

```bash
ccr launch --model <alias>
```

CCR exposes configured aliases through gateway model discovery. If your Claude
Code organization restricts model selection, CCR cannot bypass that policy. Use
an allowed default model or ask the organization administrator to permit the
needed model option.

## CCR Starts on an Unexpected Model

Without `--model`, CCR auto-selects only when exactly one routable alias exists.
With zero or multiple aliases, Claude Code uses its configured default. Pass
`--model <alias>` when you need a deterministic starting route.

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
