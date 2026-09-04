// Package quota adapts the quota-calibration request from humeo/codex-switch
// to CodexMarathon's controller provider interface. It is product-owned code
// derived from the donor's internal/quota client: retry classification,
// header parsing, and 429 handling are retained, while results are normalized
// into the controller's multi-window telemetry model. No caller-facing error
// includes an authorization header or token value.
package quota

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"codexmarathon/controller/internal/telemetry"
)

var (
	ErrMissingToken       = errors.New("quota credentials are missing an access token")
	ErrMissingUsage       = errors.New("quota response did not include usage headers")
	ErrUnexpectedResponse = errors.New("quota provider returned an unexpected response")
	ErrIdentityMismatch   = errors.New("quota credentials identify a different account")
)

// DefaultModel mirrors codex-switch's default calibration model while keeping
// the model choice explicit for callers that want a different Codex model.
const (
	DefaultModel          = "gpt-5.4-mini"
	defaultRequestTimeout = 15 * time.Second
)

// Tokens are the minimum bearer/account identity accepted by the quota
// endpoint. They are intentionally not JSON tagged or printable through this
// package's errors.
type Tokens struct {
	AccessToken string
	AccountID   string
}

// Snapshot preserves the donor client's two-window view and tracks header
// presence so omitted usage cannot be mistaken for a real zero observation.
type Snapshot struct {
	Plan                 string
	PrimaryUsedPercent   float64
	SecondaryUsedPercent float64
	PrimaryResetAfter    time.Duration
	SecondaryResetAfter  time.Duration
	PrimaryResetAt       time.Time
	SecondaryResetAt     time.Time
	HasPrimary           bool
	HasSecondary         bool
	HasCredits           bool
	CreditsBalance       string
	RateLimited          bool
}

// ParseHeaders maps the Codex response headers used by codex-switch. It is
// deterministic and does not require a network request, making it suitable
// for fixture tests and for future runtime adapters.
func ParseHeaders(headers http.Header) (Snapshot, error) {
	var snapshot Snapshot
	if value := headers.Get("x-codex-plan-type"); value != "" {
		snapshot.Plan = value
	}
	if value := headers.Get("x-codex-primary-used-percent"); value != "" {
		percent, err := parsePercent(value, "x-codex-primary-used-percent")
		if err != nil {
			return Snapshot{}, err
		}
		snapshot.PrimaryUsedPercent = percent
		snapshot.HasPrimary = true
	}
	if value := headers.Get("x-codex-secondary-used-percent"); value != "" {
		percent, err := parsePercent(value, "x-codex-secondary-used-percent")
		if err != nil {
			return Snapshot{}, err
		}
		snapshot.SecondaryUsedPercent = percent
		snapshot.HasSecondary = true
	}
	if value := headers.Get("x-codex-primary-reset-after-seconds"); value != "" {
		seconds, err := strconv.ParseInt(value, 10, 64)
		if err != nil || seconds < 0 {
			return Snapshot{}, fmt.Errorf("parse x-codex-primary-reset-after-seconds")
		}
		snapshot.PrimaryResetAfter = time.Duration(seconds) * time.Second
	}
	if value := headers.Get("x-codex-secondary-reset-after-seconds"); value != "" {
		seconds, err := strconv.ParseInt(value, 10, 64)
		if err != nil || seconds < 0 {
			return Snapshot{}, fmt.Errorf("parse x-codex-secondary-reset-after-seconds")
		}
		snapshot.SecondaryResetAfter = time.Duration(seconds) * time.Second
	}
	if value := headers.Get("x-codex-primary-reset-at"); value != "" {
		seconds, err := strconv.ParseInt(value, 10, 64)
		if err != nil || seconds < 0 {
			return Snapshot{}, fmt.Errorf("parse x-codex-primary-reset-at")
		}
		snapshot.PrimaryResetAt = time.Unix(seconds, 0).UTC()
	}
	if value := headers.Get("x-codex-secondary-reset-at"); value != "" {
		seconds, err := strconv.ParseInt(value, 10, 64)
		if err != nil || seconds < 0 {
			return Snapshot{}, fmt.Errorf("parse x-codex-secondary-reset-at")
		}
		snapshot.SecondaryResetAt = time.Unix(seconds, 0).UTC()
	}
	if value := headers.Get("x-codex-credits-has-credits"); value != "" {
		credits, err := strconv.ParseBool(value)
		if err != nil {
			return Snapshot{}, fmt.Errorf("parse x-codex-credits-has-credits")
		}
		snapshot.HasCredits = credits
	}
	if value := headers.Get("x-codex-credits-balance"); value != "" {
		snapshot.CreditsBalance = value
	}
	return snapshot, nil
}

func parsePercent(value, field string) (float64, error) {
	percent, err := strconv.ParseFloat(value, 64)
	if err != nil || percent < 0 || percent > 100 {
		return 0, fmt.Errorf("parse %s", field)
	}
	return percent, nil
}

// Client performs the donor-compatible calibration request. BaseURL and
// HTTP are injectable for tests; production defaults target chatgpt.com.
type Client struct {
	BaseURL string
	HTTP    *http.Client
	Retries int
	Now     func() time.Time
}

