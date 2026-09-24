package gateway

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"time"
)

const autoModeClassifierSystemPrefix = "You are a security monitor for autonomous AI coding agents."

const claudeCodeSessionIDHeader = "x-claude-code-session-id"

type activeModelSelection struct {
	mu                   sync.RWMutex
	defaultAlias         string
	aliases              map[string]string
	pendingAgentChildren map[string][]pendingAgentChild
	childRoutingUntil    map[string]time.Time
	// The expiry bounds memory used to fail closed after a reservation was
	// evicted without SessionEnd. A lifecycle hook is still preferred, but a
	// missing hook must not turn every observed session ID into a permanent
	// gateway allocation.
	childRoutingFailClosed map[string]time.Time
	nextAgentChildID       uint64
}

type agentChildReservation struct {
	alias      string
	rollback   func()
	pending    bool
	identified bool
	expired    bool
	ambiguous  bool
}

func (s *activeModelSelection) startAgentChildCleanup(ctx context.Context) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(agentChildReservationTTL)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-ticker.C:
				s.cleanupExpiredAgentChildren(now)
			}
		}
	}()
	return done
}

func newActiveModelSelection(alias string) activeModelSelection {
	return activeModelSelection{
		defaultAlias:           strings.TrimSpace(alias),
		aliases:                make(map[string]string),
		pendingAgentChildren:   make(map[string][]pendingAgentChild),
		childRoutingUntil:      make(map[string]time.Time),
		childRoutingFailClosed: make(map[string]time.Time),
	}
}

func (s *activeModelSelection) agentChildMarker(sessionID, spawnAlias string) func([]agentChildDescriptor) (func(), error) {
	// First-party Anthropic requests intentionally have no CCR spawn alias. Do
	// not let registerAgentChildDescriptorsAtAliasChecked infer the process
	// default here: that would make an uncorrelated native response look like a
	// CCR child and could turn valid native Agent/Task tools into a compatibility
	// error.
	if strings.TrimSpace(spawnAlias) == "" {
		return nil
	}
	return func(descriptors []agentChildDescriptor) (func(), error) {
		// The parent route is only the authorization boundary for child
		// registration. Do not persist its alias: the parent can still be in
		// flight when the user changes /model. The child is new work and must
		// resolve the session's active alias when its first request arrives.
		return s.registerAgentChildDescriptorsForNewWork(sessionID, descriptors)
	}
}

func (s *activeModelSelection) registerAgentChildDescriptorsAtAliasChecked(sessionID, spawnAlias string, descriptors []agentChildDescriptor) (func(), error) {
	return s.registerAgentChildDescriptors(sessionID, spawnAlias, descriptors, false)
}

func (s *activeModelSelection) registerAgentChildDescriptorsForNewWork(sessionID string, descriptors []agentChildDescriptor) (func(), error) {
	return s.registerAgentChildDescriptors(sessionID, "", descriptors, true)
}

func (s *activeModelSelection) registerAgentChildDescriptors(sessionID, spawnAlias string, descriptors []agentChildDescriptor, resolveAliasOnFirstRequest bool) (func(), error) {
	sessionID = strings.TrimSpace(sessionID)
	spawnAlias = strings.TrimSpace(spawnAlias)
	if !resolveAliasOnFirstRequest && spawnAlias == "" {
		spawnAlias = s.currentAlias(sessionID)
	}
	if len(descriptors) == 0 {
		return nil, nil
	}
	if sessionID == "" {
		return nil, errAgentChildSessionCorrelation
	}
	if !resolveAliasOnFirstRequest && spawnAlias == "" {
		return nil, nil
	}
	s.mu.Lock()
	now := time.Now()
	if s.childRoutingUntil == nil {
		s.childRoutingUntil = make(map[string]time.Time)
	}
	entries := make([]pendingAgentChild, 0, len(descriptors))
	for _, descriptor := range descriptors {
		s.nextAgentChildID++
		entries = append(entries, pendingAgentChild{
			id:          s.nextAgentChildID,
			descriptor:  descriptor,
			spawnAlias:  spawnAlias,
			expiresAt:   now.Add(agentChildReservationTTL),
			rejectUntil: now.Add(agentChildReservationTTL + agentChildReservationRetention),
		})
	}
	reservationUntil := now.Add(agentChildReservationTTL + agentChildReservationRetention)
	if reservationUntil.After(s.childRoutingUntil[sessionID]) {
		s.childRoutingUntil[sessionID] = reservationUntil
	}
	s.pendingAgentChildren[sessionID] = append(s.pendingAgentChildren[sessionID], entries...)
	s.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() { s.unmarkAgentChildren(sessionID, entries) })
	}, nil
}

