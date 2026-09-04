package telemetry

import (
	"context"
	"errors"
	"sort"
	"sync"
	"time"
)

// SnapshotResult carries a provider observation and its usability separately.
// A provider may return a structurally valid but unusable observation (for
// example, a stale cached response), so callers must not infer usability from
// a nil error alone.
type SnapshotResult struct {
	Telemetry  AccountTelemetry
	// Snapshot is retained as a compatibility alias for callers that use the
	// protocol vocabulary.  New code should use Telemetry.
	Snapshot   AccountTelemetry
	IsUsable   bool
	ObservedAt time.Time
	Err        error
}

// UsableSnapshot constructs a successful provider result.
func UsableSnapshot(snapshot AccountTelemetry) SnapshotResult {
	clone := snapshot.Clone()
	if !clone.IsUsable {
		clone.IsUsable = true
	}
	return SnapshotResult{
		Telemetry:  clone.Clone(),
		Snapshot:   clone.Clone(),
		IsUsable:   true,
		ObservedAt: clone.ObservedAt,
	}
}

// UnusableSnapshot constructs a non-usable result without treating it as an
// account with zero usage.
func UnusableSnapshot(err error) SnapshotResult {
	return SnapshotResult{IsUsable: false, Err: err}
}

// UsageProvider supplies a complete account snapshot.  The account ID is
// passed separately because the runtime connection is authoritative for the
// identity of sparse notifications.
type UsageProvider interface {
	Snapshot(context.Context, string) (SnapshotResult, error)
}

// ProviderFunc adapts a function to UsageProvider.
type ProviderFunc func(context.Context, string) (SnapshotResult, error)

func (f ProviderFunc) Snapshot(ctx context.Context, accountID string) (SnapshotResult, error) {
	return f(ctx, accountID)
}

// MultiAccountUsageRouter chooses the provider for an account and centralizes
// cache/state-store ingestion.  Active runtime telemetry and inactive-profile
// providers can therefore share one controller-facing abstraction without
// duplicate polling logic.
type MultiAccountUsageRouter struct {
	mu        sync.RWMutex
	providers map[string]UsageProvider
	cache     *SnapshotCache
	store     *StateStore
}

// UsageRouter is a concise name for the multi-account implementation.
type UsageRouter = MultiAccountUsageRouter

// NewMultiAccountUsageRouter creates a router.  Cache and store may be nil if
// a caller only wants provider dispatch; a non-nil cache is recommended for
// normal controller operation.
func NewMultiAccountUsageRouter(cache *SnapshotCache, store *StateStore) *MultiAccountUsageRouter {
	return &MultiAccountUsageRouter{
		providers: make(map[string]UsageProvider),
		cache:     cache,
		store:     store,
	}
}

// NewUsageRouter is an alias for NewMultiAccountUsageRouter.
func NewUsageRouter(cache *SnapshotCache, store *StateStore) *MultiAccountUsageRouter {
	return NewMultiAccountUsageRouter(cache, store)
}

// Register associates one account with a provider.
func (r *MultiAccountUsageRouter) Register(accountID string, provider UsageProvider) error {
	if r == nil {
		return errors.New("usage router is nil")
	}
	if accountID == "" {
		return errors.New("account ID is required")
	}
	if provider == nil {
		return errors.New("usage provider is nil")
	}
	r.mu.Lock()
	if r.providers == nil {
		r.providers = make(map[string]UsageProvider)
	}
	r.providers[accountID] = provider
	r.mu.Unlock()
	return nil
}

// SetProvider is an alias for Register.
func (r *MultiAccountUsageRouter) SetProvider(accountID string, provider UsageProvider) error {
	return r.Register(accountID, provider)
}

// Unregister removes an account provider.
func (r *MultiAccountUsageRouter) Unregister(accountID string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	delete(r.providers, accountID)
	r.mu.Unlock()
}

// Accounts returns registered provider IDs in deterministic order.
func (r *MultiAccountUsageRouter) Accounts() []string {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	accounts := make([]string, 0, len(r.providers))
	for accountID := range r.providers {
		accounts = append(accounts, accountID)
	}
	r.mu.RUnlock()
	sort.Strings(accounts)
	return accounts
}

// Snapshot returns a cached usable observation when available, otherwise it
// asks the registered account provider and stores a successful result.
func (r *MultiAccountUsageRouter) Snapshot(ctx context.Context, accountID string) (SnapshotResult, error) {
	return r.snapshot(ctx, accountID, false)
}

// Refresh bypasses the cache and obtains fresh telemetry from the provider.
func (r *MultiAccountUsageRouter) Refresh(ctx context.Context, accountID string) (SnapshotResult, error) {
	return r.snapshot(ctx, accountID, true)
}

func (r *MultiAccountUsageRouter) snapshot(ctx context.Context, accountID string, force bool) (SnapshotResult, error) {
	if r == nil {
		return UnusableSnapshot(errors.New("usage router is nil")), errors.New("usage router is nil")
	}
	if !force && r.cache != nil {
		if cached, ok := r.cache.Get(accountID); ok && cached.IsUsable {
			return UsableSnapshot(cached), nil
		}
	}
	r.mu.RLock()
	provider := r.providers[accountID]
	r.mu.RUnlock()
	if provider == nil {
		err := errors.New("no usage provider registered for account")
		return UnusableSnapshot(err), err
	}
	result, err := provider.Snapshot(ctx, accountID)
	if err != nil {
		result.IsUsable = false
		result.Err = err
		return result, err
	}
	telemetry := result.Telemetry
	if telemetry.AccountID == "" {
		telemetry = result.Snapshot
	}
	if telemetry.AccountID == "" {
		telemetry.AccountID = accountID
	}
	telemetry.IsUsable = result.IsUsable
	if result.ObservedAt.IsZero() {
		result.ObservedAt = telemetry.ObservedAt
	}
	result.Telemetry = telemetry.Clone()
	result.Snapshot = telemetry.Clone()
	if result.IsUsable {
		if r.cache != nil {
			r.cache.Set(accountID, telemetry)
		}
		if r.store != nil {
			r.store.Set(accountID, telemetry)
		}
	}
	return result, nil
}

// IngestFull stores a runtime's normalized full read in both state layers.
func (r *MultiAccountUsageRouter) IngestFull(accountID string, snapshot AccountTelemetry) {
	if r == nil {
		return
	}
	if r.cache != nil {
		r.cache.Set(accountID, snapshot)
	}
	if r.store != nil {
		r.store.Set(accountID, snapshot)
	}
}

// IngestSparse applies a runtime rolling update to the state store and
// invalidates the cache after a successful merge.  A refetch-required result
// is returned unchanged to the caller.
func (r *MultiAccountUsageRouter) IngestSparse(accountID string, update AccountRateLimitsUpdatedWire) SparseUpdateResult {
	if r == nil || r.store == nil {
		return SparseUpdateResult{
			Outcome:         SparseUpdateNoState,
			AccountID:       accountID,
			Reason:          "state store is unavailable",
			RefetchRequired: true,
		}
	}
	result := r.store.ApplySparseUpdate(accountID, update)
	if result.Applied() && r.cache != nil {
		// The store has merged a new observation; replace the cache with the
		// returned clone rather than leaving an older aggregate snapshot behind.
		r.cache.Set(accountID, result.Snapshot)
	}
	return result
}
