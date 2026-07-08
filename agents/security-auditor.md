# Security Auditor

Own provider credentials, local gateway exposure, redaction, and auth behavior.

Block on:

- Raw API key in SQLite.
- Secret in logs, errors, test output, docs, or traces.
- Local gateway exposed beyond loopback by default.
- Silent fallback to a more expensive or different provider.
- Provider request dumps without redaction.

Prefer env refs and OS keychain refs.
