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
- Local Claude subscription account pools require individual Claude Code
  subscription accounts that you personally control. For teams, services, and
  third-party products, use Anthropic's official API authentication instead of
  pooling personal subscription logins.

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

`ccr launch` keeps Claude Code's normal startup model, preserves subscription
authentication, and adds safe registered aliases to the `/model` picker beside
the permitted Anthropic models. CCR also prints each picker ID, such as
`/model anthropic.ccr.<alias>`, for scripted selection. Models with an effective
context window of at least one million tokens are advertised with Claude Code's
terminal `[1m]` marker. Picker rows are computed once per launch; after
capability or alias changes, relaunch Claude Code to refresh the visible picker.
To start directly on one alias, including a `chat-only` alias that disables
tools for the launch, pass it explicitly:

```bash
ccr launch --model <alias> --chrome
```

### Local Claude Subscription Account Pool

`ccr claude-account` registers local Claude subscription accounts for
`ccr launch --auth-mode subscription-pool`. OAuth access and refresh tokens are
stored only in the OS keychain. SQLite stores account metadata and keyring
references; CLI output redacts those refs as `keyring:***`. Raw OAuth tokens are
never stored in SQLite or printed.

Import exactly one credential source. On Linux and Windows, `--from current`
reads the current Claude login from the local Claude credentials file. On macOS,
Claude stores the current login in Keychain, so `--from current` is unsupported;
run `claude setup-token`, then provide only the generated token to
`--oauth-token-stdin`.
`--oauth-token-stdin` stores an access token only, with unknown expiry, and does
not contact Anthropic. When stdin is a terminal, CCR reads the token without
echoing it.

```bash
ccr claude-account import personal --from current
claude setup-token
ccr claude-account import work --oauth-token-stdin
ccr claude-account list
ccr claude-account show personal
ccr claude-account test personal
ccr claude-account refresh personal --from current
ccr claude-account disable work
ccr claude-account enable work
ccr claude-account remove work --yes
```

Use the pool for a plain interactive launch:

```bash
ccr launch --auth-mode subscription-pool
ccr launch --auth-mode subscription-pool --claude-account personal
```

Automatic pool selection atomically selects and stamps the least recently used
enabled, unexpired, non-cooling account. This is load balancing, not an
exclusive lifetime lease; overlapping launches can reuse an account after each
eligible account has been selected. An explicit `--claude-account` selects only
that account and never rotates to another one. Claude Code account identity is fixed
for the launched process; CCR does not swap identity inside a running session.
If a first-party Anthropic route returns HTTP 429 during a plain interactive
pool launch, CCR marks that account cooling down, stops Claude Code, and
relaunches with the next account using `--continue`. That automatic relaunch is
only for `ccr launch --auth-mode subscription-pool` without `--print`, without
`--claude-account`, without managed CUA options, and without extra Claude Code
arguments. Other launches fail visibly and tell you to rerun after selecting
another usable account.

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
2. Default subscription-preserving launches add registered, non-blocked,
   routable, tool-compatible aliases to the `/model` picker. Tool-disabled aliases are
   available by starting directly with `ccr launch --model <alias>`.
3. Standard first-party model names route to Anthropic. The default
   `--auth-mode preserve` keeps an existing Claude Code subscription login or
   Anthropic API-key authentication available for those routes.
4. CCR checks provider capabilities before a request is sent. Unsupported or
   unsafe behavior is rejected with an explanation; it is never redirected to
   Claude or another configured provider.

Capability truth is explicit and inspectable. Effective values come from, in
order, local overrides, provider discovery, and recognized provider-model hints.
Unknown stays unknown; CCR does not reinterpret missing metadata as support or
non-support. Vision is allowed only when the effective model capability says the
route supports image input. Unsupported image, PDF, audio, structured-output,
tool, thinking, or computer-use requests fail before provider submission.

Model self-identification is generated text, not proof of routing. When an
OpenAI-compatible model is asked which model is active, CCR adds route context
so the answer can reflect the current alias and provider model instead of an
older turn from the same Claude Code session.

Use `ccr launch --auth-mode gateway-token --model <alias>` when you want a
third-party-only session. That mode intentionally disables the original
Anthropic subscription and API-key authentication. It also lets Claude Code
authenticate to CCR's `/v1/models` endpoint for friendly discovery metadata.
Current Claude Code auto mode may require first-party Anthropic access for its
safety classifier, so use the default `--auth-mode preserve` for Agent or
Workflow actions. CCR surfaces classifier denial instead of bypassing it.

## Common Commands

```bash
ccr provider list                 # show configured providers
ccr model list                    # show model aliases
ccr model refresh --all           # refresh discoverable model capabilities
ccr model show <alias> --json     # inspect sources, overrides, and effective values
ccr model test <alias>            # validate a route against its provider
ccr conformance run <alias>       # record compatibility checks
ccr conformance run --all         # check every registered non-blocked routable alias
ccr launch                        # preserve subscription; expose aliases in /model
ccr launch --model <alias>        # start directly on one CCR alias
ccr claude-account import personal --from current
ccr claude-account list
ccr launch --auth-mode subscription-pool
ccr status                        # show the latest observed route and health
ccr trace --follow                # follow redacted route and lifecycle events
ccr sessions --active             # list active launches and Claude sessions
ccr agents --active               # list active agents, teammates, and tasks
ccr doctor --live                 # probe one model per configured provider
ccr doctor --live --all           # diagnose routable aliases; report excluded aliases as skipped
ccr profile export team.json      # export routing config without credentials
```

