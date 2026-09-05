package telemetry

import (
	"sort"
	"strings"
	"sync"
	"time"
)

// SparseUpdateOutcome describes whether a rolling notification could be
// attributed without guessing.
type SparseUpdateOutcome uint8

const (
	SparseUpdateApplied SparseUpdateOutcome = iota
	SparseUpdateRefetchRequired
	SparseUpdateNoState
)

func (o SparseUpdateOutcome) String() string {
	switch o {
	case SparseUpdateApplied:
		return "applied"
	case SparseUpdateRefetchRequired:
		return "refetch_required"
	case SparseUpdateNoState:
		return "no_state"
	default:
		return "unknown"
	}
}

// SparseUpdateResult gives the runtime adapter enough information to issue a
// complete account/rateLimits/read when a sparse update is ambiguous.
type SparseUpdateResult struct {
	Outcome         SparseUpdateOutcome
	AccountID       string
	TargetLimitID   string
	Reason          string
	RefetchRequired bool
	Snapshot        AccountTelemetry
}

func (r SparseUpdateResult) Applied() bool { return r.Outcome == SparseUpdateApplied }

// StateStore retains the most recent normalized snapshot per account.  It is
// also the authoritative merge point for sparse rolling updates.
type StateStore struct {
	mu        sync.RWMutex
	snapshots map[string]AccountTelemetry
	now       func() time.Time
}

// NewStateStore creates a store.  An optional clock makes reset-boundary tests
// deterministic; if omitted, UTC wall time is used.
func NewStateStore(clock ...func() time.Time) *StateStore {
	return &StateStore{
		snapshots: make(map[string]AccountTelemetry),
		now:       selectClock(clock...),
	}
}

// Set stores a full snapshot after cloning it.  The caller may safely reuse or
// mutate its input after this method returns.
func (s *StateStore) Set(accountID string, snapshot AccountTelemetry) {
	if s == nil {
		return
	}
	clone := snapshot.Clone()
	if clone.AccountID == "" {
		clone.AccountID = accountID
	}
	if accountID == "" {
		accountID = clone.AccountID
	}
	s.mu.Lock()
	if s.snapshots == nil {
		s.snapshots = make(map[string]AccountTelemetry)
	}
	s.snapshots[accountID] = clone
	s.mu.Unlock()
}

// Put is an alias for Set.
func (s *StateStore) Put(accountID string, snapshot AccountTelemetry) { s.Set(accountID, snapshot) }

// ApplyFullSnapshot replaces the account state and is intentionally distinct
// from ApplySparseUpdate in runtime adapters.
func (s *StateStore) ApplyFullSnapshot(accountID string, snapshot AccountTelemetry) {
	s.Set(accountID, snapshot)
}

// Get returns a deep copy and computes reset-boundary freshness on that copy.
func (s *StateStore) Get(accountID string) (AccountTelemetry, bool) {
	if s == nil {
		return AccountTelemetry{}, false
	}
	s.mu.RLock()
	snapshot, ok := s.snapshots[accountID]
	s.mu.RUnlock()
	if !ok {
		return AccountTelemetry{}, false
	}
	clone := snapshot.Clone()
	clone.RefreshStaleness(s.currentTime())
	return clone, true
}

// Snapshot is an alias for Get.
func (s *StateStore) Snapshot(accountID string) (AccountTelemetry, bool) {
	return s.Get(accountID)
}

// Delete removes one account's observed state.
func (s *StateStore) Delete(accountID string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	delete(s.snapshots, accountID)
	s.mu.Unlock()
}

// Accounts returns account IDs in deterministic order.
func (s *StateStore) Accounts() []string {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	accounts := make([]string, 0, len(s.snapshots))
	for accountID := range s.snapshots {
		accounts = append(accounts, accountID)
	}
	s.mu.RUnlock()
	sort.Strings(accounts)
	return accounts
}

