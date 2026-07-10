# Routing, Authentication, and Compatibility

## Route Selection

CCR handles every request through one fixed local Anthropic-compatible gateway.
For each request, it uses this precedence:

1. An exact configured alias or `claude-ccr-<alias>` discovery ID routes to
   that alias's provider.
2. A standard first-party Claude model name routes to Anthropic.
3. An otherwise unmatched request can use the selected launch alias when one
   was configured for the session.
4. If no safe route exists, CCR returns an explicit error.

Because exact aliases take precedence, do not name a third-party alias `sonnet`,
`opus`, `haiku`, or another first-party model identifier unless you intentionally
want it to override that route.

## Authentication Modes

### `preserve` (default)

```bash
ccr launch --auth-mode preserve
```

CCR adds a temporary local session token through `ANTHROPIC_CUSTOM_HEADERS`.
For first-party Anthropic routes it preserves an existing Claude Code
subscription login or Anthropic API-key authentication. This is the normal mode
when a session can use both first-party and external routes.

### `gateway-token`

```bash
ccr launch --auth-mode gateway-token --model coding-model
```

CCR uses only its generated local gateway token. Original Anthropic subscription
and API-key authentication are deliberately disabled, so this mode requires a
routable CCR alias. Use it for an external-provider-only session.

## Model Switching and Workers

Use `/model` in Claude Code to choose an available `CCR <alias>` option. Future
work in the same session uses that route where safely possible. Subagents,
workflow agents, and teammates created after a switch inherit the active model
where Claude Code permits it. Existing workers can remain on the model used when
they were created; CCR does not hide that fact.

## Capability Handling

CCR translates between Anthropic-compatible and OpenAI-compatible provider
protocols. It checks a provider's declared capabilities before forwarding tool
use, streaming, thinking, model discovery, and token-count operations.

- Safe limitations are surfaced in launch and response metadata.
- Chat-only routes disable Claude Code tools.
- Unsupported or unsafe requests fail with a clear error.
- CCR never silently falls back to Claude or another provider.

Built-in `WebSearch` and `WebFetch` are Claude Code host tools. CCR forwards the
model's tool protocol but cannot redirect those host-owned web operations to a
model provider. Use a custom MCP search tool when you need a different web-search
backend.

## Automation Modes

CCR enables Claude Code's gateway model discovery, auto-mode support, and deferred
tool search only when the selected route can use tools. Choose the desired Claude
Code permission mode explicitly:

```bash
ccr launch --model coding-model --permission-mode auto
```

Supported values are `default`, `manual`, `acceptEdits`, `plan`, `auto`,
`dontAsk`, and `bypassPermissions`. Your organization policy and Claude Code
settings can still restrict what is available.
