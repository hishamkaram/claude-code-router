# claude-code-router

Private Go implementation of a local Claude Code router.

`ccr` launches Claude Code through a fixed local gateway and lets engineers
switch configured model aliases inside the same Claude Code session with
`/model <alias>`.

## Current Status

Bootstrap foundation:

- Go CLI skeleton with strict validation and clear help.
- SQLite local state for providers and model aliases.
- API keys stored as environment references or OS keychain references, never raw
  SQLite values.
- Agent/skill guidance for maintainable implementation.
- Live Claude Code E2E contract and test harness skeleton.

Provider routing and the runtime gateway are intentionally not implemented in
the bootstrap commit.

## CLI Examples

```bash
ccr init
ccr provider add openrouter --api-key-env OPENROUTER_API_KEY
ccr provider add litellm --base-url http://localhost:4000 --no-api-key
ccr model add qwen --provider openrouter --model qwen/qwen3-coder
ccr model list
ccr doctor
```

Direct API-key entry is supported through stdin and OS keychain storage:

```bash
printf '%s' "$ANTHROPIC_API_KEY" | ccr provider add anthropic --api-key-stdin
```

## Safety Rules

- No silent fallback to Claude.
- No raw API keys in SQLite, logs, errors, tests, or docs.
- Degrade external-model compatibility visibly when safe.
- Reject unsafe requests clearly.
- Significant router behavior must be proven with live Claude Code E2E tests.

## Development

```bash
make build
make test
make check
make test-live
```
