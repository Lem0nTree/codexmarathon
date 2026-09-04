package reset

import (
	"sort"
	"sync"
	"time"

	"codexmarathon/controller/internal/telemetry"
)

type resetCandidate struct {
	accountID  string
	limitID    string
	kind       telemetry.WindowKind
	resetAt    time.Time
	duration   int
}

// Scheduler chooses the earliest trustworthy future reset and records when
// the controller should re-observe the pool.  It is safe for a policy loop and
// timer loop to call its methods concurrently.
type Scheduler struct {
	mu      sync.Mutex
	now     func() time.Time
	next    time.Time
	hasNext bool
	wait    ResetWaitState
	state   ResetState
}

// NewScheduler creates a reset scheduler.  An optional clock is used by
// Evaluate when its now argument is zero and by wait-state entry timestamps.
func NewScheduler(clock ...func() time.Time) *Scheduler {
	return &Scheduler{now: selectClock(clock...), state: ResetReady}
}

// NewResetScheduler is a descriptive alias for NewScheduler.
func NewResetScheduler(clock ...func() time.Time) *Scheduler { return NewScheduler(clock...) }

// Evaluate inspects all currently observed accounts.  It never treats an
// elapsed reset timestamp as proof of availability: such windows are marked
// stale on the local copy and the decision requests fresh telemetry.
func (s *Scheduler) Evaluate(accounts []telemetry.AccountTelemetry, now time.Time) ResetDecision {
	if s == nil {
		return ResetDecision{
			State:           ResetWaitForData,
			PoolExhausted:   true,
			RefetchRequired: true,
			Reason:          "reset scheduler is nil",
		}
	}
	if now.IsZero() {
		now = s.currentTime()
	}

	// Work from independent copies so evaluating a decision cannot alter the
	// caller's snapshots or accidentally clear its reset metadata.
	clones := make([]telemetry.AccountTelemetry, len(accounts))
	for i, account := range accounts {
		clones[i] = account.Clone()
		clones[i].RefreshStaleness(now)
	}
	sort.SliceStable(clones, func(i, j int) bool { return clones[i].AccountID < clones[j].AccountID })

	for _, account := range clones {
		if account.HasFreshCapacity(now) {
			s.clearWithState(ResetReady)
			return ResetDecision{State: ResetReady, Reason: "eligible account has fresh capacity"}
		}
	}

	var candidates []resetCandidate
	refreshIDs := make(map[string]struct{})
	usableData := false
	for _, account := range clones {
		if !account.IsUsable {
			if account.AccountID != "" {
				refreshIDs[account.AccountID] = struct{}{}
			}
			continue
		}
		windowsForAccount := account.WindowList()
		if len(windowsForAccount) == 0 {
			if account.AccountID != "" {
				refreshIDs[account.AccountID] = struct{}{}
			}
			continue
		}
		usableData = true
		for _, limit := range account.LimitList() {
			windows := limit.WindowList()
			for _, window := range windows {
				if window.IsStaleAt(now) {
					// Any stale window invalidates the observation. Ask the provider
					// to refresh rather than declaring this account ready. A stale
					// marker may be explicit even when no reset timestamp is present
					// (or when the timestamp is still in the future), so do not limit
					// refresh targeting to elapsed reset metadata.
					if account.AccountID != "" {
						refreshIDs[account.AccountID] = struct{}{}
					}
					continue
				}
				if !window.IsExhausted() {
					continue
				}
				if !window.HasFutureReset(now) {
					if account.AccountID != "" {
						refreshIDs[account.AccountID] = struct{}{}
					}
					continue
				}
				duration := 0
				if window.WindowDurationMins != nil {
					duration = safeDuration(*window.WindowDurationMins)
				}
				candidates = append(candidates, resetCandidate{
					accountID: account.AccountID,
					limitID:   limit.LimitID,
					kind:      window.Kind,
					resetAt:   *window.ResetsAt,
					duration:  duration,
				})
			}
		}
	}

	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].resetAt != candidates[j].resetAt {
			return candidates[i].resetAt.Before(candidates[j].resetAt)
		}
		if candidates[i].accountID != candidates[j].accountID {
			return candidates[i].accountID < candidates[j].accountID
		}
		if candidates[i].limitID != candidates[j].limitID {
			return candidates[i].limitID < candidates[j].limitID
		}
		return candidates[i].kind < candidates[j].kind
	})

	refreshAccountIDs := sortedKeys(refreshIDs)
	if len(candidates) > 0 {
		candidate := candidates[0]
		wait := ResetWaitState{
			CandidateAccountID: candidate.accountID,
			LimitID:            candidate.limitID,
			WindowDurationMins: candidate.duration,
			ExpectedResetAt:    candidate.resetAt,
			EnteredAt:          now,
		}
		s.mu.Lock()
		s.state = ResetWaitForReset
		s.wait = wait
		s.next = candidate.resetAt
		s.hasNext = true
		s.mu.Unlock()
		return ResetDecision{
			State:              ResetWaitForReset,
			PoolExhausted:      true,
			CandidateAccountID: candidate.accountID,
			LimitID:            candidate.limitID,
			DurationMins:       candidate.duration,
			ResetAt:            candidate.resetAt,
			RefetchRequired:    len(refreshAccountIDs) > 0,
			RefreshAccountIDs:  refreshAccountIDs,
			Reason:             "all eligible accounts are exhausted; waiting for earliest server reset",
		}
	}

	// No future reset can be trusted.  This includes expired reset metadata and
	// accounts for which the provider did not supply a timestamp.
	s.clearWithState(ResetWaitForData)
	return ResetDecision{
		State:             ResetWaitForData,
		PoolExhausted:     true,
		RefetchRequired:   len(refreshAccountIDs) > 0 || usableData,
		RefreshAccountIDs: refreshAccountIDs,
		Reason:            "all eligible accounts are unavailable without a trustworthy future reset",
	}
}

