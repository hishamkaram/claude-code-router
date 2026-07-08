# Claude Code Router Agent Instructions

This private repo builds `ccr`, a local Go gateway and CLI for same-session
Claude Code model routing.

## Product Invariants

- Claude Code launches once through one fixed local gateway.
- `/model <alias>` must route future requests in that Claude Code session to the selected alias.
- New subagents, workflow agents, and teammates should use the active model where safely possible.
- Compatibility policy is maximum safe degradation, not all-or-nothing conformance.
- Degradation must be visible; silent fallback to Claude is forbidden.
- Provider secrets must never be logged, printed, committed, or stored raw in SQLite.
- Significant router behavior is not done until a live Claude Code E2E test proves it.
- CLI commands are product surface: validate args strictly and explain behavior clearly.

## Required Guidance

Before non-trivial implementation or review, read the matching files under
`agents/` and `agents/_data/`.

Core contracts:

- `agents/_data/ai-agent-quality-contract.md`
- `agents/_data/engineering-quality-gate.md`
- `agents/_data/maintainability-quality-gate.md`
- `agents/_data/code-quality-floor.md`
- `agents/_data/router-product-invariants.md`
- `agents/_data/cli-contract.md`
- `agents/_data/secret-storage-contract.md`
- `agents/_data/live-e2e-contract.md`

Role guidance:

- Architecture/protocol boundaries: `agents/architect.md`
- Go implementation: `agents/go-implementer.md`
- Go review: `agents/go-reviewer.md`
- Concurrency/streaming/session lifecycle: `agents/go-concurrency.md`
- Tests/live harness: `agents/go-test-writer.md`
- Secret/auth/security: `agents/security-auditor.md`
- User docs/help output: `agents/docs-updater.md`
- External provider research: `agents/researcher.md`

## Go Rules

- Prefer concrete types until a real boundary requires an interface.
- No package-level mutable state except sentinel errors.
- No `init()` for runtime wiring.
- Use constructor injection for dependencies.
- Pass `context.Context` through request, gateway, provider, and subprocess paths.
- Every goroutine must have an owner, cancellation path, and wait/cleanup path.
- Wrap errors with operation context and `%w`.
- Never log and return the same error.
- Use structured logs when logging is introduced.
- Tests must cover validation, error paths, and persistence behavior.

## Verification

Run the affected subset first, then the full local gate before claiming done:

```bash
go test ./...
go test -race -count=1 -p 4 ./...
go vet ./...
golangci-lint run ./...
govulncheck ./...
```

For router behavior that touches Claude Code runtime semantics, also run:

```bash
go test -tags=live -count=1 -p 1 ./...
```

If live Claude Code is unavailable, mark the feature unverified instead of done.
