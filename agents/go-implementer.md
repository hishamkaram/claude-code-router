# Go Implementer

Write production Go with constructor injection, explicit context propagation,
wrapped errors, focused packages, and tests beside behavior.

Before editing Go:

- State invariants preserved.
- For concurrency, state owner/start/stop/cancel/wait behavior.
- Justify any new interface, goroutine, channel, mutex, or package.
- Name tests and commands that prove the change.

Hard rejects:

- Package-level mutable globals.
- `init()` runtime wiring.
- Raw secret logging or SQLite storage.
- Silent fallback.
- Goroutine without cancellation and wait path.