// ApplySparseUpdate applies a rolling account/rateLimits/updated payload only
// after resolving its target bucket.  When the limit ID is absent and more
// than one bucket is known, no field is mutated and RefetchRequired is true.
func (s *StateStore) ApplySparseUpdate(accountID string, update AccountRateLimitsUpdatedWire) SparseUpdateResult {
	result := SparseUpdateResult{
		Outcome:   SparseUpdateNoState,
		AccountID: accountID,
	}
	if s == nil {
		result.Reason = "state store is nil"
		return result
	}

	observedAt := s.currentTime()
	s.mu.Lock()
	existing, ok := s.snapshots[accountID]
	if !ok {
		s.mu.Unlock()
		result.Outcome = SparseUpdateNoState
		result.Reason = "no full snapshot is available"
		result.RefetchRequired = true
		return result
	}

	targetKey, targetID, updateAggregate, reason := resolveSparseTarget(existing, update.RateLimits)
	result.TargetLimitID = targetID
	if reason != "" {
		s.mu.Unlock()
		result.Outcome = SparseUpdateRefetchRequired
		result.RefetchRequired = true
		result.Reason = reason
		result.Snapshot = existing.Clone()
		return result
	}

	// Resolve first, then mutate.  This ordering is important: an ambiguous
	// notification must leave both aggregate and by-ID state untouched.
	updated := existing.Clone()
	limit, present := updated.Limits[targetKey]
	if !present {
		if updated.Aggregate == nil {
			s.mu.Unlock()
			result.Outcome = SparseUpdateRefetchRequired
			result.RefetchRequired = true
			result.Reason = "resolved bucket is not present"
			result.Snapshot = existing.Clone()
			return result
		}
		limit = updated.Aggregate.Clone()
	}
	mergeLimitSparse(&limit, update.RateLimits, observedAt)
	updated.Limits[targetKey] = limit.Clone()

	// The aggregate view is coherent only when the target is the aggregate
	// bucket.  A by-ID update for bucket B must not overwrite aggregate bucket A.
	if updateAggregate && updated.Aggregate != nil {
		aggregate := updated.Aggregate.Clone()
		mergeLimitSparse(&aggregate, update.RateLimits, observedAt)
		updated.Aggregate = &aggregate
		if aggregateKey := aggregateKeyFor(updated, targetKey); aggregateKey != "" {
			updated.Limits[aggregateKey] = aggregate.Clone()
		}
	}

	updated.ObservedAt = observedAt
	updated.AccountID = accountID
	s.snapshots[accountID] = updated.Clone()
	result.Outcome = SparseUpdateApplied
	result.Snapshot = updated.Clone()
	s.mu.Unlock()
	return result
}

// ApplySparseWire is a readable alias for protocol adapters.
func (s *StateStore) ApplySparseWire(accountID string, update AccountRateLimitsUpdatedWire) SparseUpdateResult {
	return s.ApplySparseUpdate(accountID, update)
}

func resolveSparseTarget(existing AccountTelemetry, incoming RateLimitSnapshotWire) (key, id string, updateAggregate bool, reason string) {
	if incoming.LimitID != nil && strings.TrimSpace(*incoming.LimitID) != "" {
		id = *incoming.LimitID
		if _, ok := existing.Limits[id]; ok {
			return id, id, sameAggregateID(existing, id), ""
		}
		if existing.Aggregate != nil && existing.Aggregate.LimitID == id {
			aggregateKey := aggregateKeyFor(existing, "")
			if aggregateKey == "" {
				aggregateKey = AggregateLimitKey
			}
			return aggregateKey, id, true, ""
		}
		return "", id, false, "incoming limitId does not match a known bucket"
	}

	keys := bucketKeys(existing)
	switch len(keys) {
	case 0:
		if existing.Aggregate != nil {
			key := aggregateKeyFor(existing, "")
			if key == "" {
				key = AggregateLimitKey
			}
			return key, existing.Aggregate.LimitID, true, ""
		}
		if _, ok := existing.Limits[AggregateLimitKey]; ok {
			return AggregateLimitKey, "", true, ""
		}
		return "", "", false, "no bucket can receive an aggregate sparse update"
	case 1:
		key = keys[0]
		limit := existing.Limits[key]
		if existing.Aggregate != nil && existing.Aggregate.LimitID != "" {
			id = existing.Aggregate.LimitID
		} else {
			id = limit.LimitID
		}
		return key, id, sameAggregateKey(existing, key), ""
	default:
		return "", "", false, "limitId is absent and bucket attribution is ambiguous"
	}
}

