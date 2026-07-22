# Providers and Model Aliases

A provider is a connection definition. A model alias is the stable name used by
`ccr launch --model <alias>` and the matching `/model` picker row. Picker model
IDs use `anthropic.ccr.<alias>`. An effective context window of at least one
million tokens adds the terminal `[1m]` marker. In gateway-token sessions,
authenticated discovery can additionally supply the friendly `CCR <alias>`
display name.

Keeping providers and aliases separate lets you change a provider model without
changing workflows that use the alias.

## Guided Setup

Start with:

```bash
ccr provider add --interactive
```

The optional name form is still supported:

```bash
ccr provider add --interactive openrouter
```

When a name is supplied, it becomes an editable connection-name default and CCR
uses it to infer the initial profile when possible.

The wizard saves nothing until the final review completes. Cancellation before
save creates no provider or model records.

## Searchable Profile Catalog

The interactive picker is local and curated:

| Profile | Protocol | Discovery | Default mode | Base URL |
| --- | --- | --- | --- | --- |
| `anthropic` | Anthropic-compatible | Manual model entry | `full` | Built in |
| `zai` | Anthropic-compatible | Manual model entry | `full` | Built in |
| `zai-openai` | OpenAI-compatible | Searchable `/v1/models` | `degraded` | Built in |
| `openrouter` | OpenAI-compatible | Searchable `/v1/models` | `degraded` | Built in |
| `litellm` | OpenAI-compatible | Searchable `/v1/models` | `degraded` | Required |
| `local` | OpenAI-compatible | Searchable `/v1/models` | `degraded` | Required |
| `anthropic-compatible` | Anthropic-compatible | Manual model entry | `degraded` | Required |
| `openai-compatible` | OpenAI-compatible | Searchable `/v1/models` | `degraded` | Required |

Use `local` only for a trusted local endpoint. Generic profiles are for custom
endpoints where you know the wire protocol.

## Credentials

CCR stores a reference, never a raw key. Choose one supported source:

```bash
# Environment variable reference.
ccr provider add openrouter --api-key-env OPENROUTER_API_KEY

# Absolute path to a regular file with mode 0600.
ccr provider add litellm --base-url http://localhost:4000 \
  --api-key-file ~/.config/ccr/litellm.key

# Read once from stdin and store through the OS keychain.
printf '%s' "$ANTHROPIC_API_KEY" | ccr provider add anthropic --api-key-stdin

# Only for a provider that truly needs no credential.
ccr provider add local --base-url http://localhost:4000 --no-api-key
```

The interactive wizard offers the same choices: hidden keychain prompt,
environment-variable reference, `0600` key file, or explicit no-key mode. Raw
values stay in memory until keychain storage succeeds and are never stored in
SQLite.

Create a key file without exposing its contents in shell history:

```bash
mkdir -p ~/.config/ccr
install -m 600 /dev/null ~/.config/ccr/litellm.key
read -rsp 'Provider key: ' key; printf '\n'
printf '%s\n' "$key" > ~/.config/ccr/litellm.key
unset key
```

If keychain storage is unavailable, use an environment-variable or `0600` file
reference.

## OpenAI Responses API

Responses routing is an explicit provider capability because an
OpenAI-compatible endpoint does not necessarily implement `POST /v1/responses`.
Enable it when adding or updating a provider that has that endpoint:

```bash
ccr provider add litellm-full --type litellm --base-url https://gateway.example/v1 \
  --api-key-env LITELLM_API_KEY --responses

# Existing provider:
ccr provider update litellm-full --responses
```

Only OpenAI-compatible providers can advertise this capability. To route a
specific alias through Responses, set the model facts as well. Add
`--computer-use true` only when the provider model actually supports managed
computer use:

```bash
ccr model update responses-cua --model-kind responses \
  --responses true --computer-use true
```

CCR rejects a Responses or computer-use request when either the provider or the
effective model facts do not support it; it never falls back to Chat
Completions or Claude. A Responses-only alias whose provider is not configured
with `--responses` is excluded from gateway discovery and Claude Code's picker
instead of being offered as a route that would fail.

