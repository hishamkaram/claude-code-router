# Maintainability Quality Gate

Targets:

| Type | Target | Hard Fail |
| --- | ---: | ---: |
| Go production file | 600 lines | 800 lines |
| Go test file | 700 lines | 1000 lines |
| Function | 40 lines | 80 lines |
| Orchestrator function | 80 lines | 120 lines |
| Markdown doc | 1200 lines | 1800 lines |

Rules:

- Split files by responsibility before hard fail.
- Keep package ownership clear.
- Do not add permanent exceptions.
- Every temporary exception needs owner, reason, expiry, and current audit evidence.
