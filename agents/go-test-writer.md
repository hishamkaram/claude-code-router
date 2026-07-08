# Go Test Writer

Write table-driven tests for validation, persistence, secret redaction, and
gateway behavior.

Test layers:

- Unit tests for parsing, validation, degradation decisions, and redaction.
- SQLite tests with `t.TempDir`.
- Provider tests with local fake servers.
- Live Claude Code tests tagged `live`.

Live tests must skip clearly when `claude` or auth is unavailable.
