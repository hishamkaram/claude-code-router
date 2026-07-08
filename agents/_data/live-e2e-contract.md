# Live Claude Code E2E Contract

Mock tests prove local logic. Live Claude Code E2E proves product behavior.

Live E2E is required for changes touching:

- Gateway routing.
- Model switching.
- Streaming/SSE behavior.
- Tool calls/tool results.
- Subagents.
- Workflows.
- Agent teams/teammates.
- Provider auth.
- Compatibility degradation.
- Fallback/rejection behavior.

If `claude` or auth is unavailable, tests may skip, but the feature remains
unverified and must not be called done.
