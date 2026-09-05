package telemetry

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestObserveRefreshesStaleCachedAccountOnce(t *testing.T) {
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	store := NewStateStore(func() time.Time { return now })
	cache := NewSnapshotCache(24*time.Hour, func() time.Time { return now })
	router := NewMultiAccountUsageRouter(cache, store)
	refreshes := 0
	if err := router.Register("account-a", ProviderFunc(func(context.Context, string) (SnapshotResult, error) {
		refreshes++
		return UsableSnapshot(observedAccount("account-a", now, 10)), nil
	})); err != nil {
		t.Fatal(err)
	}
	cache.Set("account-a", observedAccount("account-a", now.Add(-2*time.Hour), 90))
	store.Set("account-a", observedAccount("account-a", now.Add(-2*time.Hour), 90))
	observations, err := router.Observe(context.Background(), []string{"account-a"}, ObserveOptions{MaxAge: time.Hour, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	if len(observations) != 1 || observations[0].Err != nil || !observations[0].Refreshed {
		t.Fatalf("observations = %#v", observations)
	}
	if refreshes != 1 || observations[0].Telemetry.Limits["codex"].Windows[0].UsedPercent != 10 {
		t.Fatalf("refreshes=%d telemetry=%#v", refreshes, observations[0].Telemetry)
	}
}

func TestObserveKeepsProviderFailureUnusableAndContinuesBatch(t *testing.T) {
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	router := NewMultiAccountUsageRouter(nil, NewStateStore(func() time.Time { return now }))
	if err := router.Register("account-a", ProviderFunc(func(context.Context, string) (SnapshotResult, error) {
		return UnusableSnapshot(errors.New("provider unavailable")), errors.New("provider unavailable")
	})); err != nil {
		t.Fatal(err)
	}
	if err := router.Register("account-b", ProviderFunc(func(context.Context, string) (SnapshotResult, error) {
		return UsableSnapshot(observedAccount("account-b", now, 5)), nil
	})); err != nil {
		t.Fatal(err)
	}
	observations, err := router.Observe(context.Background(), []string{"account-b", "account-a"}, ObserveOptions{MaxAge: time.Hour, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	if len(observations) != 2 || observations[0].AccountID != "account-a" || observations[1].AccountID != "account-b" {
		t.Fatalf("order = %#v", observations)
	}
	if observations[0].Err == nil || observations[0].Telemetry.IsUsable {
		t.Fatalf("failed provider became usable: %#v", observations[0])
	}
	if observations[1].Err != nil || !observations[1].Telemetry.IsUsable {
		t.Fatalf("healthy provider lost: %#v", observations[1])
	}
}

func TestObserveDoesNotRefreshWithoutRegisteredProvider(t *testing.T) {
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	store := NewStateStore(func() time.Time { return now })
	cache := NewSnapshotCache(time.Hour, func() time.Time { return now })
	router := NewMultiAccountUsageRouter(cache, store)
	store.Set("account-a", observedAccount("account-a", now.Add(-2*time.Hour), 1))
	observations, err := router.Observe(context.Background(), []string{"account-a"}, ObserveOptions{MaxAge: time.Hour, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	if len(observations) != 1 || observations[0].Telemetry.HasFreshCapacityWithin(now, time.Hour) {
		t.Fatalf("stale unregistered account was eligible: %#v", observations)
	}
}

func observedAccount(accountID string, observedAt time.Time, usage float64) AccountTelemetry {
	return AccountTelemetry{
		AccountID:  accountID,
		ObservedAt: observedAt,
		IsUsable:   true,
		Limits:     map[string]LimitTelemetry{"codex": {LimitID: "codex", Windows: []WindowTelemetry{{Kind: PrimaryWindow, UsedPercent: usage, ObservedAt: observedAt, Freshness: WindowFresh}}}},
	}
}
