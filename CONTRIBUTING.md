# Contributing

## Before You Start

Open an issue or discussion for a substantial behavior change before writing a
large patch. CCR intentionally favors safe, visible degradation over silent
fallback, so routing and provider changes need an agreed compatibility contract.

Do not include provider credentials, session tokens, local databases, or Claude
Code transcripts containing private information in issues, commits, or pull
requests.

## Development Setup

Install Go 1.25 or newer, then run:

```bash
make build
make test
```

Use `./bin/ccr --help` to inspect the built CLI. The local database can be
isolated for development with `--db /tmp/ccr.db`.

## Pull Requests

- Keep each change focused and document user-visible behavior in the README or
  a guide when it changes.
- Add or update tests for validation, error paths, persistence, and provider
  behavior as appropriate.
- Run `make check` before requesting review.
- Run `make test-live` for changes that affect Claude Code routing, tool calls,
  model switching, authentication, subagents, workflows, or compatibility.
  If a live test cannot run, state that clearly in the pull request.
- Use clear, imperative commit subjects such as `fix(gateway): preserve ...`.

By contributing, you agree that your contributions are licensed under the
[MIT License](LICENSE).
