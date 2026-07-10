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

This example adds OpenRouter, imports its discoverable models, and launches
Claude Code with one selected alias.

```bash
export OPENROUTER_API_KEY='replace-with-your-key'

ccr init
ccr provider add openrouter --api-key-env OPENROUTER_API_KEY
ccr provider test openrouter
ccr provider discover-models openrouter
ccr provider import-models openrouter --all
ccr model list
ccr launch --model <alias>
```

Within Claude Code, open `/model` and select a CCR model option to switch future
work in that session. Your organization may restrict which Claude Code model
options are available; CCR reports that limitation instead of bypassing it.

## How Routing Works

1. CCR launches Claude Code through a loopback-only local gateway.
2. Registered model aliases appear as `CCR <alias>` in Claude Code's model
   picker and route to the configured provider.
3. Standard first-party model names route to Anthropic. The default
   `--auth-mode preserve` keeps an existing Claude Code subscription login or
   Anthropic API-key authentication available for those routes.
4. CCR checks provider capabilities before a request is sent. Unsupported or
   unsafe behavior is rejected with an explanation; it is never redirected to
   Claude or another configured provider.

Use `ccr launch --auth-mode gateway-token --model <alias>` when you want a
third-party-only session. That mode intentionally disables the original
Anthropic subscription and API-key authentication.

## Common Commands

```bash
ccr provider list                 # show configured providers
ccr model list                    # show model aliases
ccr model test <alias>            # validate a route against its provider
ccr conformance run <alias>       # record compatibility checks
ccr launch --model <alias>        # start Claude Code through CCR
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