## Discovery and Review

OpenAI-compatible profiles with model discovery verify connectivity before
persistence and supply model choices:

```bash
ccr provider import-models openrouter
```

The default `import-models` flow is guided. It discovers models, opens a
searchable multi-select, then reviews each planned alias before saving. In the
review you can keep the generated alias, rename it, change compatibility, remove
a model, or resolve alias conflicts.

Use `--all` only for deterministic automation:

```bash
ccr provider import-models openrouter --all
```

`--all` imports every discovered model with generated aliases and skips aliases
that already exist.

CCR keeps only normalized capability fields from provider discovery. Standard
OpenAI-compatible `/v1/models` metadata is used where present; LiteLLM can add
safe `/model/info` fields. Provider implementation settings and raw response
bodies are not stored. Virtual control rows such as `all-proxy-models` and rows
explicitly identified as non-chat models are not importable or routable.

Refresh and inspect registered aliases with:

```bash
ccr model refresh code-review
ccr model refresh --all
ccr model show code-review
ccr model show code-review --json
```

A failed or partial refresh preserves the last complete facts. Explicit model
overrides take precedence over discovered values, which take precedence over a
recognized provider-model hint such as a terminal `[1m]` suffix. Unknown is
distinct from `false`; missing metadata does not invent support or a limitation.
For LiteLLM, an HTTP 403 from optional `/model/info` is reported as a provider
warning. The refresh can still succeed from discovery, retained facts, and local
overrides; the warning is not itself a failed alias refresh.

Use `model update` when the provider cannot report a capability accurately:

```bash
ccr model update code-review --context-window 1000000
ccr model update code-review --tools false --streaming true
ccr model update code-review --tools auto
ccr model update code-review --clear-capabilities
```

Boolean overrides accept `true`, `false`, or `auto`. `auto` clears that one
override. CCR also supports normalized input/output limits, modalities, tool
choice and parallel tools, thinking, prompt caching, system messages, vision,
PDF and audio input, audio output, response-schema support, and computer-use
support. Run `ccr model update --help` for the exact flags.

Vision has two checks: the request must contain an image modality that CCR can
translate, and the effective model facts must allow image input. If either check
fails, the gateway rejects the request with the missing capability. It does not
strip the image, retry text-only, or route to Claude.

## Manual Model Entry

Profiles without OpenAI-compatible discovery validate config and credential
resolution, then disclose that live routing remains unverified. The wizard then
offers a repeating manual model form:

```text
alias
provider model ID
compatibility
add another / finish / save provider only / cancel
```

Manual aliases can also be added later:

```bash
ccr model add code-review --provider openrouter --model <provider-model-id>
ccr model test code-review
ccr model list
```

## Compatibility Status

New imported aliases default to `degraded`. CCR never promotes status
automatically.

| Status | Meaning |
| --- | --- |
| `full` | CCR can use the configured provider capabilities without a known degradation. |
| `degraded` | CCR can route the model, but reports feature limitations where applicable. |
| `chat-only` | Claude Code tools are disabled for this route. |
| `blocked` | CCR refuses to route the alias and hides it from the launch allowlist. |

Use:

```bash
ccr model update code-review --compat full
ccr conformance run code-review
```

Provider capabilities also gate tools, streaming, thinking, model discovery, and
token counting. CCR reports safe degradation and rejects unsafe translations; it
does not silently change providers.

Model capabilities refine that provider-level contract. In gateway-token
sessions, CCR's authenticated `/v1/models` response exposes known standardized
input/output limits and image, PDF, structured-output, and thinking support.
Picker IDs also carry Claude Code's terminal `[1m]` context marker when
applicable. CCR still enforces every effective restriction at its gateway before
a provider request is sent, regardless of which metadata the installed Claude
Code version consumes.

## Computer Use

Computer-use automation is split by owner:

