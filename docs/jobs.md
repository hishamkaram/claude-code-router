# Detached Jobs

## Commands

```bash
ccr launch --model <alias> --detach -p --prompt-file prompt.txt
ccr status <job_id> --json
ccr cancel <job_id> --json
```

Launch returns a JSON object containing `job_id` and `session_id` after durable
admission, without waiting for model execution. Admission is not a successful
provider request: later startup errors appear as failed job status.
Check a returned ID before retrying an ambiguous launch acknowledgment.

`--detach` requires print mode and a nonempty regular prompt file up to 16 MiB.
CCR reads the prompt before admission; deleting the file afterward is safe.
Use Claude options after `--`, for example `--output-format stream-json --verbose`.
CCR owns the new session ID; resume, continue, and caller-selected session IDs
cannot be combined with detached launch.
Unlike foreground passthrough, detached Claude options cannot contain another
standalone `--`: Claude can consume it as a required option's value, making it
ambiguous as a validation boundary. Use `--name=value` for such option values
and the prompt file for literal prompt text, including text starting with a dash.

Status preserves the existing no-argument configuration view. With a job ID,
it returns schema version 1, status, session ID, nullable exit code, output log
paths, cancellation request state, containment backend, and cleanup evidence.
States are `running`, `completed`, `failed`, and `cancelled`.

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
or the host reboots, status marks an unfinished job failed, with unknown cleanup.
Jobs are never automatically replayed. Recovery never signals stored process
identifiers. A crashed owner may leave workload processes requiring operator
attention; the record does not claim otherwise.

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
go test -tags=live -count=1 -v ./internal/jobs
go test -tags=live -count=1 -v ./internal/cli -run '^TestLiveDetachedClaudeJobs$'
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
