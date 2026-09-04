package quota

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"codexmarathon/controller/internal/telemetry"
)

func TestParseHeadersPreservesFractionalUsageAndResetMetadata(t *testing.T) {
	headers := http.Header{}
	headers.Set("x-codex-plan-type", "plus")
	headers.Set("x-codex-primary-used-percent", "12.5")
	headers.Set("x-codex-secondary-used-percent", "34")
	headers.Set("x-codex-primary-reset-after-seconds", "60")
	headers.Set("x-codex-secondary-reset-at", "1700000123")

	got, err := ParseHeaders(headers)
	if err != nil {
		t.Fatalf("ParseHeaders() error = %v", err)
	}
	if !got.HasPrimary || !got.HasSecondary || got.PrimaryUsedPercent != 12.5 || got.SecondaryUsedPercent != 34 {
		t.Fatalf("snapshot = %+v, want both fractional/header usages", got)
	}
	if got.PrimaryResetAfter != time.Minute || got.SecondaryResetAt.Unix() != 1700000123 {
		t.Fatalf("reset metadata = %+v", got)
	}
}

func TestCheckSendsDonorCalibrationRequestAndParsesBothWindows(t *testing.T) {
	client := Client{
		BaseURL: "https://example.test",
		HTTP: &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
			if req.Method != http.MethodPost || req.URL.Path != "/backend-api/codex/responses" {
				t.Fatalf("request = %s %s", req.Method, req.URL)
			}
			if req.Header.Get("Authorization") != "Bearer access-token" || req.Header.Get("ChatGPT-Account-Id") != "account-123" {
				t.Fatal("credential headers were not forwarded")
			}
			body, _ := io.ReadAll(req.Body)
			for _, want := range []string{`"model":"gpt-4.1"`, `"stream":true`, `"store":false`, `"effort":"none"`} {
				if !strings.Contains(string(body), want) {
					t.Fatalf("request body %q missing %s", body, want)
				}
			}
			headers := http.Header{}
			headers.Set("x-codex-primary-used-percent", "7")
			headers.Set("x-codex-secondary-used-percent", "9")
			return &http.Response{StatusCode: http.StatusOK, Header: headers, Body: io.NopCloser(strings.NewReader("{}"))}, nil
		})},
	}
	got, err := client.Check(context.Background(), Tokens{AccessToken: "access-token", AccountID: "account-123"}, "gpt-4.1")
	if err != nil {
		t.Fatalf("Check() error = %v", err)
	}
	if got.PrimaryUsedPercent != 7 || got.SecondaryUsedPercent != 9 {
		t.Fatalf("snapshot = %+v", got)
	}
}

func TestCheckTreats429AsExhaustedWithoutLeakingBody(t *testing.T) {
	client := Client{HTTP: &http.Client{Transport: roundTripperFunc(func(*http.Request) (*http.Response, error) {
		headers := http.Header{}
		headers.Set("x-codex-primary-reset-after-seconds", "60")
		return &http.Response{StatusCode: http.StatusTooManyRequests, Header: headers, Body: io.NopCloser(strings.NewReader("private challenge details"))}, nil
	})}}
	got, err := client.Check(context.Background(), Tokens{AccessToken: "token"}, "gpt-4.1")
	if err != nil {
		t.Fatalf("Check() error = %v", err)
	}
	if !got.RateLimited || !got.HasPrimary || !got.HasSecondary || got.PrimaryUsedPercent != 100 || got.SecondaryUsedPercent != 100 {
		t.Fatalf("429 snapshot = %+v", got)
	}
}

func TestProviderConvertsResetAfterToAuthoritativeFutureTimestamp(t *testing.T) {
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	provider := Provider{
		Source: CredentialSourceFunc(func(context.Context, string) (Tokens, error) {
			return Tokens{AccessToken: "token", AccountID: "account-a"}, nil
		}),
		Client: Client{HTTP: &http.Client{Transport: roundTripperFunc(func(*http.Request) (*http.Response, error) {
			headers := http.Header{}
			headers.Set("x-codex-primary-used-percent", "100")
			headers.Set("x-codex-secondary-used-percent", "25")
			headers.Set("x-codex-primary-reset-after-seconds", "600")
			return &http.Response{StatusCode: http.StatusOK, Header: headers, Body: io.NopCloser(strings.NewReader("{}"))}, nil
		})}},
		Now: func() time.Time { return now },
	}
	result, err := provider.Snapshot(context.Background(), "account-a")
	if err != nil {
		t.Fatalf("Snapshot() error = %v", err)
	}
	if !result.IsUsable {
		t.Fatal("provider returned unusable snapshot")
	}
	window := result.Telemetry.Limits["codex"].Windows[0]
	if window.Kind != telemetry.PrimaryWindow || window.ResetsAt == nil || !window.ResetsAt.Equal(now.Add(10*time.Minute)) {
		t.Fatalf("primary window = %+v", window)
	}
}

func TestSnapshotToTelemetryRejectsMissingWindowAsUnusable(t *testing.T) {
	account := SnapshotToTelemetry("account-a", Snapshot{HasPrimary: true, PrimaryUsedPercent: 1}, time.Now().UTC())
	if account.IsUsable {
		t.Fatal("missing secondary window was treated as usable")
	}
}

func TestProviderRejectsCredentialIdentityMismatch(t *testing.T) {
	provider := Provider{Source: CredentialSourceFunc(func(context.Context, string) (Tokens, error) {
		return Tokens{AccessToken: "token", AccountID: "account-b"}, nil
	})}
	result, err := provider.Snapshot(context.Background(), "account-a")
	if err != ErrIdentityMismatch || result.IsUsable {
		t.Fatalf("mismatched provider result = %#v, err=%v", result, err)
	}
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (fn roundTripperFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return fn(req)
}
