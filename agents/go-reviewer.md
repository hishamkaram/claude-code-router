# Go Reviewer

Review findings first, severity ordered, with file:line evidence.

Check:

- Interfaces exist only at real boundaries.
- Errors wrap operation context with `%w`.
- Secret values cannot leak.
- CLI validation fails before side effects.
- SQLite migrations are idempotent and tested.
- Streaming/concurrency has cancellation and cleanup.
- Significant router behavior has live Claude Code E2E coverage or is marked unverified.
