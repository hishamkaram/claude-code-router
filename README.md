# claude-code-router

Private Go implementation of a local Claude Code router.

`ccr` launches Claude Code through a fixed local gateway and routes the session
to a configured model alias selected at launch time.

## Current Status

Local CLI foundation:

- Strict provider/model management, including add, update, test, remove, and
  interactive guided flows.
- SQLite local state for providers, model aliases, launch sessions, observed
  agents, and conformance records.
- API keys stored as environment references or OS keychain references, never raw
  SQLite values.
- Protocol-based provider profiles for Anthropic-compatible and
  OpenAI-compatible providers, including separate Z.AI Anthropic and Z.AI
  OpenAI presets.
- OpenAI-compatible model discovery through `/v1/models` for LiteLLM,
  OpenRouter, Z.AI OpenAI, and local providers when the provider declares model
  discovery support.
- Loopback-only local gateway launch that injects `ANTHROPIC_BASE_URL`,
  `ANTHROPIC_AUTH_TOKEN`, and Claude gateway/simple-mode environment.
- Anthropic-compatible pass-through routing and OpenAI-compatible translation.
  `ccr launch --model <alias>` sets the default route for Claude Code's
  built-in model names; configured aliases requested by Claude Code are honored.
  When omitted, launch auto-selects only if one routable alias exists.
- Tool use, streaming, thinking, model discovery, and token counting are gated
  by visible provider capability metadata. Unsupported requests return explicit
  errors instead of falling back silently. Claude Code launch disables tools
  when the selected model or provider is chat-only.
- Live Claude Code availability tests plus a tagged end-to-end smoke using the
  installed Claude binary and a fake OpenAI-compatible provider. Remote live
  provider E2E remains unverified until run against real credentials.

Remote provider live E2E remains unverified until run against real credentials.

## CLI Examples

```bash
ccr init
ccr provider add openrouter --api-key-env OPENROUTER_API_KEY
ccr provider add zai --api-key-env ZAI_API_KEY
ccr provider add glm --protocol anthropic-compatible --base-url https://example.invalid --api-key-env GLM_API_KEY
ccr provider add litellm --base-url http://localhost:4000 --no-api-key
ccr provider test litellm
ccr provider update litellm --base-url http://localhost:5000
ccr provider remove litellm --yes
ccr model add qwen --provider openrouter --model qwen/qwen3-coder
ccr model test qwen
ccr conformance run qwen
ccr launch --model qwen
ccr sessions
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