| Mode | Owner | Notes |
| --- | --- | --- |
| Client-managed CUA | Claude Code host | Direct first-party Anthropic routes only. Claude Code owns the browser/session and approval prompts. |
| Managed Docker browser | CCR executor | Uses the signed GHCR browser image published with a release. Prefer this for reproducible local automation. |
| Managed host browser | CCR executor | Uses a trusted browser on the host. It is local and explicit, but less isolated than Docker. |
| External executor | User-provided bridge | Requires a public HTTPS base URL, no URL credentials/query/fragment, no redirects, and a bearer token passed by environment-variable reference. |
| macOS helper preview | CCR helper | Source-built only, not in Homebrew or release archives. Unsigned preview; treat as experimental until signed and notarized. |

Managed CUA is never implicit. A route must effectively support computer use,
the provider must be OpenAI-compatible and configured as Responses-capable, and
the launch must choose a managed executor before CCR accepts native computer-use
actions. An OpenAI Responses computer request without that executor is rejected
before provider submission; CCR never emits an opaque provider action to Claude
Code. Direct first-party Anthropic CUA remains client-managed by Claude Code.
Approval decisions and audit metadata are stored locally; screenshots, page
text, prompts, responses, credentials, and raw action payloads are not retained.

The Docker executor defaults to the signed release image
`ghcr.io/hishamkaram/claude-code-router/browser:latest`. Pin a signed release
tag when reproducibility matters. Releases publish Linux `amd64` and `arm64`
image manifests. Chromium runs without its internal Linux
sandbox because CCR uses a non-root, read-only, no-new-privileges container;
Docker capability, filesystem, resource, and process limits are the isolation
boundary. Do not treat the managed browser as a credential-bearing desktop
session.

Docker and trusted-host browser executors preserve `CTRL`, `ALT`, `META`, and
`SHIFT` modifiers on pointer actions. The macOS preview explicitly rejects
pointer modifiers rather than performing a different action.

Safe external executor launch shape:

```bash
export CCR_CUA_EXTERNAL_TOKEN='<external-executor-token>'
ccr launch --model responses-cua \
  --ccr-cua-mode managed \
  --ccr-cua-executor external:browser \
  --ccr-cua-external-url https://executor.example/cua \
  --ccr-cua-external-token-env CCR_CUA_EXTERNAL_TOKEN
```

The token value is read from the named environment variable and is not passed to
the Claude Code child process.

For the macOS helper preview, build the helper yourself on macOS 14 or later
and put the resulting `ccr-cua-macos` binary on the `PATH` used to start CCR:

```bash
make build-cua-macos
```

Grant Accessibility and Screen Recording permission to the process that
launches the helper, then restart the helper before trying `macos-preview`.
Unsigned preview builds may need permissions granted again after rebuilds or
when the launching identity changes.

## Conformance and Diagnostics

Run the protocol matrix through CCR's production gateway path:

```bash
ccr conformance run code-review
ccr conformance run --all
ccr conformance list code-review --json
```

The matrix checks configuration, discovery where supported, text, streaming,
forced tools, thinking, token counting, cancellation, and sanitized errors.
Capability-disabled checks are reported as not applicable. A failed declared
capability does not silently change the model's compatibility setting.

`ccr conformance run --all` runs every non-blocked routable alias with bounded
provider concurrency, continues after individual failures, and returns nonzero
when any required alias fails. Provider control aliases and Responses-only
aliases whose provider lacks `--responses` are reported as skipped without a
provider request. `ccr doctor` is offline by default. Use `ccr doctor --live`
to probe one alias per provider or `ccr doctor --live --all` to probe every
routable alias and report excluded aliases as skipped. Live Doctor
failures identify the failed check, safe HTTP status details, bounded evidence,
and a command to run next.

## Team Profiles

`ccr profile export` creates deterministic provider and model configuration,
including normalized discovered capabilities and explicit overrides, for review
or team distribution. It excludes raw credentials, keychain identifiers, and
key-file paths. On import, bind each required provider to an environment variable
on the destination machine:

```bash
ccr profile import team.json --dry-run
ccr profile import team.json --credential openrouter=OPENROUTER_API_KEY
```

Unknown fields, unsupported schema versions, invalid URLs, and oversized files
are rejected before any database change. Existing provider or model names cause
the whole import to fail atomically.