// Check sends the smallest supported streamed Codex request and extracts
// quota headers. The endpoint response body is discarded and never included
// in errors, preventing provider pages from leaking into logs.
func (c Client) Check(ctx context.Context, tokens Tokens, model string) (Snapshot, error) {
	if strings.TrimSpace(tokens.AccessToken) == "" {
		return Snapshot{}, ErrMissingToken
	}
	if ctx == nil {
		ctx = context.Background()
	}
	baseURL := c.BaseURL
	if baseURL == "" {
		baseURL = "https://chatgpt.com"
	}
	client := c.HTTP
	if client == nil {
		client = &http.Client{Timeout: defaultRequestTimeout}
	}
	requestBody, err := json.Marshal(quotaRequest{
		Model: model,
		Input: []quotaMessage{{Role: "user", Content: "hi"}},
		Instructions: ".",
		Store:        false,
		Stream:       true,
		Reasoning:    quotaReasoning{Effort: "none"},
	})
	if err != nil {
		return Snapshot{}, errors.New("encode quota request")
	}
	endpoint, err := url.JoinPath(baseURL, "/backend-api/codex/responses")
	if err != nil {
		return Snapshot{}, errors.New("build quota endpoint")
	}
	retries := c.Retries
	if retries <= 0 {
		retries = 3
	}
	var lastErr error
	for attempt := 0; attempt < retries; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(requestBody))
		if err != nil {
			return Snapshot{}, errors.New("build quota request")
		}
		req.Header.Set("Authorization", "Bearer "+tokens.AccessToken)
		if tokens.AccountID != "" {
			req.Header.Set("ChatGPT-Account-Id", tokens.AccountID)
		}
		req.Header.Set("Content-Type", "application/json")
		response, err := client.Do(req)
		if err != nil {
			if !isRetryableError(err) || attempt == retries-1 {
				return Snapshot{}, errors.New("quota request failed")
			}
			lastErr = errors.New("quota request temporarily unavailable")
			sleepForAttempt(ctx, attempt)
			continue
		}
		snapshot, retryable, err := snapshotFromResponse(response)
		if err == nil && !retryable {
			return snapshot, nil
		}
		if !retryable || attempt == retries-1 {
			if err == nil {
				err = ErrUnexpectedResponse
			}
			return Snapshot{}, err
		}
		lastErr = err
		sleepForAttempt(ctx, attempt)
	}
	if lastErr != nil {
		return Snapshot{}, lastErr
	}
	return Snapshot{}, ErrUnexpectedResponse
}

type quotaRequest struct {
	Model        string         `json:"model"`
	Input        []quotaMessage `json:"input"`
	Instructions string         `json:"instructions"`
	Store        bool           `json:"store"`
	Stream       bool           `json:"stream"`
	Reasoning    quotaReasoning `json:"reasoning"`
}

type quotaMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type quotaReasoning struct {
	Effort string `json:"effort"`
}

func snapshotFromResponse(response *http.Response) (Snapshot, bool, error) {
	if response == nil {
		return Snapshot{}, true, ErrUnexpectedResponse
	}
	var body []byte
	if response.Body != nil {
		defer response.Body.Close()
		var readErr error
		body, readErr = io.ReadAll(io.LimitReader(response.Body, 64*1024))
		if readErr != nil {
			return Snapshot{}, true, errors.New("read quota response")
		}
	}
	switch response.StatusCode {
	case http.StatusOK:
		snapshot, err := ParseHeaders(response.Header)
		if err != nil {
			return Snapshot{}, false, err
		}
		// Preserve codex-switch's low-level client behavior: a successful
		// response is returned even when a provider omits one usage header.
		// Provider/SnapshotToTelemetry keeps that partial result unusable for
		// policy, so an omitted header can never authorize a switch.
		return snapshot, false, nil
	case http.StatusTooManyRequests:
		snapshot, err := ParseHeaders(response.Header)
		if err != nil {
			return Snapshot{}, false, err
		}
		// A 429 is authoritative evidence of exhaustion even when one of the
		// usage headers is omitted. Preserve any supplied reset timestamps.
		snapshot.PrimaryUsedPercent = 100
		snapshot.SecondaryUsedPercent = 100
		snapshot.HasPrimary = true
		snapshot.HasSecondary = true
		snapshot.RateLimited = true
		return snapshot, false, nil
	case http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return Snapshot{}, true, fmt.Errorf("quota provider temporary status %d", response.StatusCode)
	case http.StatusForbidden:
		if looksLikeHTMLChallenge(response.Header, body) {
			return Snapshot{}, true, errors.New("quota provider challenge")
		}
		return Snapshot{}, false, errors.New("quota provider rejected credentials")
	default:
		return Snapshot{}, false, fmt.Errorf("quota provider status %d", response.StatusCode)
	}
}

func looksLikeHTMLChallenge(headers http.Header, body []byte) bool {
	contentType := strings.ToLower(headers.Get("Content-Type"))
	if strings.Contains(contentType, "text/html") {
		return true
	}
	normalized := strings.ToLower(string(body))
	return strings.Contains(normalized, "<html") ||
		strings.Contains(normalized, "cf-chl") ||
		strings.Contains(normalized, "attention required") ||
		strings.Contains(normalized, "challenge")
}

