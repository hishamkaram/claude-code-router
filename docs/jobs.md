# Detached Jobs

## Commands

```bash
ccr launch --model <alias> --detach -p --prompt-file prompt.txt
ccr status <job_id> --json
ccr cancel <job_id> --json
```

Launch returns a JSON object containing `job_id`, `session_id`, and `submission_id` after durable
admission, without waiting for model execution. Admission is not a successful
provider request: later startup errors appear as failed job status.
Persist a caller-generated submission ID before invocation; recover an ambiguous
acknowledgment with `ccr status --submission-id=<id> --json`. Status never starts work.

`--detach` requires print mode and a nonempty regular prompt file up to 16 MiB.
CCR reads the prompt before admission; deleting the file afterward is safe.
Use Claude options after `--`, for example `--output-format stream-json --verbose`.
CCR owns fresh session IDs. Detached continuation uses `--resume=<sid>` before
the CCR option separator and requires `--output-format=stream-json --verbose`.
Continue, fork, and caller-selected fresh session IDs remain rejected.
Unlike foreground passthrough, detached Claude options cannot contain another
standalone `--`: Claude can consume it as a required option's value, making it
ambiguous as a validation boundary. Use `--name=value` for such option values
and the prompt file for literal prompt text, including text starting with a dash.

Status preserves the existing no-argument configuration view. With a job ID,
new jobs return schema version 2, status, session ID, nullable exit code, output log
paths, cancellation request state, containment backend, and cleanup evidence.
States remain `running`, `completed`, `failed`, and `cancelled`. Schema-1 records
remain readable; absent stop evidence is not inferred.

Cancellation requests are idempotent. A response may still say `running` while
the owner stops the workload; query status again for the terminal result.
Unknown IDs are errors. Repeated cancellation of terminal jobs performs no
process operation. There is no PID, PGID, unit-name, or cgroup-path input.

## Storage And Ownership

Records and logs live under `$XDG_DATA_HOME/claude-code-router/jobs/<job_id>/`,
or `~/.local/share/claude-code-router/jobs/<job_id>/` by default. `--db` changes
the routing database, not the job directory. Use the same data-directory
environment for launch, status, and cancel.

Job directories are private and records/logs use mode 0600. Metadata updates
use synchronized temporary files and atomic replacement. Credentials and the
launch environment are not serialized into job metadata. Output logs preserve
Claude output and may contain sensitive workspace content; do not publish them
without inspection. Prompt input is transferred privately, not stored as a job
configuration file.

Each detached job has one CCR owner that keeps the gateway alive and holds an
exclusive ownership lease. A private local socket accepts cancellation requests.
The launching CLI may exit; the owner and record remain. If the owner disappears
or the host reboots after execution became possible, status marks an unfinished
job failed, with unknown cleanup. Execution-possible jobs are never replayed.
Prepared admission can be recovered only by resubmitting its complete matching
request while no participating owner holds its session lease. Recovery never signals stored process
identifiers. A crashed owner may leave workload processes requiring operator
attention; the record does not claim otherwise.

## Submission Recovery And Continuation

```bash
ccr launch --model <alias> --detach -p --prompt-file first.txt \
  --submission-id=<persisted-token> --output-format=stream-json --verbose
ccr status --submission-id=<persisted-token> --json
ccr status --session-id=<sid> --json
ccr launch --model <alias> --detach -p --prompt-file next.txt \
  --submission-id=<new-persisted-token> --resume=<sid> \
  --expected-parent-job=<head-job-id> --output-format=stream-json --verbose
```

A submission ID is 1–128 printable ASCII characters. Prefer a persisted random
128-bit token. Matching resubmission returns the original job, including after
completion. A different prompt, ordered forwarded arguments, normalized CCR
options, requested session, expected parent, or canonical execution context
conflicts without changing the original binding. The prompt-file locator is
excluded; its exact bytes are included. Live and completed replay does not
resolve current configuration again.

A private SQLite registry in the canonical job root uses WAL, full
synchronization, bounded busy waits, and short transactions. It owns admission
bindings and session heads; job records own lifecycle and cleanup evidence.
Bindings and head tombstones survive artifact deletion. Never delete the
registry to recover a missing job. A missing established registry fails closed.
The registry stores digests and authentication references through a digest,
not prompt text or raw credentials.

Each session has a nonblocking lease retained through final publication. Busy
sessions reject admission; independent sessions can run concurrently. Only
sessions registered in this effective user's canonical job store qualify,
including unambiguous genuine schema-1 detached sessions. Foreground, external,
unknown, and other-store sessions cannot be resumed. Completed, failed, and
cancelled heads require positive stopped evidence. A missing or unresolved head
never permits choosing an older successful ancestor. Supply the expected parent
to reject a changed head, and resolve SID-only anchors before preparing prompts.
These leases coordinate participating CCR jobs in one store; they do not exclude
native Claude processes or jobs using another store.

