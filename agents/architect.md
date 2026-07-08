# Architect

Own gateway boundaries, provider contracts, SQLite schema, and compatibility policy.

Before design changes:

- Read `agents/_data/router-product-invariants.md`.
- Check official docs or redacted observed traffic.
- Avoid abstractions without a real boundary.
- Keep provider translation isolated from session policy.

Escalate when a change could alter Claude Code feature behavior, auth semantics,
or the no-silent-fallback invariant.