func isRetryableError(err error) bool {
	var networkError net.Error
	return errors.As(err, &networkError) && (networkError.Timeout() || networkError.Temporary())
}

func sleepForAttempt(ctx context.Context, attempt int) {
	delay := time.Duration(10*(1<<attempt)) * time.Millisecond
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-timer.C:
	}
}

// CredentialSource is intentionally narrow. The integrated runtime or
// account manager supplies opaque profile credentials; quota never prints or
// persists them.
type CredentialSource interface {
	Tokens(context.Context, string) (Tokens, error)
}

type CredentialSourceFunc func(context.Context, string) (Tokens, error)

func (f CredentialSourceFunc) Tokens(ctx context.Context, accountID string) (Tokens, error) {
	return f(ctx, accountID)
}

// Provider turns the donor quota client into telemetry. It can be registered
// for inactive accounts in MultiAccountUsageRouter; active runtime events may
// use a separate provider backed by the runtime state store.
type Provider struct {
	Source CredentialSource
	Client Client
	Model  string
	Now    func() time.Time
}

func (p Provider) Snapshot(ctx context.Context, accountID string) (telemetry.SnapshotResult, error) {
	if p.Source == nil {
		err := errors.New("quota credential source is unavailable")
		return telemetry.UnusableSnapshot(err), err
	}
	tokens, err := p.Source.Tokens(ctx, accountID)
	if err != nil {
		safeErr := safeCredentialError(err)
		return telemetry.UnusableSnapshot(safeErr), safeErr
	}
	if tokens.AccountID != "" && tokens.AccountID != accountID {
		return telemetry.UnusableSnapshot(ErrIdentityMismatch), ErrIdentityMismatch
	}
	if tokens.AccountID == "" {
		tokens.AccountID = accountID
	}
	model := p.Model
	if strings.TrimSpace(model) == "" {
		model = DefaultModel
	}
	snapshot, err := p.Client.Check(ctx, tokens, model)
	if err != nil {
		return telemetry.UnusableSnapshot(err), err
	}
	observedAt := time.Now().UTC()
	if p.Now != nil {
		observedAt = p.Now().UTC()
	}
	account := SnapshotToTelemetry(accountID, snapshot, observedAt)
	if !account.IsUsable {
		err := ErrMissingUsage
		return telemetry.SnapshotResult{
			Telemetry:  account.Clone(),
			Snapshot:   account.Clone(),
			IsUsable:   false,
			ObservedAt: observedAt,
			Err:        err,
		}, err
	}
	return telemetry.UsableSnapshot(account), nil
}

func safeCredentialError(err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return errors.New("quota credentials unavailable")
}

// SnapshotToTelemetry converts a quota response into one normalized bucket.
// Reset-after metadata is resolved relative to the observation clock only
// when an absolute reset timestamp was not supplied.
func SnapshotToTelemetry(accountID string, snapshot Snapshot, observedAt time.Time) telemetry.AccountTelemetry {
	if observedAt.IsZero() {
		observedAt = time.Now().UTC()
	}
	primaryReset := resetPointer(snapshot.PrimaryResetAt, snapshot.PrimaryResetAfter, observedAt)
	secondaryReset := resetPointer(snapshot.SecondaryResetAt, snapshot.SecondaryResetAfter, observedAt)
	limit := telemetry.LimitTelemetry{
		LimitID:  "codex",
		LimitName: "codex",
		Windows: []telemetry.WindowTelemetry{},
	}
	if snapshot.HasPrimary {
		limit.Windows = append(limit.Windows, telemetry.WindowTelemetry{
			Kind:               telemetry.PrimaryWindow,
			UsedPercent:        snapshot.PrimaryUsedPercent,
			ResetsAt:           primaryReset,
			ObservedAt:         observedAt,
			Freshness:          telemetry.WindowFresh,
		})
	}
	if snapshot.HasSecondary {
		limit.Windows = append(limit.Windows, telemetry.WindowTelemetry{
			Kind:               telemetry.SecondaryWindow,
			UsedPercent:        snapshot.SecondaryUsedPercent,
			ResetsAt:           secondaryReset,
			ObservedAt:         observedAt,
			Freshness:          telemetry.WindowFresh,
		})
	}
	limit.RefreshStaleness(observedAt)
	return telemetry.AccountTelemetry{
		AccountID:  accountID,
		Limits:     map[string]telemetry.LimitTelemetry{"codex": limit},
		Aggregate:  &limit,
		ObservedAt: observedAt,
		IsUsable:   snapshot.HasPrimary && snapshot.HasSecondary,
		Source:     "quota_provider",
	}
}

func resetPointer(absolute time.Time, after time.Duration, observedAt time.Time) *time.Time {
	if !absolute.IsZero() {
		reset := absolute.UTC()
		return &reset
	}
	if after <= 0 {
		return nil
	}
	reset := observedAt.Add(after).UTC()
	return &reset
}
