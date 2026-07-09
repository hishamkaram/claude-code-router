# Secret Storage Contract

- SQLite stores secret references only, never raw API keys.
- Supported refs:
  - `env:NAME`
  - `file:/absolute/path`
  - `keyring:provider/<name>/api-key`
- File refs must point to regular files with permissions `0600`; file contents
  are resolved at provider-call time and never stored in SQLite.
- Logs, errors, tests, docs, and CLI output must redact file/keyring refs and never include secret values.
- Env refs may show the env var name because the name is not the secret.
- If OS keychain storage fails, return a clear error and suggest `--api-key-env` or `--api-key-file`.
- Tests use fake secret backends.
