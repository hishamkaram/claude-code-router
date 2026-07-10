# Providers and Model Aliases

A provider is a connection definition. A model alias is the stable name you use
in `ccr launch --model <alias>` and select as `CCR <alias>` in Claude Code.
Keeping those separate lets you change a provider model without changing the
workflows that use the alias.

## Supported Provider Profiles

| Profile | Protocol | Typical use |
| --- | --- | --- |
| `anthropic` | Anthropic-compatible | Direct Anthropic API-key route |
| `zai` | Anthropic-compatible | Z.AI's Anthropic-compatible endpoint |
| `anthropic-compatible` | Anthropic-compatible | Another Anthropic-style endpoint |
| `openrouter` | OpenAI-compatible | OpenRouter model routing and discovery |
| `zai-openai` | OpenAI-compatible | Z.AI OpenAI-compatible endpoint |
| `litellm` | OpenAI-compatible | LiteLLM gateway or proxy |
| `local` | OpenAI-compatible | A trusted local endpoint without an API key |
| `openai-compatible` | OpenAI-compatible | Any compatible endpoint with an API key |

Profiles supply conservative defaults for protocol and capabilities. You can
override the endpoint with `--base-url` where the provider supports it.

## Credentials

CCR stores a reference, never a raw key. Choose one of these supported methods:

```bash
# Environment variable reference.
ccr provider add openrouter --api-key-env OPENROUTER_API_KEY

# Absolute path to a regular file with mode 0600.
ccr provider add litellm --base-url http://localhost:4000 \
  --api-key-file ~/.config/ccr/litellm.key

# Read once from stdin and store through the OS keychain.
printf '%s' "$ANTHROPIC_API_KEY" | ccr provider add anthropic --api-key-stdin

# Only for a provider that truly needs no credential, such as a trusted local proxy.
ccr provider add local --base-url http://localhost:4000 --no-api-key
```

Create a key file without exposing its contents in shell history:

```bash
mkdir -p ~/.config/ccr
install -m 600 /dev/null ~/.config/ccr/litellm.key
read -rsp 'Provider key: ' key; printf '\n'
printf '%s\n' "$key" > ~/.config/ccr/litellm.key
unset key
```

If keychain storage is unavailable, use an environment-variable or `0600` file
reference. Do not pass raw keys in command arguments or commit them to config.

## Add and Validate a Provider

```bash
ccr provider add zai --api-key-env ZAI_API_KEY
ccr provider test zai
ccr provider list
```

For an endpoint not covered by a preset, declare its wire protocol explicitly:

```bash
ccr provider add compatible-api \
  --protocol openai-compatible \
  --base-url https://provider.example/v1 \
  --api-key-env PROVIDER_API_KEY
```

Use `ccr provider add --interactive <name>` for a guided setup flow.

## Add and Discover Models

OpenAI-compatible providers can expose models through their model-discovery
endpoint:

```bash
ccr provider discover-models openrouter
ccr provider import-models openrouter --all
```

Or create a deliberate alias:

```bash
ccr model add code-review --provider openrouter --model <provider-model-id>
ccr model test code-review
ccr model update code-review --compat full
ccr model list
```

`ccr conformance run <alias>` records the compatibility checks performed for an
alias. Use it before depending on a model for tool-heavy work.

## Compatibility Status

Each model alias has a compatibility status:

| Status | Meaning |
| --- | --- |
| `full` | CCR can use the configured provider capabilities without a known degradation. |
| `degraded` | CCR can route the model, but reports feature limitations where applicable. |
| `chat-only` | Claude Code tools are disabled for this route. |
| `blocked` | CCR refuses to route the alias. |

Provider capabilities also gate tools, streaming, thinking, model discovery, and
token counting. CCR reports safe degradation and rejects unsafe translations;
it does not silently change providers.
