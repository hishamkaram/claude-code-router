# Engineering Quality Gate

Apply when a change touches architecture, exported surface, config, SQLite schema,
provider contracts, gateway behavior, auth/secrets, concurrency, or live E2E.

Block or escalate on:

- Unclear owner or hidden global state.
- Interface added without a real boundary.
- Provider behavior based on assumption instead of docs or observed traffic.
- Silent fallback to Claude.
- Raw secret storage or secret-bearing errors/logs.
- Runtime behavior without observability and tests.
- New Claude Code feature path without live E2E plan.

Before approving or finishing, document:

- Backward compatibility.
- Failure behavior.
- Observability path.
- Unit/integration tests.
- Live Claude Code E2E status when applicable.