func (s *activeModelSelection) unmarkAgentChildren(sessionID string, entries []pendingAgentChild) {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" || len(entries) == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	pending := s.pendingAgentChildren[sessionID]
	if len(pending) == 0 {
		return
	}
	remove := make(map[uint64]struct{}, len(entries))
	for index := range entries {
		remove[entries[index].id] = struct{}{}
	}
	kept := pending[:0]
	for index := range pending {
		entry := pending[index]
		if _, shouldRemove := remove[entry.id]; !shouldRemove {
			kept = append(kept, entry)
		}
	}
	if len(kept) == 0 {
		delete(s.pendingAgentChildren, sessionID)
		delete(s.childRoutingUntil, sessionID)
		return
	}
	s.pendingAgentChildren[sessionID] = kept
}

func (s *activeModelSelection) reserveAgentChild(sessionID string, req anthropicRequest) agentChildReservation {
	sessionID = strings.TrimSpace(sessionID)
	if !isFirstPartyAnthropicModelRequest(req.Model) {
		return agentChildReservation{}
	}
	identified := agentChildRequestHasSubagentSystemMarker(req)
	if sessionID == "" {
		return uncorrelatedAgentChildReservation(identified)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	entries := s.pendingAgentChildren[sessionID]
	now := time.Now()
	if len(entries) == 0 {
		return s.missingAgentChildReservationLocked(sessionID, identified, now)
	}
	kept, selected, expired, ambiguous := selectPendingAgentChild(entries, req, now)
	s.storeSelectedAgentChildEntriesLocked(sessionID, kept)
	return s.finishAgentChildReservationLocked(sessionID, req, entries, selected, expired, ambiguous, now)
}

// uncorrelatedAgentChildReservation refuses identified native child work when
// the gateway cannot associate it with the Claude session that created it.
func uncorrelatedAgentChildReservation(identified bool) agentChildReservation {
	if identified {
		return agentChildReservation{identified: true}
	}
	return agentChildReservation{}
}

func (s *activeModelSelection) missingAgentChildReservationLocked(sessionID string, identified bool, now time.Time) agentChildReservation {
	// A child request must not silently fall through to first-party Anthropic
	// after its descriptor has been evicted. The bounded marker covers the late
	// continuation window; an absent lifecycle hook must not retain the session
	// ID forever.
	if identified && s.agentChildRoutingMarkerActiveLocked(sessionID, now) {
		return agentChildReservation{identified: true}
	}
	return agentChildReservation{}
}

func (s *activeModelSelection) agentChildRoutingMarkerActiveLocked(sessionID string, now time.Time) bool {
	if until, ok := s.childRoutingFailClosed[sessionID]; ok && until.After(now) {
		return true
	}
	return s.childRoutingUntil[sessionID].After(now) || s.currentAliasLocked(sessionID) != ""
}

func (s *activeModelSelection) storeSelectedAgentChildEntriesLocked(sessionID string, kept []pendingAgentChild) {
	if len(kept) == 0 {
		delete(s.pendingAgentChildren, sessionID)
		return
	}
	s.pendingAgentChildren[sessionID] = kept
}

func (s *activeModelSelection) finishAgentChildReservationLocked(sessionID string, req anthropicRequest, entries []pendingAgentChild, selected pendingAgentChild, expired, ambiguous bool, now time.Time) agentChildReservation {
	if selected.id == 0 {
		if ambiguous {
			return agentChildReservation{pending: true, identified: true, ambiguous: true}
		}
		// An accepted child request without a native subagent marker cannot create
		// a continuation identity. Refuse a subsequent request when it has the
		// child transcript shape (or repeats the child prompt), rather than
		// silently sending that continuation to the Anthropic subscription. A
		// standalone request with no child evidence remains an explicit/native
		// parent request and is allowed to use the first-party route.
		if hasAgentChildContinuationEvidence(entries, req) {
			return agentChildReservation{identified: true}
		}
		return agentChildReservation{}
	}
	if expired {
		return agentChildReservation{pending: true, identified: true, expired: true}
	}
	wasActive := selected.active
	alias, activationGeneration := s.activateAgentChild(sessionID, selected, req, now)
	if alias == "" {
		return agentChildReservation{
			rollback: func() { s.restoreAgentChild(sessionID, selected.id, activationGeneration, wasActive) },
			pending:  true, identified: true,
		}
	}
	var once sync.Once
	return agentChildReservation{
		alias: alias,
		rollback: func() {
			once.Do(func() { s.restoreAgentChild(sessionID, selected.id, activationGeneration, wasActive) })
		},
		pending: true, identified: true,
	}
}

func (s *activeModelSelection) peekAgentChild(sessionID string, req anthropicRequest) agentChildReservation {
	sessionID = strings.TrimSpace(sessionID)
	if !isFirstPartyAnthropicModelRequest(req.Model) {
		return agentChildReservation{}
	}
	identified := agentChildRequestHasSubagentSystemMarker(req)
	if sessionID == "" {
		if identified {
			return agentChildReservation{identified: true}
		}
		return agentChildReservation{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	entries := s.pendingAgentChildren[sessionID]
	now := time.Now()
	if len(entries) == 0 {
		return s.peekMissingAgentChildLocked(sessionID, identified, now)
	}
	return s.peekPendingAgentChildLocked(sessionID, req, entries, now)
}

func (s *activeModelSelection) peekMissingAgentChildLocked(sessionID string, identified bool, now time.Time) agentChildReservation {
	failClosedUntil, failClosed := s.childRoutingFailClosed[sessionID]
	if failClosed && !failClosedUntil.After(now) {
		failClosed = false
	}
	if identified && (failClosed || s.childRoutingUntil[sessionID].After(now) || s.currentAliasLocked(sessionID) != "") {
		return agentChildReservation{identified: true}
	}
	return agentChildReservation{}
}

func (s *activeModelSelection) peekPendingAgentChildLocked(sessionID string, req anthropicRequest, entries []pendingAgentChild, now time.Time) agentChildReservation {
	_, selected, expired, ambiguous := selectPendingAgentChild(entries, req, now)
	if ambiguous {
		return agentChildReservation{pending: true, identified: true, ambiguous: true}
	}
	if selected.id == 0 {
		if hasAgentChildContinuationEvidence(entries, req) {
			return agentChildReservation{identified: true}
		}
		return agentChildReservation{}
	}
	if expired {
		return agentChildReservation{pending: true, identified: true, expired: true}
	}
	alias := selected.spawnAlias
	if alias == "" {
		// Peeking is used by token-count probes and must not consume or pin a
		// new-work reservation. The first real child message resolves and stores
		// the active alias in reserveAgentChild; a /model change between the
		// probe and that message must remain observable.
		alias = s.currentAliasLocked(sessionID)
	}
	return agentChildReservation{alias: alias, pending: true, identified: true}
}

// observeAgentChildLifecycle ties the reservation lifetime to Claude Code's
// session lifecycle. Initial child descriptors still use a short correlation
// window, but once a child request has been accepted its reservation is held
// until the owning Claude session ends. Lifecycle hooks also refresh a pending
// descriptor when Claude has started background work before its first request.
func (s *activeModelSelection) observeAgentChildLifecycle(sessionID, eventName string) {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if eventName == "SessionEnd" {
		delete(s.pendingAgentChildren, sessionID)
		delete(s.childRoutingUntil, sessionID)
		delete(s.childRoutingFailClosed, sessionID)
		delete(s.aliases, sessionID)
		return
	}
	if eventName != "SubagentStart" && eventName != "TaskCreated" {
		return
	}
	now := time.Now()
	for index := range s.pendingAgentChildren[sessionID] {
		entry := &s.pendingAgentChildren[sessionID][index]
		if entry.active || !entry.rejectUntil.After(now) {
			continue
		}
		entry.expiresAt = now.Add(agentChildReservationTTL)
		entry.rejectUntil = now.Add(agentChildReservationTTL + agentChildReservationRetention)
		if entry.rejectUntil.After(s.childRoutingUntil[sessionID]) {
			s.childRoutingUntil[sessionID] = entry.rejectUntil
		}
	}
}

func (s *activeModelSelection) activateAgentChild(sessionID string, selected pendingAgentChild, req anthropicRequest, now time.Time) (alias string, activationGeneration uint64) {
	continuationIdentity := agentChildContinuationIdentity(req, selected.descriptor)
	for index := range s.pendingAgentChildren[sessionID] {
		if s.pendingAgentChildren[sessionID][index].id != selected.id {
			continue
		}
		s.pendingAgentChildren[sessionID][index].activationGeneration++
		s.pendingAgentChildren[sessionID][index].active = true
		if continuationIdentity != "" {
			s.pendingAgentChildren[sessionID][index].continuationIdentity = continuationIdentity
		}
		activeUntil := now.Add(agentChildActiveLeaseTTL)
		s.pendingAgentChildren[sessionID][index].expiresAt = activeUntil
		s.pendingAgentChildren[sessionID][index].rejectUntil = activeUntil.Add(agentChildReservationRetention)
		if s.childRoutingUntil == nil {
			s.childRoutingUntil = make(map[string]time.Time)
		}
		if s.pendingAgentChildren[sessionID][index].rejectUntil.After(s.childRoutingUntil[sessionID]) {
			s.childRoutingUntil[sessionID] = s.pendingAgentChildren[sessionID][index].rejectUntil
		}
		break
	}
	alias = selected.spawnAlias
	if alias == "" {
		alias = s.currentAliasLocked(sessionID)
		s.setAgentChildSpawnAlias(sessionID, selected.id, alias)
	}
	for index := range s.pendingAgentChildren[sessionID] {
		if s.pendingAgentChildren[sessionID][index].id == selected.id {
			return alias, s.pendingAgentChildren[sessionID][index].activationGeneration
		}
	}
	return alias, 0
}

func (s *activeModelSelection) setAgentChildSpawnAlias(sessionID string, id uint64, alias string) {
	for index := range s.pendingAgentChildren[sessionID] {
		if s.pendingAgentChildren[sessionID][index].id == id {
			s.pendingAgentChildren[sessionID][index].spawnAlias = alias
			return
		}
	}
}

type pendingAgentChildMatches struct {
	kept                []pendingAgentChild
	live                []pendingAgentChild
	expired             []pendingAgentChild
	liveContinuation    []pendingAgentChild
	expiredContinuation []pendingAgentChild
}

func classifyPendingAgentChildren(entries []pendingAgentChild, req anthropicRequest, now time.Time) pendingAgentChildMatches {
	// Classification is also used by peekAgentChild under RLock. Do not reuse
	// the reservation slice's backing array: appending an unmatched sibling to
	// entries[:0] would mutate the live table while only a read lock is held and
	// could erase a reservation that the following message still needs.
	matches := pendingAgentChildMatches{kept: make([]pendingAgentChild, 0, len(entries))}
	for index := range entries {
		entry := entries[index]
		if !entry.active && !entry.rejectUntil.After(now) {
			continue
		}
		matched := agentChildRequestMatches(req, entry.descriptor)
		if entry.active {
			matched = agentChildContinuationMatches(req, entry)
		}
		if !matched {
			matches.kept = append(matches.kept, entry)
			continue
		}
		if entry.expiresAt.After(now) || entry.active && entry.expiresAt.IsZero() {
			if entry.active {
				matches.liveContinuation = append(matches.liveContinuation, entry)
			} else {
				matches.live = append(matches.live, entry)
			}
			continue
		}
		if entry.active {
			matches.expiredContinuation = append(matches.expiredContinuation, entry)
		} else {
			matches.expired = append(matches.expired, entry)
		}
	}
	return matches
}

func choosePendingAgentChild(live, expired []pendingAgentChild) (selected pendingAgentChild, expiredReservation, ambiguous bool) {
	switch {
	case len(live) == 1 && len(expired) == 0:
		return live[0], false, false
	case len(live) == 0 && len(expired) == 1:
		return expired[0], true, false
	case len(live)+len(expired) > 1:
		// The native child marker is shared by sibling reservations. Without a
		// real child identity, even same-alias duplicates are not safe to choose:
		// consuming one reservation could attach the request to the wrong child
		// and make a later continuation inherit the wrong spawn-time route.
		return pendingAgentChild{}, false, true
	default:
		return pendingAgentChild{}, false, false
	}
}

func isGenericWorkflowReservation(entry pendingAgentChild) bool {
	return entry.descriptor.workflow &&
		strings.TrimSpace(entry.descriptor.prompt) == "" &&
		strings.TrimSpace(entry.descriptor.description) == ""
}

func preferSpecificPendingAgentChildren(matches *pendingAgentChildMatches) {
	specificLive := make([]pendingAgentChild, 0, len(matches.live))
	specificExpired := make([]pendingAgentChild, 0, len(matches.expired))
	generic := make([]pendingAgentChild, 0)
	for index := range matches.live {
		entry := matches.live[index]
		if isGenericWorkflowReservation(entry) {
			generic = append(generic, entry)
			continue
		}
		specificLive = append(specificLive, entry)
	}
	for index := range matches.expired {
		entry := matches.expired[index]
		if isGenericWorkflowReservation(entry) {
			generic = append(generic, entry)
			continue
		}
		specificExpired = append(specificExpired, entry)
	}
	if len(specificLive)+len(specificExpired) == 0 {
		return
	}
	matches.kept = append(matches.kept, generic...)
	matches.live = specificLive
	matches.expired = specificExpired
}

func selectPendingAgentChild(entries []pendingAgentChild, req anthropicRequest, now time.Time) (kept []pendingAgentChild, selected pendingAgentChild, expired, ambiguous bool) {
	matches := classifyPendingAgentChildren(entries, req, now)
	// Capture this before generic workflow reservations are deprioritized. A
	// generic workflow has no prompt identity, so it remains a competing match
	// when an active continuation is also present even if a more specific
	// reservation is preferred for ordinary initial selection.
	hasInitialMatches := len(matches.live)+len(matches.expired) > 0
	preferSpecificPendingAgentChildren(&matches)
	kept = matches.kept
	// A stable native child identity is usable only when it is the sole
	// candidate. Claude Code's marker is shared by sibling children; if a live
	// continuation and an initial reservation both match, there is no native
	// child ID available to tell them apart. Refuse visibly instead of routing
	// a new sibling to an older worker's spawn-time alias.
	if len(matches.liveContinuation) > 0 && hasInitialMatches {
		kept = append(kept, matches.liveContinuation...)
		kept = append(kept, matches.expiredContinuation...)
		kept = append(kept, matches.live...)
		kept = append(kept, matches.expired...)
		return kept, pendingAgentChild{}, false, true
	}
	// An expired continuation does not block a fresh live child reservation;
	// the existing worker's bounded lease has already ended. If there is no
	// prompt-only match, however, retain the visible expiry refusal for that
	// identified continuation instead of allowing it to fall through.
	if len(matches.liveContinuation) > 0 ||
		!hasInitialMatches && len(matches.expiredContinuation) > 0 {
		selected, expired, ambiguous = choosePendingAgentChild(matches.liveContinuation, matches.expiredContinuation)
		kept = retainSelectedPendingAgentChild(kept, matches.liveContinuation, matches.expiredContinuation, selected, ambiguous)
		// Prompt-only sibling reservations remain available for their own native
		// child request; they must not be consumed by this continuation.
		kept = append(kept, matches.live...)
		kept = append(kept, matches.expired...)
		return kept, selected, expired, ambiguous
	}
	hasInitialMatches = len(matches.live)+len(matches.expired) > 0
	live, expiredMatches := matches.live, matches.expired
	if !hasInitialMatches {
		live, expiredMatches = matches.liveContinuation, matches.expiredContinuation
	}
	selected, expired, ambiguous = choosePendingAgentChild(live, expiredMatches)
	kept = retainSelectedPendingAgentChild(kept, live, expiredMatches, selected, ambiguous)
	// A live active reservation represents a child that has already started.
	// It must survive a sibling's initial request: the sibling may be selected
	// by the descriptor match above, but the active child still needs its own
	// continuation reservation.
	if hasInitialMatches {
		kept = retainActiveAgentChildContinuations(kept, matches, selected.id)
	}
	return kept, selected, expired, ambiguous
}

func retainSelectedPendingAgentChild(kept, live, expired []pendingAgentChild, selected pendingAgentChild, ambiguous bool) []pendingAgentChild {
	if ambiguous {
		kept = append(kept, live...)
		return append(kept, expired...)
	}
	if selected.id == 0 {
		return kept
	}
	return append(kept, selected)
}

func appendUnselectedPendingAgentChildren(kept, entries []pendingAgentChild, selectedID uint64) []pendingAgentChild {
	for index := range entries {
		if entries[index].id != selectedID {
			kept = append(kept, entries[index])
		}
	}
	return kept
}

func retainActiveAgentChildContinuations(kept []pendingAgentChild, matches pendingAgentChildMatches, selectedID uint64) []pendingAgentChild {
	kept = appendUnselectedPendingAgentChildren(kept, matches.liveContinuation, selectedID)
	// Keep an expired active child through rejectUntil. Its next continuation
	// must receive the visible expiry refusal rather than silently falling
	// through to the native Anthropic route.
	return appendUnselectedPendingAgentChildren(kept, matches.expiredContinuation, selectedID)
}

func (s *activeModelSelection) restoreAgentChild(sessionID string, id, activationGeneration uint64, active bool) {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" || id == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for index := range s.pendingAgentChildren[sessionID] {
		entry := &s.pendingAgentChildren[sessionID][index]
		if entry.id != id || entry.activationGeneration != activationGeneration {
			continue
		}
		now := time.Now()
		entry.active = active
		if active {
			activeUntil := now.Add(agentChildActiveLeaseTTL)
			entry.expiresAt = activeUntil
			entry.rejectUntil = activeUntil.Add(agentChildReservationRetention)
		} else {
			entry.expiresAt = now.Add(agentChildReservationTTL)
			entry.rejectUntil = now.Add(agentChildReservationTTL + agentChildReservationRetention)
		}
		if s.childRoutingUntil == nil {
			s.childRoutingUntil = make(map[string]time.Time)
		}
		if entry.rejectUntil.After(s.childRoutingUntil[sessionID]) {
			s.childRoutingUntil[sessionID] = entry.rejectUntil
		}
		return
	}
}

func (s *activeModelSelection) cleanupExpiredAgentChildren(now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for sessionID, entries := range s.pendingAgentChildren {
		s.cleanupAgentChildSessionLocked(sessionID, entries, now)
	}
	removeExpiredAgentChildMarkers(s.childRoutingUntil, now)
	removeExpiredAgentChildMarkers(s.childRoutingFailClosed, now)
}

func (s *activeModelSelection) cleanupAgentChildSessionLocked(sessionID string, entries []pendingAgentChild, now time.Time) {
	kept := entries[:0]
	for index := range entries {
		kept = s.retainAgentChildEntryLocked(sessionID, kept, entries[index], now)
	}
	if len(kept) > 0 {
		s.pendingAgentChildren[sessionID] = kept
		return
	}
	delete(s.pendingAgentChildren, sessionID)
	if !s.childRoutingUntil[sessionID].After(now) {
		delete(s.childRoutingUntil, sessionID)
	}
}

func (s *activeModelSelection) retainAgentChildEntryLocked(sessionID string, kept []pendingAgentChild, entry pendingAgentChild, now time.Time) []pendingAgentChild {
	// Keep the descriptor through its bounded rejection window so a late
	// continuation receives the specific expiry error. Once that window closes,
	// discard it but retain a session-level fail-closed marker.
	if (entry.active && (entry.expiresAt.IsZero() || entry.expiresAt.After(now))) || entry.rejectUntil.After(now) {
		return append(kept, entry)
	}
	s.markAgentChildFailClosedLocked(sessionID, now)
	return kept
}

func (s *activeModelSelection) markAgentChildFailClosedLocked(sessionID string, now time.Time) {
	if s.childRoutingFailClosed == nil {
		s.childRoutingFailClosed = make(map[string]time.Time)
	}
	failClosedUntil := now.Add(agentChildReservationRetention)
	if failClosedUntil.After(s.childRoutingFailClosed[sessionID]) {
		s.childRoutingFailClosed[sessionID] = failClosedUntil
	}
}

func removeExpiredAgentChildMarkers(markers map[string]time.Time, now time.Time) {
	for sessionID, until := range markers {
		if !until.After(now) {
			delete(markers, sessionID)
		}
	}
}

func (s *activeModelSelection) currentAlias(sessionID string) string {
	sessionID = strings.TrimSpace(sessionID)
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.currentAliasLocked(sessionID)
}

func (s *activeModelSelection) currentAliasLocked(sessionID string) string {
	if sessionID != "" {
		alias, observed := s.aliases[sessionID]
		if observed {
			return alias
		}
	}
	return s.defaultAlias
}

func (s *activeModelSelection) observe(sessionID string, route messageRoute) {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return
	}
	alias := ""
	if !route.firstPartyAnthropic {
		alias = route.model.Alias
	}
	s.mu.Lock()
	if route.agentChildRouted {
		// A reservation-routed child request must not change the parent session's
		// active alias. The parent may have selected a newer alias while the
		// child was in flight; only explicit top-level requests observe state.
		s.mu.Unlock()
		return
	}
	s.aliases[sessionID] = alias
	s.mu.Unlock()
}

func (h *handler) selectMessageRouteForRequest(ctx context.Context, sessionID string, req anthropicRequest) (messageRoute, *requestValidationError) {
	route, validationErr := h.selectRouteForRequest(ctx, sessionID, req)
	classifier := isAutoModeClassifierRequest(req)
	if validationErr == nil && (!classifier || classifierExplicitlySelectedRoute(req.Model, route)) {
		h.activeModel.observe(sessionID, route)
	}
	return route, validationErr
}

func classifierExplicitlySelectedRoute(requestedModel string, route messageRoute) bool {
	if route.firstPartyAnthropic || route.model.Alias == "" {
		return false
	}
	requestedModel = strings.TrimSpace(requestedModel)
	if requestedModel == route.model.Alias {
		return true
	}
	discovery := parseDiscoveryID(requestedModel)
	return discovery.valid && discovery.alias == route.model.Alias
}

func claudeCodeSessionID(r *http.Request) string {
	return strings.TrimSpace(r.Header.Get(claudeCodeSessionIDHeader))
}

func isAutoModeClassifierRequest(req anthropicRequest) bool {
	switch system := req.System.(type) {
	case string:
		return hasAutoModeClassifierPrefix(system)
	case []any:
		for _, item := range system {
			block, ok := item.(map[string]any)
			if !ok {
				continue
			}
			blockType, ok := block["type"].(string)
			if !ok || blockType != "text" {
				continue
			}
			text, ok := block["text"].(string)
			if ok && hasAutoModeClassifierPrefix(text) {
				return true
			}
		}
	}
	return false
}

func hasAutoModeClassifierPrefix(system string) bool {
	return strings.HasPrefix(strings.TrimSpace(system), autoModeClassifierSystemPrefix)
}
