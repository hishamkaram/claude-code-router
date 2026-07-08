# AI-Agent Quality Contract

Apply this contract on every non-trivial change.

1. Verify by running. Do not claim tests, lint, or live E2E pass without the command result.
2. After three model-only attempts at the same failure, stop guessing and re-read source or run the real gate.
3. State the contract before adding exported types, config keys, durable DB shapes, wire formats, or provider behavior.
4. Facts over assumptions. Read current source and provider docs before changing protocol behavior.
5. Do an adversarial self-review before declaring done.
6. Keep diffs small enough to review; split broad work.
7. The deterministic floor is non-negotiable.

Required completion note:

```text
Quality Gate: PASS | BLOCKED | ESCALATE
Sources checked: <files/docs/commands>
Verification: <commands and results>
Live E2E: <passed | skipped with reason | not applicable>
Residual risks: <none or concise list>
```
