# Providers and Model Aliases

A provider is a connection definition. A model alias is the stable name you see
as `CCR <alias>` in Claude Code's `/model` picker and can also pass to
`ccr launch --model <alias>`.

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
