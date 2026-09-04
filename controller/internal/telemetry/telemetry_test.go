package telemetry

import (
	"encoding/json"
	"testing"
	"time"
)

func TestSparseUpdateAbsentLimitIDIsAmbiguousWithoutMutation(t *testing.T) {
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	store := NewStateStore(func() time.Time { return now })
	store.Set("account-a", AccountTelemetry{
		AccountID: "account-a",
		IsUsable:  true,
		Limits: map[string]LimitTelemetry{
			"codex":  testLimit("codex", 10, now.Add(time.Hour)),
			"other":  testLimit("other", 20, now.Add(2*time.Hour)),
		},
		Aggregate: ptrLimit(testLimit("codex", 10, now.Add(time.Hour))),
	})

	result := store.ApplySparseUpdate("account-a", AccountRateLimitsUpdatedWire{
		RateLimits: RateLimitSnapshotWire{
			Primary: &RateLimitWindowWire{UsedPercent: 99},
		},
	})
	if result.Outcome != SparseUpdateRefetchRequired || !result.RefetchRequired {
		t.Fatalf("expected ambiguous update to request refetch, got %#v", result)
	}

	snapshot, ok := store.Get("account-a")
	if !ok {
		t.Fatal("snapshot disappeared after ambiguous update")
	}
	if got := snapshot.Limits["codex"].Windows[0].UsedPercent; got != 10 {
		t.Fatalf("ambiguous update mutated codex bucket: got %v", got)
	}
	if got := snapshot.Limits["other"].Windows[0].UsedPercent; got != 20 {
		t.Fatalf("ambiguous update mutated other bucket: got %v", got)
	}
}

func TestSparseUpdateByIDDoesNotOverwriteDifferentAggregate(t *testing.T) {
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	store := NewStateStore(func() time.Time { return now })
	store.Set("account-a", AccountTelemetry{
		AccountID: "account-a",
		IsUsable:  true,
		Limits: map[string]LimitTelemetry{
			"codex": testLimit("codex", 10, now.Add(time.Hour)),
			"team":  testLimit("team", 20, now.Add(2*time.Hour)),
		},
		Aggregate: ptrLimit(testLimit("codex", 10, now.Add(time.Hour))),
	})
	limitID := "team"
	result := store.ApplySparseUpdate("account-a", AccountRateLimitsUpdatedWire{
		RateLimits: RateLimitSnapshotWire{
			LimitID: &limitID,
			Primary: &RateLimitWindowWire{UsedPercent: 42},
		},
	})
	if !result.Applied() {
		t.Fatalf("expected targeted update to apply, got %#v", result)
	}
	snapshot, _ := store.Get("account-a")
	if got := snapshot.Limits["team"].Windows[0].UsedPercent; got != 42 {
		t.Fatalf("targeted team update missing: got %v", got)
	}
	if got := snapshot.Aggregate.Windows[0].UsedPercent; got != 10 {
		t.Fatalf("targeted team update overwrote aggregate: got %v", got)
	}
}

func TestPerWindowStalenessIsIndependent(t *testing.T) {
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	primaryReset := now.Add(-time.Minute)
	secondaryReset := now.Add(time.Hour)
	limit := LimitTelemetry{
		LimitID: "codex",
		Windows: []WindowTelemetry{
			{Kind: PrimaryWindow, UsedPercent: 100, ResetsAt: &primaryReset, Freshness: WindowFresh},
			{Kind: SecondaryWindow, UsedPercent: 30, ResetsAt: &secondaryReset, Freshness: WindowFresh},
		},
	}
	account := AccountTelemetry{IsUsable: true, Limits: map[string]LimitTelemetry{"codex": limit}}
	account.RefreshStaleness(now)
	windows := account.Limits["codex"].Windows
	if windows[0].Freshness != WindowStale {
		t.Fatalf("primary window should be stale: %#v", windows[0])
	}
	if windows[1].Freshness != WindowFresh {
		t.Fatalf("secondary window should remain fresh: %#v", windows[1])
	}
}

