# CLI Contract

The CLI is part of the product.

Rules:

- Every command has clear help and examples.
- Missing required inputs fail before side effects.
- Unknown commands and invalid args return actionable errors.
- Provider/model commands explain what is stored and where.
- API keys are accepted only through env references, stdin-to-keychain, or future secure prompt flow.
- Any provider-contacting command must show which provider/model it will use.
- Help must explain model aliases, providers, SQLite state, secret handling, compatibility modes, live E2E, and no-silent-fallback.

Tests:

- Root and subcommand help.
- Invalid args and missing args.
- Provider/model validation.
- Secret redaction.
- SQLite path override.
- Doctor/status output.
