# Claude Code Router

[![CI](https://github.com/hishamkaram/claude-code-router/actions/workflows/ci.yml/badge.svg)](https://github.com/hishamkaram/claude-code-router/actions/workflows/ci.yml)
[![License: MIT](https://img.shields.io/badge/License-MIT-green.svg)](LICENSE)

`ccr` is a local gateway for using Claude Code with first-party Anthropic models
and configured third-party model providers in one session. It keeps Claude Code
connected to one loopback-only gateway while routing each request to the selected
model safely and visibly.

CCR is built for users who want to keep Claude Code's normal workflow while
adding providers such as OpenRouter, Z.AI, LiteLLM, or a local OpenAI-compatible
endpoint. It never silently falls back to a different model or provider.

## Install

### Homebrew (macOS)

```bash
brew install hishamkaram/tap/claude-code-router
```

### GitHub Releases (macOS and Linux)

Download the archive for your operating system and CPU from the
[latest release](https://github.com/hishamkaram/claude-code-router/releases/latest),
verify it against `checksums.txt`, then place `ccr` on your `PATH`.

```bash
tar -xzf <downloaded-archive>.tar.gz
mkdir -p ~/.local/bin
install -m 755 ccr ~/.local/bin/ccr
ccr version
```

### Go

Install from source with Go 1.25 or newer:

```bash
go install github.com/hishamkaram/claude-code-router/cmd/ccr@latest
ccr version
```

## Requirements

- [Claude Code](https://code.claude.com/docs/en/overview) must be installed and
  available as `claude`.
- Sign in to Claude Code when you want to use first-party Anthropic routes.
- Set any external-provider credentials in environment variables, a `0600` key
  file, or the OS keychain. CCR never stores raw API keys in SQLite.

Run this after installation to check the local setup:

```bash
ccr doctor
```

## Quick Start

The guided path is the shortest way to add a provider, choose credentials, import
models, and review aliases before anything is saved.

```bash
ccr init
ccr provider add --interactive
ccr model list
ccr launch
```

`ccr launch` keeps Claude Code's normal startup model and preserves subscription
authentication. Current Claude Code does not auto-populate gateway aliases in
the `/model` picker while that login remains active, so CCR prints ready-to-use
commands such as `/model claude-ccr-<alias>` for every safe registered alias.
Those direct selections still route future work in the same session. To start
directly on one alias, including a `chat-only` alias that disables tools for the
launch, pass it explicitly:

```bash
ccr launch --model <alias> --chrome
```

### Scripted Setup

For automation, add providers and models without prompts:

```bash
export OPENROUTER_API_KEY='replace-with-your-key'

ccr provider add openrouter --api-key-env OPENROUTER_API_KEY
ccr provider test openrouter
ccr provider import-models openrouter --all
```

Or import with the searchable review flow:

```bash
ccr provider import-models openrouter
```

For providers without model discovery, add aliases explicitly:

```bash
ccr model add coding-model --provider openrouter --model <provider-model-id>
ccr model test coding-model
```

Your organization may restrict which Claude Code model options are available;
CCR reports that limitation instead of bypassing it.

## How Routing Works

1. CCR launches Claude Code through a loopback-only local gateway.
2. Default subscription-preserving launches print direct
   `/model claude-ccr-<alias>` commands for registered, non-blocked,
   tool-compatible aliases. Tool-disabled aliases are available by starting
   directly with `ccr launch --model <alias>`.
3. Standard first-party model names route to Anthropic. The default
   `--auth-mode preserve` keeps an existing Claude Code subscription login or
   Anthropic API-key authentication available for those routes.
4. CCR checks provider capabilities before a request is sent. Unsupported or
   unsafe behavior is rejected with an explanation; it is never redirected to
   Claude or another configured provider.

Model self-identification is generated text, not proof of routing. When an
OpenAI-compatible model is asked which model is active, CCR adds route context
so the answer can reflect the current alias and provider model instead of an
older turn from the same Claude Code session.

Use `ccr launch --auth-mode gateway-token --model <alias>` when you want a
third-party-only session. That mode intentionally disables the original
Anthropic subscription and API-key authentication, but allows Claude Code to
discover registered aliases from CCR's `/v1/models` endpoint for its picker.

## Common Commands

```bash
ccr provider list                 # show configured providers
ccr model list                    # show model aliases
ccr model test <alias>            # validate a route against its provider
ccr conformance run <alias>       # record compatibility checks
ccr launch                        # preserve subscription; print direct /model commands
ccr launch --model <alias>        # start directly on one CCR alias
ccr status                        # inspect local router state
ccr sessions                      # list launched sessions
ccr agents                        # list observed agents and workers
```

## Documentation

- [Getting started](docs/getting-started.md)
- [Providers and model aliases](docs/providers.md)
- [Routing, authentication, and compatibility](docs/routing.md)
- [Troubleshooting](docs/troubleshooting.md)
- [Maintainer release process](docs/releasing.md)

## Security and Local State

CCR stores provider configuration, model aliases, sessions, and compatibility
metadata in a local SQLite database. By default it uses
`$XDG_DATA_HOME/claude-code-router/ccr.db`, or
`~/.local/share/claude-code-router/ccr.db` when `XDG_DATA_HOME` is unset. Use
`--db <path>` to keep state elsewhere.

SQLite contains only secret references such as `env:OPENROUTER_API_KEY`, never
the API-key value. See [provider credential handling](docs/providers.md#credentials)
for the supported secret sources.

## Development

```bash
make build
make test
make check
make test-live
```

Live tests require a working, authenticated Claude Code installation. They may
skip when it is unavailable; skipped live tests are not equivalent to a verified
runtime route.

## Contributing and Security

Read [CONTRIBUTING.md](CONTRIBUTING.md) before opening a pull request. Report
security vulnerabilities privately as described in [SECURITY.md](SECURITY.md).

## License

MIT. See [LICENSE](LICENSE).
