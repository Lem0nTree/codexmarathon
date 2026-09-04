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

func TestProactiveThresholdSelectsDeterministicLowerPressureAccount(t *testing.T) {
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	engine := NewConfiguredEngine(EngineConfig{FreshnessTTL: time.Hour, Cooldown: time.Minute}, nil, func() time.Time { return now })
	decision := engine.EvaluateThreshold([]telemetry.AccountTelemetry{
		policyAccountWithWindows("account-z", []float64{20, 35}, now),
		policyAccountWithWindows("account-b", []float64{20, 10}, now),
		policyAccountWithWindows("account-a", []float64{20, 10}, now),
	}, "account-z", Thresholds{PrimaryPercent: 80, SecondaryPercent: 90}, now)
	if decision.Type != PolicyTransition || decision.AccountID != "account-a" {
		t.Fatalf("unexpected threshold decision: %#v", decision)
	}
	if decision.Rank.SecondaryUsedPercent != 10 || decision.Rank.PrimaryUsedPercent != 20 {
		t.Fatalf("unexpected candidate rank: %#v", decision.Rank)
	}
}

func TestUsageLimitExceededSelectsFreshReplacement(t *testing.T) {
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	resetAt := now.Add(time.Hour)
	engine := NewConfiguredEngine(EngineConfig{FreshnessTTL: time.Hour}, nil, func() time.Time { return now })
	decision := engine.EvaluateUsageLimitExceeded([]telemetry.AccountTelemetry{
		exhaustedPolicyAccount("account-a", resetAt, now),
		policyAccountWithWindows("account-b", []float64{5, 6}, now),
	}, "account-a", "usage-event-1", now)
	if decision.Type != PolicyTransition || decision.AccountID != "account-b" {
		t.Fatalf("unexpected hard-limit decision: %#v", decision)
	}
	duplicate := engine.EvaluateUsageLimitExceeded([]telemetry.AccountTelemetry{
		exhaustedPolicyAccount("account-a", resetAt, now),
		policyAccountWithWindows("account-b", []float64{5, 6}, now),
	}, "account-a", "usage-event-1", now)
	if duplicate.Type != PolicyStay {
		t.Fatalf("duplicate hard-limit event retriggered switch: %#v", duplicate)
	}
}

func TestThresholdDoesNotAuthorizeIncompleteOrStaleCandidate(t *testing.T) {
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	engine := NewConfiguredEngine(EngineConfig{FreshnessTTL: time.Hour}, nil, func() time.Time { return now })
	stale := now.Add(-2 * time.Hour)
	decision := engine.EvaluateThreshold([]telemetry.AccountTelemetry{
		policyAccountWithWindows("account-a", []float64{95, 20}, now),
		policyAccountWithWindows("account-b", []float64{1, 1}, stale),
		{AccountID: "account-c", IsUsable: true, ObservedAt: now, Limits: map[string]telemetry.LimitTelemetry{
			"partial": {LimitID: "partial", Windows: nil},
		}},
	}, "account-a", Thresholds{PrimaryPercent: 80, SecondaryPercent: 90}, now)
	if decision.Type != PolicyStay && decision.Type != PolicyNoTelemetry {
		t.Fatalf("invalid candidate authorized a switch: %#v", decision)
	}
	if decision.AccountID != "" {
		t.Fatalf("invalid candidate selected: %#v", decision)
	}
}

func TestPoolExhaustionKeepsEarliestResetAndRequestsRevalidation(t *testing.T) {
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	first := now.Add(15 * time.Minute)
	second := now.Add(30 * time.Minute)
	engine := NewConfiguredEngine(EngineConfig{FreshnessTTL: time.Hour}, nil, func() time.Time { return now })
	decision := engine.EvaluateUsageLimitExceeded([]telemetry.AccountTelemetry{
		exhaustedPolicyAccount("account-b", second, now),
		exhaustedPolicyAccount("account-a", first, now),
	}, "account-a", "hard-event", now)
	if decision.Type != PolicyWaitForReset || !decision.RefetchRequired {
		t.Fatalf("unexpected exhaustion decision: %#v", decision)
	}
	if decision.CandidateAccountID != "account-a" || !decision.ResetAt.Equal(first) {
		t.Fatalf("wrong earliest reset: %#v", decision)
	}
	if len(decision.RefreshAccountIDs) != 1 || decision.RefreshAccountIDs[0] != "account-a" {
		t.Fatalf("candidate was not included in revalidation: %#v", decision.RefreshAccountIDs)
	}
}

func TestTransitionCooldownPreventsImmediateLoop(t *testing.T) {
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	engine := NewConfiguredEngine(EngineConfig{FreshnessTTL: time.Hour, Cooldown: time.Minute}, nil, func() time.Time { return now })
	engine.RecordTransition("account-a", "account-b", now)
	decision := engine.EvaluateThreshold([]telemetry.AccountTelemetry{
		policyAccountWithWindows("account-a", []float64{95, 96}, now),
		policyAccountWithWindows("account-b", []float64{1, 2}, now),
	}, "account-a", Thresholds{PrimaryPercent: 80, SecondaryPercent: 90}, now.Add(10*time.Second))
	if decision.Type != PolicyCooldown {
		t.Fatalf("cooldown did not suppress repeated switch: %#v", decision)
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

func policyAccountWithWindows(accountID string, usage []float64, observedAt time.Time) telemetry.AccountTelemetry {
	windows := []telemetry.WindowTelemetry{{Kind: telemetry.PrimaryWindow, UsedPercent: usage[0], ObservedAt: observedAt, Freshness: telemetry.WindowFresh}, {Kind: telemetry.SecondaryWindow, UsedPercent: usage[1], ObservedAt: observedAt, Freshness: telemetry.WindowFresh}}
	return telemetry.AccountTelemetry{AccountID: accountID, IsUsable: true, ObservedAt: observedAt, Limits: map[string]telemetry.LimitTelemetry{"codex": {LimitID: "codex", Windows: windows}}}
}

func exhaustedPolicyAccount(accountID string, resetAt, observedAt time.Time) telemetry.AccountTelemetry {
	windows := []telemetry.WindowTelemetry{{Kind: telemetry.PrimaryWindow, UsedPercent: 100, ResetsAt: &resetAt, ObservedAt: observedAt, Freshness: telemetry.WindowFresh}, {Kind: telemetry.SecondaryWindow, UsedPercent: 100, ResetsAt: &resetAt, ObservedAt: observedAt, Freshness: telemetry.WindowFresh}}
	return telemetry.AccountTelemetry{AccountID: accountID, IsUsable: true, ObservedAt: observedAt, Limits: map[string]telemetry.LimitTelemetry{"codex": {LimitID: "codex", Windows: windows}}}
}
