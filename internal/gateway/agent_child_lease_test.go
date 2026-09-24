package gateway

import (
	"testing"
	"time"
)

func TestActiveAgentChildLeaseRetainsBoundedFailClosedTombstone(t *testing.T) {
	now := time.Now()
	s := newActiveModelSelection("active")
	descriptor := agentChildDescriptor{prompt: "bounded child"}
	req := anthropicRequest{
		Model:    "sonnet",
		System:   "You are a subagent continuing the bounded child.",
		Messages: []anthropicMessage{{Role: "user", Content: "bounded child"}},
	}
	s.mu.Lock()
	s.pendingAgentChildren["session-a"] = []pendingAgentChild{{
		id:                   1,
		descriptor:           descriptor,
		spawnAlias:           "active",
		continuationIdentity: agentChildContinuationIdentity(req, descriptor),
		active:               true,
		expiresAt:            now.Add(-time.Second),
		rejectUntil:          now.Add(time.Minute),
	}}
	s.childRoutingUntil["session-a"] = now.Add(time.Minute)
	s.mu.Unlock()

	s.cleanupExpiredAgentChildren(now)
	s.mu.RLock()
	_, retained := s.pendingAgentChildren["session-a"]
	s.mu.RUnlock()
	if !retained {
		t.Fatal("expired active child was removed before its fail-closed tombstone expired")
	}
	reservation := s.reserveAgentChild("session-a", req)
	if !reservation.expired || !reservation.identified || reservation.alias != "" {
		t.Fatalf("expired active child reservation = %#v; want identified refusal", reservation)
	}

	// A child can legitimately outlive the active lease. Descriptor eviction must
	// not make its next first-party request eligible for subscription routing.
	s.cleanupExpiredAgentChildren(now.Add(agentChildActiveLeaseTTL + agentChildReservationRetention + time.Minute))
	s.mu.RLock()
	_, pending := s.pendingAgentChildren["session-a"]
	_, tombstone := s.childRoutingUntil["session-a"]
	_, failClosed := s.childRoutingFailClosed["session-a"]
	s.mu.RUnlock()
	if pending || tombstone || !failClosed {
		t.Fatal("expired active child was not collapsed to a bounded fail-closed session marker")
	}
	reservation = s.reserveAgentChild("session-a", req)
	if reservation.pending || !reservation.identified || reservation.expired || reservation.alias != "" {
		t.Fatalf("long-expired active child reservation = %#v; want identified refusal", reservation)
	}

	s.cleanupExpiredAgentChildren(now.Add(agentChildActiveLeaseTTL + 2*agentChildReservationRetention + time.Minute))
	s.mu.RLock()
	_, failClosed = s.childRoutingFailClosed["session-a"]
	s.mu.RUnlock()
	if failClosed {
		t.Fatal("expired active child fail-closed marker was retained beyond its bounded lifetime")
	}

	s.observeAgentChildLifecycle("session-a", "SessionEnd")
	s.mu.RLock()
	_, pending = s.pendingAgentChildren["session-a"]
	_, tombstone = s.childRoutingUntil["session-a"]
	_, failClosed = s.childRoutingFailClosed["session-a"]
	s.mu.RUnlock()
	if pending || tombstone || failClosed {
		t.Fatal("SessionEnd did not clean up the retained active child reservation")
	}
}
