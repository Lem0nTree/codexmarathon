package policy

import (
	"testing"
	"time"

	"codexmarathon/controller/internal/telemetry"
)

func TestEvaluateChoosesDeterministicFreshAccount(t *testing.T) {
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	decision := Evaluate([]telemetry.AccountTelemetry{
		policyAccount("account-b", 20, now.Add(time.Hour), now),
		policyAccount("account-a", 20, now.Add(time.Hour), now),
	}, now)
	if decision.Type != PolicyTransition || decision.AccountID != "account-a" {
		t.Fatalf("unexpected account decision: %#v", decision)
	}
}

func TestEvaluateWaitsForEarliestResetWhenPoolExhausted(t *testing.T) {
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	first := now.Add(10 * time.Minute)
	decision := Evaluate([]telemetry.AccountTelemetry{
		policyAccount("account-a", 100, first, now),
	}, now)
	if decision.Type != PolicyWaitForReset || decision.CandidateAccountID != "account-a" {
		t.Fatalf("unexpected reset decision: %#v", decision)
	}
	if !decision.ResetAt.Equal(first) {
		t.Fatalf("wrong reset time: %v", decision.ResetAt)
	}
}

func TestEvaluateDoesNotAssumeElapsedResetRestoredCapacity(t *testing.T) {
	now := time.Date(2026, 9, 4, 12, 1, 0, 0, time.UTC)
	resetAt := now.Add(-time.Minute)
	decision := Evaluate([]telemetry.AccountTelemetry{policyAccount("account-a", 100, resetAt, now)}, now)
	if decision.Type != PolicyNoTelemetry || !decision.RefetchRequired {
		t.Fatalf("elapsed reset should require telemetry, got %#v", decision)
	}
}

func policyAccount(accountID string, usage float64, resetAt, observedAt time.Time) telemetry.AccountTelemetry {
	window := telemetry.WindowTelemetry{
		Kind:       telemetry.PrimaryWindow,
		UsedPercent: usage,
		ResetsAt:   &resetAt,
		ObservedAt: observedAt,
		Freshness:  telemetry.WindowFresh,
	}
	return telemetry.AccountTelemetry{
		AccountID:  accountID,
		IsUsable:   true,
		ObservedAt: observedAt,
		Limits: map[string]telemetry.LimitTelemetry{
			"codex": {LimitID: "codex", Windows: []telemetry.WindowTelemetry{window}},
		},
	}
}
