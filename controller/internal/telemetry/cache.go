package telemetry

import (
	"sync"
	"time"
)

type cacheEntry struct {
	snapshot AccountTelemetry
	storedAt time.Time
}

// SnapshotCache is a TTL cache of immutable-by-convention account snapshots.
// It deep-copies on both ingress and egress, which is required because maps,
// slices, and pointers in AccountTelemetry otherwise share backing storage.
type SnapshotCache struct {
	mu      sync.RWMutex
	entries map[string]cacheEntry
	ttl     time.Duration
	now     func() time.Time
}

// NewSnapshotCache creates a cache.  A non-positive TTL disables age expiry;
// reset-boundary freshness is still computed independently on Get.
func NewSnapshotCache(ttl time.Duration, clock ...func() time.Time) *SnapshotCache {
	return &SnapshotCache{
		entries: make(map[string]cacheEntry),
		ttl:     ttl,
		now:     selectClock(clock...),
	}
}

// TTL returns the configured age bound.
func (c *SnapshotCache) TTL() time.Duration {
	if c == nil {
		return 0
	}
	return c.ttl
}

// Set stores a deep copy.  If the snapshot has no observation timestamp, the
// injected clock supplies one for TTL accounting and returned state.
func (c *SnapshotCache) Set(accountID string, snapshot AccountTelemetry) {
	if c == nil {
		return
	}
	clone := snapshot.Clone()
	if clone.AccountID == "" {
		clone.AccountID = accountID
	}
	storedAt := clone.ObservedAt
	if storedAt.IsZero() {
		storedAt = c.currentTime()
		clone.ObservedAt = storedAt
	}
	if accountID == "" {
		accountID = clone.AccountID
	}
	c.mu.Lock()
	if c.entries == nil {
		c.entries = make(map[string]cacheEntry)
	}
	c.entries[accountID] = cacheEntry{snapshot: clone, storedAt: storedAt}
	c.mu.Unlock()
}

// Put is an alias for Set.
func (c *SnapshotCache) Put(accountID string, snapshot AccountTelemetry) { c.Set(accountID, snapshot) }

// Get returns a deep copy.  An expired entry is removed and reported as a
// miss.  Freshness is evaluated on the returned copy, leaving the cached
// snapshot itself suitable for another caller at a different time.
func (c *SnapshotCache) Get(accountID string) (AccountTelemetry, bool) {
	if c == nil {
		return AccountTelemetry{}, false
	}
	now := c.currentTime()
	c.mu.Lock()
	entry, ok := c.entries[accountID]
	if !ok {
		c.mu.Unlock()
		return AccountTelemetry{}, false
	}
	if c.ttl > 0 && now.Sub(entry.storedAt) >= c.ttl {
		delete(c.entries, accountID)
		c.mu.Unlock()
		return AccountTelemetry{}, false
	}
	clone := entry.snapshot.Clone()
	c.mu.Unlock()
	clone.RefreshStaleness(now)
	return clone, true
}

// Snapshot is an alias for Get.
func (c *SnapshotCache) Snapshot(accountID string) (AccountTelemetry, bool) {
	return c.Get(accountID)
}

// Invalidate removes one account's cached observation.
func (c *SnapshotCache) Invalidate(accountID string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	delete(c.entries, accountID)
	c.mu.Unlock()
}

// InvalidateAll removes all cached observations.  It is the conservative
// option after an identity change when the controller cannot yet narrow the
// affected account set.
func (c *SnapshotCache) InvalidateAll() {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.entries = make(map[string]cacheEntry)
	c.mu.Unlock()
}

// Len returns the number of currently stored entries (including entries that
// may be age-expired but have not yet been requested).
func (c *SnapshotCache) Len() int {
	if c == nil {
		return 0
	}
	c.mu.RLock()
	length := len(c.entries)
	c.mu.RUnlock()
	return length
}

func (c *SnapshotCache) currentTime() time.Time {
	if c == nil || c.now == nil {
		return time.Now().UTC()
	}
	return c.now()
}
