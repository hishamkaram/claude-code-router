# CCR macOS Computer-Use Preview Helper

`ccr-cua-macos` is an unsigned, local-only preview helper for CCR managed
computer-use. It receives JSON Lines on standard input and emits JSON Lines on
standard output. It is intentionally not a general command runner.

The helper supports only these actions:

- `screenshot`
- `click`
- `double_click`
- `drag`
- `move`
- `type`
- `keypress`
- `scroll`
- `wait`

It never launches a shell or another process, reads no environment values, and
has no credential or environment fields in its protocol. Standard output is the
private protocol pipe, not a log. Screenshots are emitted only as `result`
payloads; the helper does not log screenshots, typed text, tokens, or action
payloads to standard output or standard error.

## Build

On macOS 14 or later, build a local unsigned preview binary with:

```bash
make build-cua-macos
```

No package manager, signing identity, sandbox entitlement, network access, or
provider credential is needed. This is deliberately a standalone preview
artifact, not a released or notarized macOS application.

## Permission Gate

A `start` message succeeds only when both permissions are already available:

- **Accessibility**, checked as CoreGraphics event-posting access.
- **Screen Recording**, checked before screen capture.

When either is unavailable, the helper returns a `permission_required`,
`preview_only` protocol error and performs no action. It does not open System
Settings or issue a permission request itself. Enable the missing permission in
**System Settings > Privacy & Security** for the process that launches the
helper, then restart the helper and send `start` again.

Because this preview binary is unsigned, macOS may not retain grants across
rebuilds or when its launching identity changes. That is an expected preview
limitation and must be checked during live testing.

## Session Model

At launch the helper creates an in-memory 256-bit session token and emits a
`ready` message. CCR's managed executor retains that token only in memory,
sends it with every `start`, `action`, and `close` request, and never forwards
it to a provider or logs it. The helper never repeats the token after `ready`.

See [PROTOCOL.md](PROTOCOL.md) for the exact wire contract. The portable
fixture checks can run on any platform with Python 3:

```bash
python3 -m unittest discover -s cmd/ccr-cua-macos/tests -v
```

The equivalent repository target is:

```bash
make test-cua-macos-fixture
```