Execution intent is durable before CCR releases the blocked workload. Owner loss
after that boundary may mean zero executions and an unknown result; it never
permits another execution. Before that boundary, a matching request may complete
the original prepared admission. Changed execution configuration aborts prepared
recovery only after the session lease proves no participating owner can execute.
Prepared cancellation durably prevents revival and preserves the previous head.
Receipt delivery failure does not revoke a surviving owner's work.

## Result And Failure Evidence

`workload_disposition` is `not_started` when the execution gate never became
releasable, `stopped` with a reaped child exit and known empty cleanup observation,
or `unknown` when evidence is insufficient. Terminal status alone is insufficient.
A proven never-started attempt does not manufacture exit 0 or complete coverage.

For stream-JSON jobs, CCR independently observes stdout and checks admitted
session and expected model identity. Finalization joins process cleanup, bounded
output observation, and gateway request accounting. `result_evidence` records a
terminal byte boundary, SHA-256 digest, session/model identity, and success.
Consumers must validate that exact prefix; later appends do not change the
accepted result, and truncation or altered bytes invalidate it.

Failure `reason_code` is one of `startup_failed`, `execution_failed`,
`observation_failed`, `session_identity_mismatch`, `owner_lost`, or
`admission_interrupted`. Startup failure requires no observed initialization or
gateway request entry. CCR does not infer missing history from stderr or read
Claude's private transcripts. Identity mismatch preserves admitted and observed
IDs even when cancellation also fails. Missing or unknown reason codes never
mean success or permission to retry with a fresh session. A caller may explicitly
record a fresh retry after a safely stopped startup failure, using a new session
and submission; that success does not prove the earlier cause.

## Containment And Coverage

The production target is a Linux VPS with a working systemd user manager.
CCR first creates a blocked child, admits it to a unique user scope, and only
then releases the workload. Admission passes a PID file descriptor rather than
a recyclable PID. The manager's unique bus identity is retained for control,
and only that manager's events for the owned unit enter the bounded event queue.
Scope setup errors after creation begins refuse
execution rather than silently changing containment. The owner remains outside
the scope and asks systemd to stop only that scope. It does not need to write
root-owned cgroup controls or request delegation.

CCR collects the child exit code itself. Scope `Result` does not encode the
application exit code. CCR pins the unit while collecting evidence and retains
its own terminal record after systemd collection.
An exit code of -1 means the direct child exited through a signal.
If the direct child migrates outside the scope and does not exit within cleanup's
five-second deadline, CCR releases its handle without signaling the external unit.
The terminal record has a null exit code, unknown coverage, and the unconfirmed
child identity. Terminal status therefore does not imply every process exited.
Loss of the owner bus connection also produces unknown cleanup; it never
enables a direct-child or process-group signal fallback for a scoped workload.

Emptiness evidence is either a `cgroup.events` subtree observation or a positive
successful-stop confirmation from the pinned systemd unit when the kernel file
has already been removed. A missing scope alone never proves cleanup.

macOS is a native development/degraded platform. Linux without an available
user manager also uses a dedicated process group, retaining the unreaped child
identity until group signalling finishes. Linux owners enable orphan reaping
when available. Process-table observations are diagnostics, never signal targets.
After signalling, CCR polls the group within the five-second cleanup deadline
until no non-zombie members remain. If that deadline interrupts observation,
the record retains the last observed survivors and explains the interruption.
An observation failure without a successful snapshot reports unknown coverage.

Cleanup coverage is separate from survivor lists:

- `complete`: cleanup observation and reaping obligations are satisfied and
  escape can be excluded. The current same-user native backends do not claim it.
- `partial`: observation succeeded, but escape cannot be excluded. An empty
  survivor list is valid and does not imply complete cleanup.
- `unknown`: observation failed or evidence is unavailable; a reason is included.

Missing or unrecognized coverage always means `unknown` to consumers.
Changing sessions, process groups, or parents does not escape a Linux scope.
However, invoking `systemd-run --user` or calling the user manager over D-Bus can
launch an external unit. CCR reports this attribution limit and leaves unrelated
units alone. It does not block D-Bus or guess which external processes to kill.

## Verification

```bash
export CCR_LIVE_LEGACY_CCR=/absolute/path/to/ccr-0.5.1
go test -tags=live -count=1 -v ./internal/jobs
go test -tags=live -count=1 -v ./internal/cli -run '^TestLiveDetached(ClaudeJobs|ClaudeContinuation|LegacyContinuation)$'
```

The Linux containment suite checks escaped descendants, concurrent forking,
unrelated sibling preservation, external-unit escape reporting, missing scopes,
direct-child migration with bounded completion, and admission/cancellation races.
Mandatory mutation controls disable job
containment and require continued descendant activity. Separate test-owned
guardian scopes clean up those intentionally surviving fixtures.

The CLI test uses the actual Claude executable against a local provider fixture
and verifies durable asynchronous admission, output framing, and cancellation.
Required missing capabilities fail these tests rather than producing a skipped
acceptance result.