`ccr status`, `ccr trace`, `ccr sessions`, and `ccr agents` also support stable
`schema_version: 1` JSON output. Launches inject a compact CCR status line and
Claude lifecycle hooks for that process only. Existing status-line and hook
configuration is preserved. Use `--no-statusline`, `--no-lifecycle`, or
`--no-history` to disable those features independently for one launch.

## Team Profiles

Export provider and model routing configuration for another machine without
exporting credentials:

```bash
ccr profile export team.json
ccr profile import team.json --dry-run
ccr profile import team.json --credential openrouter=OPENROUTER_API_KEY
```

Profiles may carry environment-variable names, but never raw secret values,
keychain identifiers, or credential-file paths. Imports are validated and
applied atomically; conflicts fail without partial changes.

## Documentation

- [Getting started](docs/getting-started.md)
- [Providers and model aliases](docs/providers.md)
- [Routing, authentication, and compatibility](docs/routing.md)
- [Troubleshooting](docs/troubleshooting.md)
- [Maintainer release process](docs/releasing.md)

## Security and Local State

CCR stores provider configuration, model aliases, redacted route history,
hook-observed lifecycle state, and compatibility metadata in a local SQLite
database. By default it uses
`$XDG_DATA_HOME/claude-code-router/ccr.db`, or
`~/.local/share/claude-code-router/ccr.db` when `XDG_DATA_HOME` is unset. Use
`--db <path>` to keep state elsewhere.

SQLite contains only secret references such as `env:OPENROUTER_API_KEY` or
redacted `keyring:***`, never the API-key or OAuth-token value. Claude account
access and refresh tokens are stored only in the OS keychain. Route history
never stores prompts, responses, tool
arguments, hook bodies, transcript paths, or authorization headers. CCR records
provider-reported token usage when available but does not estimate monetary
cost. Redacted route and lifecycle metadata is bounded by the local retention
policy, currently 30 days and 10,000 combined events, and can be purged with
`ccr trace purge --all --yes`. See
[provider credential handling](docs/providers.md#credentials) for the supported
secret sources.

Computer-use automation has two explicit boundaries. Direct first-party
Anthropic CUA is client-managed: Claude Code owns the browser, approvals, and
tool-result loop. OpenAI Responses computer use is managed by CCR only. Without
a launch-scoped managed executor, CCR rejects that request before provider
submission rather than emitting a tool action Claude Code cannot execute.
Managed CUA routes run only when explicitly configured for a supported executor:
Docker browser image, trusted host browser, external executor, or unsigned macOS
helper preview. Managed CUA requires an OpenAI Responses-capable provider and a
selected alias with effective Responses and computer-use support. External
managed executors must use a public HTTPS base URL with no credentials, query
string, fragment, or redirects, and launch requires a bearer token supplied
through an environment-variable reference such as:

```bash
export CCR_CUA_EXTERNAL_TOKEN='<external-executor-token>'
ccr launch --model <responses-cua-alias> \
  --ccr-cua-mode managed \
  --ccr-cua-executor external:browser \
  --ccr-cua-external-url https://executor.example/cua \
  --ccr-cua-external-token-env CCR_CUA_EXTERNAL_TOKEN
```

CCR records local approval and audit metadata, not screenshots, prompts, page
contents, or credentials. The macOS helper preview is source-built only. It is
not included in Homebrew bottles or GoReleaser archives, is unsigned, and is
not a production security boundary.

## Development

```bash
make build
make test
make check
make test-cua-macos-fixture
make test-live-fixture
CCR_LIVE_REAL_MATRIX=1 make test-live-real
CCR_LIVE_REAL_MATRIX=1 make test-live-matrix
# With every optional real vision/CUA/executor environment configured:
CCR_LIVE_REAL_MATRIX=1 make test-live-real-full
CCR_LIVE_REAL_MATRIX=1 make test-live-matrix-full
```

The required fixture target needs an installed Claude Code CLI but no provider
credential. CI runs 12 fixture jobs without skips: Linux and macOS, pinned
Claude Code 2.1.209 and the latest npm release, and `openai-chat`,
`anthropic-native`, and `openai-responses` protocols. A separate macOS CI
job validates the portable helper protocol fixtures and compiles the source-built
preview helper. The default real target uses first-party Anthropic
authentication and every configured non-blocked routable alias in the selected
database. `test-live-matrix` runs the fixture target plus that default real
target locally. Real vision, Anthropic CUA, OpenAI Responses CUA, and executor
coverage stay opt-in through the individual `test-live-real-*` targets or the
`test-live-real-full` aggregate; skipped real tests are not evidence of a
verified runtime route.

## Contributing and Security

Read [CONTRIBUTING.md](CONTRIBUTING.md) before opening a pull request. Report
security vulnerabilities privately as described in [SECURITY.md](SECURITY.md).

## License

MIT. See [LICENSE](LICENSE).
