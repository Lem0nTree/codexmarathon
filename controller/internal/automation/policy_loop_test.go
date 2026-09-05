package automation

import (
	"context"
	"errors"
	"testing"
	"time"

	"codexmarathon/controller/internal/policy"
	"codexmarathon/controller/internal/telemetry"
)

func TestEvaluateRefreshesInactiveAccountsAndRunsOneTransition(t *testing.T) {
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	store := telemetry.NewStateStore(func() time.Time { return now })
	router := telemetry.NewMultiAccountUsageRouter(nil, store)
	if err := router.Register("account-a", telemetry.ProviderFunc(func(context.Context, string) (telemetry.SnapshotResult, error) {
		return telemetry.UsableSnapshot(autoAccount("account-a", now, 95)), nil
	})); err != nil {
		t.Fatal(err)
	}
	if err := router.Register("account-b", telemetry.ProviderFunc(func(context.Context, string) (telemetry.SnapshotResult, error) {
		return telemetry.UsableSnapshot(autoAccount("account-b", now, 5)), nil
	})); err != nil {
		t.Fatal(err)
	}
	transitions := 0
	loop := New(Config{
		Router: router,
		Policy: policy.NewConfiguredEngine(policy.EngineConfig{FreshnessTTL: time.Hour}, nil, func() time.Time { return now }),
		Accounts: AccountSource{
			IDs:    func() ([]string, error) { return []string{"account-b", "account-a"}, nil },
			Active: func() (string, error) { return "account-a", nil },
		},
		Transition:   func(context.Context, string) error { transitions++; return nil },
		Thresholds:   policy.Thresholds{PrimaryPercent: 80, SecondaryPercent: 90},
		FreshnessTTL: time.Hour,
		Now:          func() time.Time { return now },
	})
	result, err := loop.Evaluate(context.Background(), Event{ID: "threshold-1", Type: EventThresholdReached, OccurredAt: now})
	if err != nil {
		t.Fatal(err)
	}
	if !result.TransitionRan || result.Decision.AccountID != "account-b" || transitions != 1 {
		t.Fatalf("result = %#v transitions=%d", result, transitions)
	}
}

func TestEvaluateDoesNotSwitchWhenProviderFails(t *testing.T) {
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	router := telemetry.NewMultiAccountUsageRouter(nil, telemetry.NewStateStore())
	_ = router.Register("account-a", telemetry.ProviderFunc(func(context.Context, string) (telemetry.SnapshotResult, error) {
		return telemetry.UnusableSnapshot(errors.New("network unavailable")), errors.New("network unavailable")
	}))
	_ = router.Register("account-b", telemetry.ProviderFunc(func(context.Context, string) (telemetry.SnapshotResult, error) {
		return telemetry.UnusableSnapshot(errors.New("network unavailable")), errors.New("network unavailable")
	}))
	transitions := 0
	loop := New(Config{
		Router: router,
		Policy: policy.NewEngine(nil, func() time.Time { return now }),
		Accounts: AccountSource{
			IDs:    func() ([]string, error) { return []string{"account-a", "account-b"}, nil },
			Active: func() (string, error) { return "account-a", nil },
		},
		Transition: func(context.Context, string) error { transitions++; return nil },
		Now:        func() time.Time { return now },
	})
	result, err := loop.Evaluate(context.Background(), Event{Type: EventUsageLimitExceeded, OccurredAt: now})
	if err != nil {
		t.Fatal(err)
	}
	if result.Decision.Type == policy.PolicyTransition || transitions != 0 {
		t.Fatalf("provider failures authorized transition: %#v", result)
	}
}

func TestRunReturnsOnCancellationWhileWaiting(t *testing.T) {
	now := time.Now().UTC()
	router := telemetry.NewMultiAccountUsageRouter(nil, telemetry.NewStateStore())
	_ = router.Register("account-a", telemetry.ProviderFunc(func(context.Context, string) (telemetry.SnapshotResult, error) {
		return telemetry.UnusableSnapshot(errors.New("no data")), errors.New("no data")
	}))
	loop := New(Config{
		Router: router,
		Policy: policy.NewEngine(nil),
		Accounts: AccountSource{
			IDs:    func() ([]string, error) { return []string{"account-a"}, nil },
			Active: func() (string, error) { return "account-a", nil },
		},
		Now:          func() time.Time { return now },
		PollInterval: time.Hour,
		MaxWaitSlice: time.Millisecond,
	})
	events := make(chan Event, 1)
	events <- Event{Type: EventUsageLimitExceeded, OccurredAt: now}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	timer := time.AfterFunc(20*time.Millisecond, cancel)
	defer timer.Stop()
	if err := loop.Run(ctx, events); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v, want context.Canceled", err)
	}
}

func autoAccount(accountID string, observedAt time.Time, usage float64) telemetry.AccountTelemetry {
	return telemetry.AccountTelemetry{AccountID: accountID, IsUsable: true, ObservedAt: observedAt, Limits: map[string]telemetry.LimitTelemetry{
		"codex": {LimitID: "codex", Windows: []telemetry.WindowTelemetry{{Kind: telemetry.PrimaryWindow, UsedPercent: usage, ObservedAt: observedAt, Freshness: telemetry.WindowFresh}}},
	}}
}
