package reset

import (
	"testing"
	"time"

	"codexmarathon/controller/internal/telemetry"
)

func TestSchedulerChoosesEarliestTrustworthyReset(t *testing.T) {
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	first := now.Add(15 * time.Minute)
	second := now.Add(35 * time.Minute)
	scheduler := NewScheduler(func() time.Time { return now })

	decision := scheduler.Evaluate([]telemetry.AccountTelemetry{
		exhaustedAccount("account-b", "codex", second, now),
		exhaustedAccount("account-a", "codex", first, now),
	}, now)
	if decision.State != ResetWaitForReset || !decision.PoolExhausted {
		t.Fatalf("expected pool wait, got %#v", decision)
	}
	if decision.CandidateAccountID != "account-a" || !decision.ResetAt.Equal(first) {
		t.Fatalf("wrong earliest candidate: %#v", decision)
	}
	next, ok := scheduler.NextRecheck()
	if !ok || !next.Equal(first) {
		t.Fatalf("wrong next recheck: %v, %v", next, ok)
	}
}

func TestElapsedResetRequiresFreshTelemetry(t *testing.T) {
	now := time.Date(2026, 9, 4, 12, 31, 0, 0, time.UTC)
	resetAt := now.Add(-time.Minute)
	scheduler := NewScheduler(func() time.Time { return now })
	decision := scheduler.Evaluate([]telemetry.AccountTelemetry{
		exhaustedAccount("account-a", "codex", resetAt, now),
	}, now)
	if decision.State != ResetWaitForData {
		t.Fatalf("elapsed reset should wait for data, got %#v", decision)
	}
	if !decision.RefetchRequired {
		t.Fatal("elapsed reset did not require revalidation")
	}
	if len(decision.RefreshAccountIDs) != 1 || decision.RefreshAccountIDs[0] != "account-a" {
		t.Fatalf("wrong revalidation accounts: %#v", decision.RefreshAccountIDs)
	}
	if _, ok := scheduler.NextRecheck(); ok {
		t.Fatal("elapsed reset left a stale scheduled wake-up")
	}
}

func TestMissingResetDoesNotSynthesizeFutureTime(t *testing.T) {
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	scheduler := NewScheduler(func() time.Time { return now })
	account := telemetry.AccountTelemetry{
		AccountID: "account-a",
		IsUsable:  true,
		Limits: map[string]telemetry.LimitTelemetry{
			"codex": {LimitID: "codex", Windows: []telemetry.WindowTelemetry{
				{Kind: telemetry.PrimaryWindow, UsedPercent: 100, Freshness: telemetry.WindowFresh},
			}},
		},
	}
	decision := scheduler.Evaluate([]telemetry.AccountTelemetry{account}, now)
	if decision.State != ResetWaitForData || !decision.RefetchRequired {
		t.Fatalf("missing reset metadata should wait for data, got %#v", decision)
	}
	if !decision.ResetAt.IsZero() {
		t.Fatalf("scheduler synthesized a reset time: %v", decision.ResetAt)
	}
}

func TestExplicitlyStaleWindowRequestsAccountRefresh(t *testing.T) {
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	scheduler := NewScheduler(func() time.Time { return now })
	account := telemetry.AccountTelemetry{
		AccountID: "account-a",
		IsUsable:  true,
		Limits: map[string]telemetry.LimitTelemetry{
			"codex": {LimitID: "codex", Windows: []telemetry.WindowTelemetry{
				{Kind: telemetry.PrimaryWindow, UsedPercent: 20, Freshness: telemetry.WindowStale},
			}},
		},
	}
	decision := scheduler.Evaluate([]telemetry.AccountTelemetry{account}, now)
	if decision.State != ResetWaitForData || !decision.RefetchRequired {
		t.Fatalf("stale window should wait for data, got %#v", decision)
	}
	if len(decision.RefreshAccountIDs) != 1 || decision.RefreshAccountIDs[0] != "account-a" {
		t.Fatalf("stale window refresh targets = %#v, want [account-a]", decision.RefreshAccountIDs)
	}
}

func TestSchedulerRevalidationRequestNeverMarksAccountReady(t *testing.T) {
	now := time.Date(2026, 9, 4, 12, 31, 0, 0, time.UTC)
	resetAt := now.Add(-time.Minute)
	scheduler := NewScheduler(func() time.Time { return now })
	account := exhaustedAccount("account-a", "codex", resetAt, now)
	request := scheduler.RevalidationRequest([]telemetry.AccountTelemetry{account}, now)
	if len(request.AccountIDs) != 1 || request.AccountIDs[0] != "account-a" {
		t.Fatalf("unexpected revalidation request: %#v", request)
	}
	// The old observation remains exhausted and stale until a provider returns
	// a new full snapshot; RevalidationRequest is data-only by design.
	if account.Limits["codex"].Windows[0].UsedPercent != 100 {
		t.Fatal("revalidation request mutated caller telemetry")
	}
}

func exhaustedAccount(accountID, limitID string, resetAt, observedAt time.Time) telemetry.AccountTelemetry {
	window := telemetry.WindowTelemetry{
		Kind:       telemetry.PrimaryWindow,
		UsedPercent: 100,
		ResetsAt:   &resetAt,
		ObservedAt: observedAt,
		Freshness:  telemetry.WindowFresh,
	}
	return telemetry.AccountTelemetry{
		AccountID:  accountID,
		IsUsable:   true,
		ObservedAt: observedAt,
		Limits: map[string]telemetry.LimitTelemetry{
			limitID: {LimitID: limitID, Windows: []telemetry.WindowTelemetry{window}},
		},
	}
}
