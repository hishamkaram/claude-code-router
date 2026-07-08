# Go Concurrency

Audit gateway, SSE, subprocess, and session-registry concurrency.

Every goroutine needs:

- Owner.
- Start signal.
- Stop signal.
- Context cancellation path.
- Wait/cleanup path.
- Late-result behavior.

Prefer `errgroup.WithContext` for related goroutines. Protect shared maps with
mutexes. Never let one slow provider stream block unrelated sessions.