func TestSnapshotCacheDeepCopiesIngressAndEgress(t *testing.T) {
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	cache := NewSnapshotCache(time.Hour, func() time.Time { return now })
	resetAt := now.Add(time.Hour)
	source := AccountTelemetry{
		AccountID: "account-a",
		IsUsable:  true,
		Limits: map[string]LimitTelemetry{
			"codex": {
				LimitID: "codex",
				Windows: []WindowTelemetry{{Kind: PrimaryWindow, UsedPercent: 10, ResetsAt: &resetAt}},
			},
		},
	}
	cache.Set("account-a", source)
	source.Limits["codex"].Windows[0].UsedPercent = 77
	first, ok := cache.Get("account-a")
	if !ok {
		t.Fatal("cache miss after Set")
	}
	first.Limits["codex"].Windows[0].UsedPercent = 88
	if first.Limits["codex"].Windows[0].ResetsAt == nil {
		t.Fatal("deep copy lost reset timestamp")
	}
	*first.Limits["codex"].Windows[0].ResetsAt = now.Add(4 * time.Hour)
	second, ok := cache.Get("account-a")
	if !ok {
		t.Fatal("cache miss after caller mutation")
	}
	if got := second.Limits["codex"].Windows[0].UsedPercent; got != 10 {
		t.Fatalf("caller mutation changed cache usage: got %v", got)
	}
	if got := second.Limits["codex"].Windows[0].ResetsAt; !got.Equal(resetAt) {
		t.Fatalf("caller mutation changed cache reset: got %v", got)
	}
}

func TestWireWindowExplicitZeroIsPresent(t *testing.T) {
	var wire RateLimitWindowWire
	if err := json.Unmarshal([]byte(`{"usedPercent":0,"windowDurationMins":null,"resetsAt":null}`), &wire); err != nil {
		t.Fatal(err)
	}
	if !wire.HasUsedPercent() {
		t.Fatal("explicit zero usage was treated as absent")
	}
}

func TestSparseUpdateMissingUsageRetainsPriorWindowValue(t *testing.T) {
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	store := NewStateStore(func() time.Time { return now })
	store.Set("account-a", AccountTelemetry{
		AccountID: "account-a",
		IsUsable:  true,
		Limits: map[string]LimitTelemetry{
			"codex": testLimit("codex", 65, now.Add(time.Hour)),
		},
	})
	var update AccountRateLimitsUpdatedWire
	if err := json.Unmarshal([]byte(`{"rateLimits":{"limitId":"codex","primary":{"resetsAt":1725454800}}}`), &update); err != nil {
		t.Fatal(err)
	}
	result := store.ApplySparseUpdate("account-a", update)
	if !result.Applied() {
		t.Fatalf("expected sparse metadata update to apply, got %#v", result)
	}
	snapshot, _ := store.Get("account-a")
	if got := snapshot.Limits["codex"].Windows[0].UsedPercent; got != 65 {
		t.Fatalf("missing usage field reset prior usage to %v", got)
	}
}

func TestSnapshotCacheHonorsTTLWithInjectedClock(t *testing.T) {
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	cache := NewSnapshotCache(time.Minute, func() time.Time { return now })
	cache.Set("account-a", AccountTelemetry{AccountID: "account-a", IsUsable: true})
	if _, ok := cache.Get("account-a"); !ok {
		t.Fatal("cache unexpectedly missed before TTL")
	}
	now = now.Add(time.Minute)
	if _, ok := cache.Get("account-a"); ok {
		t.Fatal("cache returned an entry at its TTL boundary")
	}
}

func testLimit(id string, usage float64, reset time.Time) LimitTelemetry {
	window := WindowTelemetry{Kind: PrimaryWindow, UsedPercent: usage, ResetsAt: &reset, Freshness: WindowFresh}
	return LimitTelemetry{LimitID: id, Windows: []WindowTelemetry{window}}
}

func ptrLimit(limit LimitTelemetry) *LimitTelemetry { return &limit }
