# Routing, Authentication, and Compatibility

## Launch Model Visibility

`ccr launch` starts Claude Code through one fixed local Anthropic-compatible
gateway. When routable aliases exist, CCR passes an ephemeral Claude Code
allowlist for that launch:

- existing user `availableModels` entries are preserved;
- if no user allowlist exists, CCR includes known first-party Claude model IDs;
- every configured, non-blocked alias that is safe for the launch tool mode is
  added as `anthropic.ccr.<alias>`;
- `~/.claude/settings.json` is never written.

Without `--model`, Claude Code keeps its normal startup model. In the default
subscription-preserving mode, IDs beginning with `anthropic.` become custom
rows in the visual picker, so registered models appear beside the permitted
Anthropic models without authenticated gateway discovery. Pass `--model
<alias>` when you want that alias to be the startup model, or when it is
`chat-only` and needs tools disabled for the whole launch. See Claude Code's
[`availableModels` documentation](https://code.claude.com/docs/en/model-config).

Claude Code treats the strings `sonnet`, `opus`, and `haiku` inside custom IDs
as native model-family signals. CCR selectively percent-escapes those substrings
in picker IDs so native rows remain visible. For example, an alias named
`my-sonnet` appears as `anthropic.ccr.my-s%6fnnet`; the stored CCR alias remains
`my-sonnet`.

## Route Selection

For each request, CCR uses this precedence:

1. An exact configured alias, canonical `anthropic.ccr.<alias>` picker ID, or
   legacy `claude-ccr-<alias>` ID routes to that alias's provider.
2. A standard first-party Claude model name routes to Anthropic.
3. If no safe route exists, CCR returns an explicit error. Unknown or malformed
   CCR IDs never fall through to Anthropic or a startup alias.

Because exact aliases take precedence, do not name a third-party alias `sonnet`,
`opus`, `haiku`, or another first-party model identifier unless you intentionally
want it to override that route.

## Model Switching and Workers

Open `/model` and select the `anthropic.ccr.<alias>` row to use a registered
route. CCR also prints the exact ID for direct or scripted selection. Legacy
`claude-ccr-<alias>` IDs remain gateway-routable but are not placed in new
allowlists. Future work in the same session uses the selected route where safely
possible. Subagents, workflow agents, and teammates created after a switch
inherit the active model where Claude Code permits it. Existing workers can
remain on the model used when they were created; CCR does not hide that fact.

The next authenticated request after `/model` is the routing source of truth.
Its actual alias, provider model, protocol, capabilities, result, latency, and
provider-reported token usage appear in `ccr status`, `ccr trace`, and the CCR
status line. CCR does not infer a route from generated model self-identification.

## Runtime and Lifecycle Visibility

Each launch gets a normalized launch record. Launch-scoped Claude hooks observe
session, subagent, task, teammate-idle, stop-failure, and session-end events.
Abrupt exits mark unfinished observed work abandoned. Hook payloads are reduced
to identifiers, kinds, states, and bounded reasons before persistence; prompts,
task descriptions, tool arguments, transcript paths, and raw hook bodies are
not retained.

```bash
ccr status --json
ccr trace --follow
ccr sessions --active
ccr agents --active
```

The hook and status endpoints listen only on the launch gateway's loopback
address and require a separate ephemeral token. Existing user hooks are kept.
An existing status line wins over CCR's launch-only status line. The
`--no-history`, `--no-lifecycle`, and `--no-statusline` flags disable their
respective feature for one launch.

## Authentication Modes

### `preserve` (default)

```bash
ccr launch --auth-mode preserve
```

CCR adds a temporary local session token through `ANTHROPIC_CUSTOM_HEADERS`.
For first-party Anthropic routes it preserves an existing Claude Code
subscription login or Anthropic API-key authentication. This is the normal mode
when a session can use both first-party and external routes. Claude Code does
not run authenticated gateway discovery in this mode; CCR supplies registered
picker rows through the launch-only `availableModels` override instead.

### `gateway-token`

```bash
ccr launch --auth-mode gateway-token --model coding-model
```

CCR uses only its generated local gateway token. Original Anthropic subscription
and API-key authentication are deliberately disabled. Because a no-startup-model
session must preserve first-party auth, `gateway-token` requires an explicit CCR
startup alias. Claude Code can authenticate to `/v1/models` in this mode and use
its friendly discovery metadata. See Anthropic's
[gateway documentation](https://code.claude.com/docs/en/llm-gateway).

## Capability Handling

CCR translates between Anthropic-compatible and OpenAI-compatible provider
protocols. It checks a provider's declared capabilities before forwarding tool
use, streaming, thinking, model discovery, and token-count operations.

- Safe limitations are surfaced in launch and response metadata.
- Chat-only routes disable Claude Code tools.
- Unsupported or unsafe requests fail with a clear error.
- CCR never silently falls back to Claude or another provider.
- Observed token counts are recorded when supplied; monetary cost is not
  estimated.

Built-in `WebSearch` and `WebFetch` are Claude Code host tools. CCR forwards the
model's tool protocol but cannot redirect those host-owned web operations to a
model provider. Use a custom MCP search tool when you need a different
web-search backend.

## Automation Modes

CCR requests Claude Code gateway model discovery in `gateway-token` mode and
enables deferred tool search when the active route can use tools. Choose the
desired Claude Code permission mode explicitly:

```bash
ccr launch --model coding-model --permission-mode auto
```

Supported values are `default`, `manual`, `acceptEdits`, `plan`, `auto`,
`dontAsk`, and `bypassPermissions`. Your organization policy and Claude Code
settings can still restrict what is available.