func bucketKeys(snapshot AccountTelemetry) []string {
	keys := make([]string, 0, len(snapshot.Limits))
	for key := range snapshot.Limits {
		if key == AggregateLimitKey {
			continue
		}
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func sameAggregateID(snapshot AccountTelemetry, id string) bool {
	if snapshot.Aggregate == nil {
		return false
	}
	if snapshot.Aggregate.LimitID == id {
		return true
	}
	return snapshot.Aggregate.LimitID == "" && len(bucketKeys(snapshot)) == 1
}

func sameAggregateKey(snapshot AccountTelemetry, key string) bool {
	if snapshot.Aggregate == nil {
		return false
	}
	if key == AggregateLimitKey {
		return true
	}
	if snapshot.Aggregate.LimitID == "" && len(bucketKeys(snapshot)) == 1 {
		return true
	}
	return snapshot.Aggregate.LimitID != "" && snapshot.Aggregate.LimitID == key
}

func aggregateKeyFor(snapshot AccountTelemetry, preferred string) string {
	if preferred != "" {
		if _, ok := snapshot.Limits[preferred]; ok {
			return preferred
		}
	}
	if snapshot.Aggregate != nil && snapshot.Aggregate.LimitID != "" {
		if _, ok := snapshot.Limits[snapshot.Aggregate.LimitID]; ok {
			return snapshot.Aggregate.LimitID
		}
	}
	if _, ok := snapshot.Limits[AggregateLimitKey]; ok {
		return AggregateLimitKey
	}
	if len(snapshot.Limits) == 1 {
		for key := range snapshot.Limits {
			return key
		}
	}
	return ""
}

func mergeLimitSparse(target *LimitTelemetry, incoming RateLimitSnapshotWire, observedAt time.Time) {
	if target == nil {
		return
	}
	if incoming.LimitID != nil {
		target.LimitID = *incoming.LimitID
	}
	if incoming.LimitName != nil {
		target.LimitName = *incoming.LimitName
	}
	if incoming.PlanType != nil {
		target.PlanType = *incoming.PlanType
	}
	if incoming.RateLimitReachedType != nil {
		target.RateLimitReachedType = *incoming.RateLimitReachedType
	}

	windows := target.WindowList()
	mergeWindow := func(kind WindowKind, wire *RateLimitWindowWire) {
		if wire == nil {
			return
		}
		index := -1
		for i := range windows {
			if windows[i].Kind == kind {
				index = i
				break
			}
		}
		if index < 0 {
			// A future sparse payload may omit usedPercent even though the
			// current schema requires it. Do not create a new window with an
			// invented zero usage value; the next full read must establish that
			// window's usage authoritatively.
			if !wire.HasUsedPercent() {
				return
			}
			windows = append(windows, NormalizeWindow(*wire, kind, observedAt))
			return
		}
		window := &windows[index]
		// A non-nil nested wire object carries the usage value, including an
		// explicit zero after a real server reset.  Nullable metadata is merged
		// only when supplied so sparse updates never erase known values. If a
		// future payload omits usedPercent, retain the prior authoritative usage
		// instead of interpreting the Go zero value as a reset.
		if wire.HasUsedPercent() {
			window.UsedPercent = wire.UsedPercent
		}
		if wire.WindowDurationMins != nil {
			window.WindowDurationMins = cloneInt64(wire.WindowDurationMins)
		}
		if wire.ResetsAt != nil {
			reset := time.Unix(*wire.ResetsAt, 0).UTC()
			window.ResetsAt = &reset
		}
		window.ObservedAt = observedAt
		window.Freshness = WindowFresh
		window.RefreshStaleness(observedAt)
	}
	mergeWindow(PrimaryWindow, incoming.Primary)
	mergeWindow(SecondaryWindow, incoming.Secondary)
	target.Windows = windows
	syncConvenienceWindows(target)
	target.RefreshStaleness(observedAt)
}

func syncConvenienceWindows(limit *LimitTelemetry) {
	if limit == nil {
		return
	}
	limit.Primary = nil
	limit.Secondary = nil
	for _, window := range limit.Windows {
		clone := window.Clone()
		switch clone.Kind {
		case PrimaryWindow:
			limit.Primary = &clone
		case SecondaryWindow:
			limit.Secondary = &clone
		}
	}
}

func selectClock(clocks ...func() time.Time) func() time.Time {
	if len(clocks) > 0 && clocks[0] != nil {
		return clocks[0]
	}
	return func() time.Time { return time.Now().UTC() }
}

func (s *StateStore) currentTime() time.Time {
	if s == nil || s.now == nil {
		return time.Now().UTC()
	}
	return s.now()
}