// NextRecheck returns the earliest future reset selected by Evaluate.
func (s *Scheduler) NextRecheck() (time.Time, bool) {
	if s == nil {
		return time.Time{}, false
	}
	s.mu.Lock()
	next, ok := s.next, s.hasNext
	s.mu.Unlock()
	return next, ok
}

// Due reports whether a scheduled boundary has been reached.
func (s *Scheduler) Due(now time.Time) bool {
	if s == nil {
		return false
	}
	if now.IsZero() {
		now = s.currentTime()
	}
	next, ok := s.NextRecheck()
	return ok && !next.After(now)
}

// WaitState returns a copy of the current advisory waiting metadata.
func (s *Scheduler) WaitState() (ResetWaitState, bool) {
	if s == nil {
		return ResetWaitState{}, false
	}
	s.mu.Lock()
	wait, ok := s.wait, s.state == ResetWaitForReset
	s.mu.Unlock()
	return wait, ok
}

// State reports the last scheduler state.
func (s *Scheduler) State() ResetState {
	if s == nil {
		return ResetWaitForData
	}
	s.mu.Lock()
	state := s.state
	s.mu.Unlock()
	return state
}

// RevalidationRequest returns the account(s) that must be refreshed once the
// selected reset boundary has elapsed.  It does not mark them usable.
func (s *Scheduler) RevalidationRequest(accounts []telemetry.AccountTelemetry, now time.Time) RevalidationRequest {
	if s == nil {
		return RevalidationRequest{}
	}
	if now.IsZero() {
		now = s.currentTime()
	}
	ids := make(map[string]struct{})
	for _, account := range accounts {
		clone := account.Clone()
		clone.RefreshStaleness(now)
		for _, window := range clone.WindowList() {
			if window.ResetsAt != nil && !window.ResetsAt.After(now) {
				ids[clone.AccountID] = struct{}{}
				break
			}
		}
	}
	return RevalidationRequest{AccountIDs: sortedKeys(ids), DueAt: now}
}

// Clear forgets any scheduled reset and returns the scheduler to READY.  It
// does not alter caller-owned telemetry.
func (s *Scheduler) Clear() {
	if s == nil {
		return
	}
	s.clearWithState(ResetReady)
}

func (s *Scheduler) clearWithState(state ResetState) {
	s.mu.Lock()
	s.state = state
	s.next = time.Time{}
	s.hasNext = false
	s.wait = ResetWaitState{}
	s.mu.Unlock()
}

func safeDuration(value int64) int {
	if value <= 0 {
		return 0
	}
	maxInt := int64(^uint(0) >> 1)
	if value > maxInt {
		return int(maxInt)
	}
	return int(value)
}

func sortedKeys(values map[string]struct{}) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		if key != "" {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	return keys
}

func selectClock(clocks ...func() time.Time) func() time.Time {
	if len(clocks) > 0 && clocks[0] != nil {
		return clocks[0]
	}
	return func() time.Time { return time.Now().UTC() }
}

func (s *Scheduler) currentTime() time.Time {
	if s == nil || s.now == nil {
		return time.Now().UTC()
	}
	return s.now()
}
