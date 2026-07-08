# Secret Storage Contract

- SQLite stores secret references only, never raw API keys.
- Supported refs:
  - `env:NAME`
  - `keyring:provider/<name>/api-key`
- Logs, errors, tests, docs, and CLI output must redact keyring refs and never include secret values.
- Env refs may show the env var name because the name is not the secret.
- If OS keychain storage fails, return a clear error and suggest `--api-key-env`.
- Tests use fake secret backends.
