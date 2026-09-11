//go:build linux || darwin

package cli

import (
	"context"
	"errors"

	"github.com/google/uuid"

	"github.com/hishamkaram/claude-code-router/internal/jobs"
)

// A nil lease means a committed replay or a surviving owner: return the same
// receipt without consulting current execution configuration or starting work.
func prepareDetachedAdmission(ctx context.Context, registry *jobs.Registry, opts *options, deps Dependencies, invocation launchInvocation, requestDigest string) (jobs.Admission, *jobs.SessionLease, error) {
	if invocation.submissionID == "" {
		invocation.submissionID = uuid.NewString()
	}
	existing, err := registry.Lookup(ctx, invocation.submissionID)
	if err == nil {
		return recoverDetachedAdmission(ctx, registry, opts, deps, invocation, requestDigest, existing)
	}
	if !errors.Is(err, jobs.ErrAdmissionNotFound) {
		return jobs.Admission{}, nil, err
	}
	candidate := jobs.Admission{
		SubmissionID: invocation.submissionID, JobID: "ccr-" + uuid.NewString(), SessionID: uuid.NewString(),
		RequestDigest: requestDigest, RequestedResumeSession: invocation.resumeSession, ExpectedParentJob: invocation.expectedParent,
	}
	if invocation.resumeSession != "" {
		candidate.SessionID = invocation.resumeSession
	}
	lease, err := registry.LockSession(candidate.SessionID)
	if errors.Is(err, jobs.ErrSessionBusy) {
		// Another caller can bind this same submission after our first lookup.
		// Recover that binding even while its surviving owner holds the lease.
		if raced, lookupErr := registry.Lookup(ctx, invocation.submissionID); lookupErr == nil {
			return recoverDetachedAdmission(ctx, registry, opts, deps, invocation, requestDigest, raced)
		}
	}
	if err != nil {
		return jobs.Admission{}, nil, err
	}
	raced, lookupErr := registry.Lookup(ctx, invocation.submissionID)
	if lookupErr == nil {
		_ = lease.Close()
		return recoverDetachedAdmission(ctx, registry, opts, deps, invocation, requestDigest, raced)
	}
	if !errors.Is(lookupErr, jobs.ErrAdmissionNotFound) {
		_ = lease.Close()
		return jobs.Admission{}, nil, lookupErr
	}
	reserved, err := reserveDetachedAdmission(ctx, registry, lease, opts, deps, invocation, candidate)
	if err != nil {
		_ = lease.Close()
		return jobs.Admission{}, nil, err
	}
	if reserved.JobID != candidate.JobID {
		_ = lease.Close()
		return recoverDetachedAdmission(ctx, registry, opts, deps, invocation, requestDigest, reserved)
	}
	return reserved, lease, nil
}

func reserveDetachedAdmission(ctx context.Context, registry *jobs.Registry, lease *jobs.SessionLease, opts *options, deps Dependencies, invocation launchInvocation, candidate jobs.Admission) (jobs.Admission, error) {
	if candidate.RequestedResumeSession != "" {
		predecessor, err := registry.ResumeHead(ctx, lease, candidate.ExpectedParentJob)
		if err != nil {
			return jobs.Admission{}, err
		}
		candidate.ResumedFrom, candidate.ResumedFromStatus = predecessor.JobID, predecessor.Status
	}
	digest, err := executionFingerprint(ctx, opts, deps, invocation)
	if err != nil {
		return jobs.Admission{}, err
	}
	candidate.ExecutionDigest = digest
	return registry.Reserve(ctx, lease, candidate)
}

func recoverDetachedAdmission(ctx context.Context, registry *jobs.Registry, opts *options, deps Dependencies, invocation launchInvocation, requestDigest string, existing jobs.Admission) (jobs.Admission, *jobs.SessionLease, error) {
	if existing.RequestDigest != requestDigest {
		return jobs.Admission{}, nil, jobs.ErrSubmissionConflict
	}
	if existing.State != jobs.AdmissionPrepared {
		return existing, nil, nil
	}
	lease, err := registry.LockSession(existing.SessionID)
	if errors.Is(err, jobs.ErrSessionBusy) {
		return existing, nil, nil
	}
	if err != nil {
		return jobs.Admission{}, nil, err
	}
	current, err := registry.Lookup(ctx, existing.SubmissionID)
	if err != nil {
		_ = lease.Close()
		return jobs.Admission{}, nil, err
	}
	if current.State != jobs.AdmissionPrepared {
		_ = lease.Close()
		return current, nil, nil
	}
	digest, err := executionFingerprint(ctx, opts, deps, invocation)
	if err != nil || digest != current.ExecutionDigest {
		// The session lease excludes every participating owner. Atomic abort wins
		// only while the durable execution boundary is still uncommitted.
		_, abortErr := registry.AbortPrepared(ctx, current.SubmissionID)
		_ = lease.Close()
		if abortErr != nil {
			return jobs.Admission{}, nil, abortErr
		}
		aborted, lookupErr := registry.Lookup(ctx, current.SubmissionID)
		return aborted, nil, lookupErr
	}
	return current, lease, nil
}
