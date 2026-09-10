# Durable Jobs Acceptance

Status: CCR implementation locally verified on 2026-09-10. CCR-only publication
is authorized subject to green CI and release gates. Runner migration is separate.

## Scope

- CCR-owned durable detached jobs and job-ID status/cancellation.
- Production target: Linux VPS with a usable systemd user manager.
- macOS: native degraded execution, without an escaped-descendant guarantee.
- Runner migration remains blocked on the runner repository/path being supplied.

## Host Evidence

The implementation host reported Linux `6.8.0-138-generic`, `cgroup2fs`, KVM,
a running systemd user manager, and user-service delegation. Per-job scope
admission and cancellation were then exercised directly; root-owned control
files were not modified.

## Live Proofs

The following tests passed on the Linux VPS:

- Double-forked, session-detached descendants stop with their job scope.
- Concurrently forking descendants stop with their job scope.
- Disabling job containment makes both cleanup assertions fail, demonstrated
  by continued descendant activity. Test-owned guardian scopes perform teardown.
- An escaped descendant retaining a large stdin pipe cannot hang the owner.
- A direct child migrated into another scope is reported with unknown cleanup
  after bounded observation, without signaling the external scope.
- Losing the owner's private bus connection after migration also leaves the
  external workload active and reports unknown cleanup without PID fallback.
- An unrelated sibling scope remains active after cancellation.
- A workload-created external user-manager unit remains active and produces
  partial coverage with the D-Bus attribution limitation, not complete coverage.
- Missing-scope cancellation performs no fallback process signaling.
- Cancellation racing admission either refuses workload execution or stops
  the admitted workload without asserting complete coverage.
- The built CCR executable launches actual Claude Code asynchronously, preserves
  stream-JSON output, tolerates prompt-file deletion after admission, persists
  terminal status, and accepts idempotent job-ID cancellation.

The real CLI tests use a local provider fixture. These are not claims about
live paid-provider connectivity or macOS runtime verification.

## Review Rounds

Round 1 completed with four findings:

| Finding | Resolution | Regression |
| --- | --- | --- |
| Escaped stdin holder could block completion | Bound the subprocess pipe-copy wait after child exit | Live stdin-holder containment and mutation cases |
| Socket lookup depended on caller TMPDIR | Stable private per-user socket directory | Cancel from a different TMPDIR |
| JSON escaping exceeded prompt admission limit | Binary prompt field with bounded base64 expansion | Maximum-size prompt round trip |
| Explicitly empty prompt path bypassed validation | Reject empty path during argument parsing | Pre-admission CLI validation |

Round 2 completed with three findings:

| Finding | Resolution | Regression |
| --- | --- | --- |
| Initial bus authentication ignored cancellation | Close the pending connection on setup cancellation | Stalled Unix bus authentication |
| Unrelated manager events could grow blocked delivery goroutines | Manager/unit-scoped filtering with bounded delivery and explicit overflow | Unrelated-unit/sender flood and owned-event overflow |
| FIFO prompt blocked before validation | Open nonblocking, then validate the descriptor | FIFO prompt without a writer |

Round 3 completed with one finding: attached and bundled short session options
bypassed detached admission validation. The validator now recognizes those
forms, with unit and built-CLI regressions using a valid prompt file.

Round 4 completed with two findings:

| Finding | Resolution | Regression |
| --- | --- | --- |
| Direct-child migration retained the owner after scope cleanup | Cancellable exit observation, owner-managed stdin, and bounded release without external signaling | Live migration of the direct child with a large unread prompt |
| Unix persistence assertions ran on Windows | Restrict persistence test to supported native platforms | Windows test compilation retains portable ID/coverage tests |

The stdin ownership change supersedes the initial pipe-copy timeout: there are
no implicit subprocess I/O goroutines to retain when a child must be released.

Round 5 completed with one finding: failed scope control still enabled a direct
PID kill. That fallback has been removed. A live lost-bus regression combines
direct-child migration with failure of only the owner's private connection and
requires the external workload to remain active with unknown cleanup reported.

Admission also uses PID file descriptors to prevent delayed manager requests
from resolving a recycled child PID.

Round 6 reproduced an ambiguous standalone `--` boundary consumed by Claude as
an option value. Detached mode now rejects that ambiguous token before admission;
foreground passthrough remains unchanged. Unit and built-CLI regressions passed.
The reviewer rechecked the updated working tree and completed with no actionable
defects remaining. This satisfies the zero-findings review gate for CCR.

## Gates And Remaining Work

The final working tree passed all local gates after the review fixes:

| Gate | Result |
| --- | --- |
| `go test ./...` | Pass |
| `go test -race -count=1 -p 4 ./...` | Pass |
| `go vet ./...` | Pass |
| `golangci-lint run ./...` | Zero issues |
| `govulncheck ./...` | No vulnerabilities found |
| `make maintainability` | Zero hard violations; 70 target warnings |
| `go test -tags=live -count=1 -p 1 ./...` | Pass |
| Required uncommitted code review | Completed with no actionable defects |

The focused feature live tests have no skipped acceptance cases. Existing
opt-in suites do not establish paid-provider acceptance without their external
configuration.

Two gateway retry tests timed out in the reviewer's concurrent full race run;
their isolated rerun passed. The separate final full race rerun passed; this
does not imply those transient timeouts never occurred.

Final macOS and Windows builds cross-compiled successfully. macOS runtime CI
remains unverified here. The prepared CI matrix includes Linux and macOS with
pinned and latest Claude versions; remote CI has not run for this branch.

The runner repository/path is still needed to remove its old supervision and
signaling implementation and verify its integration against the new job API.
This work is not included in the CCR-only release. Publication evidence is
tracked in [v0.5.0 acceptance](v0.5.0.md).

## Design References

- [Linux cgroup v2 semantics](https://docs.kernel.org/admin-guide/cgroup-v2.html)
- [Systemd scope lifecycle](https://raw.githubusercontent.com/systemd/systemd/v255/man/systemd.scope.xml)
- [Systemd scope empty-event and stop handling](https://raw.githubusercontent.com/systemd/systemd/v255/src/core/scope.c)
- [Go subprocess I/O lifecycle](https://pkg.go.dev/os/exec)
