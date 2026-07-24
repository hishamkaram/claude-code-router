# Secret Storage Contract

- SQLite stores secret references only, never raw API keys.
- Supported refs:
  - `env:NAME`
  - `file:/absolute/path`
  - `keyring:provider/<name>/api-key`
- Claude subscription account OAuth credentials use dedicated
  `keyring:claude-account/<name>/{access-token,refresh-token}` entries. These
  refs are accepted only by the account registry and cannot be configured as
  provider secret refs.
- File refs must point to regular files with permissions `0600`; file contents
  are resolved at provider-call time and never stored in SQLite.
- Logs, errors, tests, docs, and CLI output must redact file/keyring refs and never include secret values.
- Env refs may show the env var name because the name is not the secret.
- If provider API-key storage fails, return a clear error and suggest
  `--api-key-env` or `--api-key-file`.
- If Claude account OAuth storage fails, require a working OS keychain; never
  suggest provider API-key environment or file references.
- Tests use fake secret backends.
