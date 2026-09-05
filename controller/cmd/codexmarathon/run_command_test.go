package main

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"codexmarathon/controller/app"
	"codexmarathon/controller/internal/automation"
	"codexmarathon/controller/internal/policy"
	installedruntime "codexmarathon/controller/internal/runtime"
	"codexmarathon/controller/internal/telemetry"
)

func TestInstalledNotificationEventsDriveAccountTransitions(t *testing.T) {
	now := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name         string
		notification installedruntime.InstalledNotification
		wantType     automation.EventType
		wantThread   string
	}{
		{
			name: "rate limit update",
			notification: installedruntime.InstalledNotification{
				Method: "account/rateLimits/updated",
				Params: json.RawMessage(`{"rateLimits":{"primary":{"usedPercent":95}}}`),
			},
			wantType: automation.EventTelemetryChanged,
		},
		{
			name: "usage limit turn completion",
			notification: installedruntime.InstalledNotification{
				Method: "turn/completed",
				Params: json.RawMessage(`{"threadId":"thread-1","turn":{"id":"turn-1","status":"failed","error":{"code":"usage_limit_exceeded"}}}`),
			},
			wantType:   automation.EventUsageLimitExceeded,
			wantThread: "thread-1",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			event, threadID, ok := app.InstalledNotificationAutomationEvent(test.notification, "account-a", now)
			if !ok {
				t.Fatal("notification was not converted into an automation event")
			}
			if event.Type != test.wantType {
				t.Fatalf("event type = %q, want %q", event.Type, test.wantType)
			}
			if threadID != test.wantThread {
				t.Fatalf("thread ID = %q, want %q", threadID, test.wantThread)
			}

			router := telemetry.NewMultiAccountUsageRouter(nil, telemetry.NewStateStore(func() time.Time { return now }))
			if err := router.Register("account-a", telemetry.ProviderFunc(func(context.Context, string) (telemetry.SnapshotResult, error) {
				usage := 10.0
				if test.wantType == automation.EventTelemetryChanged {
					usage = 95
				}
				return telemetry.UsableSnapshot(installedTestAccount("account-a", usage, now)), nil
			})); err != nil {
				t.Fatal(err)
			}
			if err := router.Register("account-b", telemetry.ProviderFunc(func(context.Context, string) (telemetry.SnapshotResult, error) {
				return telemetry.UsableSnapshot(installedTestAccount("account-b", 10, now)), nil
			})); err != nil {
				t.Fatal(err)
			}

			var transitionedTo string
			loop := automation.New(automation.Config{
				Router: router,
				Policy: policy.NewConfiguredEngine(policy.EngineConfig{FreshnessTTL: time.Hour}, nil, func() time.Time { return now }),
				Accounts: automation.AccountSource{
					IDs:    func() ([]string, error) { return []string{"account-a", "account-b"}, nil },
					Active: func() (string, error) { return "account-a", nil },
				},
				Transition: func(_ context.Context, accountID string) error {
					transitionedTo = accountID
					return nil
				},
				Thresholds:   policy.DefaultThresholds(),
				FreshnessTTL: time.Hour,
				Now:          func() time.Time { return now },
			})
			result, err := loop.Evaluate(context.Background(), event)
			if err != nil {
				t.Fatal(err)
			}
			if !result.TransitionRan || transitionedTo != "account-b" {
				t.Fatalf("event did not execute replacement transition: result=%#v target=%q", result, transitionedTo)
			}
		})
	}
}

func installedTestAccount(accountID string, usage float64, observedAt time.Time) telemetry.AccountTelemetry {
	return telemetry.AccountTelemetry{
		AccountID:  accountID,
		IsUsable:   true,
		ObservedAt: observedAt,
		Limits: map[string]telemetry.LimitTelemetry{
			"codex": {
				LimitID: "codex",
				Windows: []telemetry.WindowTelemetry{{
					Kind:        telemetry.PrimaryWindow,
					UsedPercent: usage,
					ObservedAt:  observedAt,
					Freshness:   telemetry.WindowFresh,
				}},
			},
		},
	}
}
