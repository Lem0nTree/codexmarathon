package app

// This file is the controller-side adapter for the user's installed Codex
// app-server.  The custom Marathon protocol remains available through
// ConnectRuntime for the optional embedded/runtime fixture, but the product
// path uses these methods to talk to the process the user already installed.

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"codexmarathon/controller/internal/automation"
	"codexmarathon/controller/internal/credentials"
	installedruntime "codexmarathon/controller/internal/runtime"
	"codexmarathon/controller/internal/telemetry"
)

var (
	// ErrInstalledAuthNotApplied means the installed app-server acknowledged
	// account/read but did not report that the newly deployed auth was adopted.
	// Callers must treat this as a failed live transition and may select the
	// owned process restart fallback.
	ErrInstalledAuthNotApplied = errors.New("installed Codex did not apply the new authentication")
	// ErrInstalledIdentityMismatch means the installed process reported an
	// identity different from the selected profile after reload.
	ErrInstalledIdentityMismatch = errors.New("installed Codex identity does not match the selected account")
	// ErrInstalledFallbackUnavailable means a live control operation is not
	// available and no Codex process owned by the companion was supplied for a
	// safe restart/resume.
	ErrInstalledFallbackUnavailable = errors.New("controlled Codex restart requires a process launched by CodexMarathon")
)

// InstalledTransitionMode records which installed-Codex boundary completed.
type InstalledTransitionMode string

const (
	InstalledTransitionLive    InstalledTransitionMode = "live-reload"
	InstalledTransitionRestart InstalledTransitionMode = "restart-resume"
)

// InstalledTransitionResult contains only transition metadata. It never
// retains auth JSON or provider token fields.
type InstalledTransitionResult struct {
	Mode              InstalledTransitionMode `json:"mode"`
	AccountID         string                  `json:"account_id"`
	PreviousAccountID string                  `json:"previous_account_id,omitempty"`
	Version           string                  `json:"codex_version,omitempty"`
	AuthChanged       bool                    `json:"auth_changed"`
	IdentityVerified  bool                    `json:"identity_verified"`
	ResumedThread     string                  `json:"resumed_thread,omitempty"`
	ResumedLast       bool                    `json:"resumed_last,omitempty"`
}

// DiscoverInstalledCodex exposes the read-only installed-Codex discovery
// used by the command layer. It runs --version/--help only and never reads
// auth.json.
func DiscoverInstalledCodex(ctx context.Context, options installedruntime.DiscoveryOptions) (installedruntime.Installation, error) {
	return installedruntime.DiscoverInstalled(ctx, options)
}

// ConnectInstalledCodex attaches to the user's private app-server control
// socket and performs the standard initialize/initialized handshake.
func ConnectInstalledCodex(ctx context.Context, installation installedruntime.Installation) (*installedruntime.InstalledClient, error) {
	return installedruntime.ConnectInstalled(ctx, installation)
}

// ConnectInstalledCodexEndpoint is the explicit endpoint form used by local
// test harnesses and operators. Network WebSockets are limited to loopback by
// the runtime client.
func ConnectInstalledCodexEndpoint(ctx context.Context, endpoint string) (*installedruntime.InstalledClient, error) {
	return installedruntime.ConnectInstalledEndpoint(ctx, endpoint)
}

// NewInstalledQuotaProvider returns a controller usage provider backed by the
// installed Codex app-server. The getter is evaluated for every observation so
// an owned restart can replace the app-server connection without leaving the
// automation loop attached to a closed client.
func NewInstalledQuotaProvider(getClient func() *installedruntime.InstalledClient) telemetry.UsageProvider {
	return telemetry.ProviderFunc(func(ctx context.Context, accountID string) (telemetry.SnapshotResult, error) {
		if getClient == nil {
			err := errors.New("installed Codex app-server client is unavailable")
			return telemetry.UnusableSnapshot(err), err
		}
		client := getClient()
		if client == nil {
			err := installedruntime.ErrAppServerUnavailable
			return telemetry.UnusableSnapshot(err), err
		}
		raw, err := client.ReadRateLimits(ctx)
		if err != nil {
			return telemetry.UnusableSnapshot(err), err
		}
		response, err := telemetry.DecodeRateLimitsResponse(raw)
		if err != nil {
			wrapped := fmt.Errorf("decode installed Codex rate limits: %w", err)
			return telemetry.UnusableSnapshot(wrapped), wrapped
		}
		snapshot := telemetry.NormalizeRateLimits(accountID, response, time.Now().UTC())
		if len(snapshot.WindowList()) == 0 {
			err := errors.New("installed Codex rate-limit response has no usage windows")
			return telemetry.UnusableSnapshot(err), err
		}
		return telemetry.UsableSnapshot(snapshot), nil
	})
}

// IngestInstalledRateLimitsUpdate merges one standard sparse rolling update
// into controller telemetry. Ambiguous or out-of-order updates invalidate the
// cache and force a complete account/rateLimits/read through the registered
// installed provider, so policy never evaluates an older cached snapshot as
// if it were the new server state.
func (c *Controller) IngestInstalledRateLimitsUpdate(ctx context.Context, accountID string, params json.RawMessage) error {
	if c == nil || c.router == nil {
		return errors.New("usage router is unavailable")
	}
	accountID = strings.TrimSpace(accountID)
	if accountID == "" {
		return errors.New("installed rate-limit update has no active account")
	}
	var update telemetry.AccountRateLimitsUpdatedWire
	if err := json.Unmarshal(params, &update); err != nil {
		if c.cache != nil {
			c.cache.Invalidate(accountID)
		}
		return fmt.Errorf("decode installed Codex rate-limit update: %w", err)
	}
	result := c.router.IngestSparse(accountID, update)
	if !result.RefetchRequired {
		return nil
	}
	if c.cache != nil {
		c.cache.Invalidate(accountID)
	}
	if _, err := c.router.Refresh(ctx, accountID); err != nil {
		return fmt.Errorf("refresh installed Codex rate limits after update: %w", err)
	}
	return nil
}

// VerifyInstalledIdentity reads the standard account object after a process
// restart and compares it with both the selected profile and the atomically
// deployed auth snapshot. Callers should invoke this before treating a
// restart/resume fallback as committed; the generic transition method returns
// IdentityVerified=false until its caller performs this post-reconnect check.
func (c *Controller) VerifyInstalledIdentity(ctx context.Context, client *installedruntime.InstalledClient, accountID string) (bool, error) {
	if c == nil {
		return false, errors.New("nil controller")
	}
	if client == nil {
		return false, installedruntime.ErrAppServerUnavailable
	}
	accountID = strings.TrimSpace(accountID)
	if accountID == "" {
		return false, errors.New("target account id is required")
	}
	account, err := client.ReadAccount(ctx)
	if err != nil {
		return false, err
	}
	return verifyInstalledIdentity(c.Config().AuthPath, accountID, account.Account)
}

// InstalledNotificationAutomationEvent maps stable standard app-server
// notifications into secret-free automation wake-ups. It also returns the
// most recent thread ID because the installed Codex protocol carries that
// resume target on turn lifecycle notifications.
//
// Standard Codex app-server versions do not expose Marathon's parked
// continuation methods. A completed turn whose error identifies a usage or
// rate-limit exhaustion therefore wakes account policy; the caller can run a
// live account/read transition or the companion-owned restart/resume fallback.
func InstalledNotificationAutomationEvent(notification installedruntime.InstalledNotification, accountID string, now time.Time) (automation.Event, string, bool) {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	now = now.UTC()
	method := strings.TrimSpace(notification.Method)
	threadID, turnID := installedNotificationIDs(notification.Params)
	event := automation.Event{Type: automation.EventTelemetryChanged, AccountID: strings.TrimSpace(accountID), OccurredAt: now}
	switch method {
	case "account/rateLimits/updated", "account/updated":
		return event, threadID, true
	case "turn/completed":
		if !installedUsageLimitPayload(notification.Params) {
			return automation.Event{}, threadID, false
		}
		event.Type = automation.EventUsageLimitExceeded
		if turnID != "" {
			event.ID = "turn/" + turnID
		} else {
			digest := sha256.Sum256(notification.Params)
			event.ID = fmt.Sprintf("turn/%x", digest[:8])
		}
		return event, threadID, true
	default:
		return automation.Event{}, threadID, false
	}
}

func installedNotificationIDs(raw json.RawMessage) (threadID, turnID string) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", ""
	}
	var params map[string]json.RawMessage
	if json.Unmarshal(raw, &params) != nil {
		return "", ""
	}
	threadID = jsonStringField(params, "threadId", "thread_id", "threadID")
	if turn, ok := params["turn"]; ok {
		var turnObject map[string]json.RawMessage
		if json.Unmarshal(turn, &turnObject) == nil {
			turnID = jsonStringField(turnObject, "id", "turnId", "turn_id", "turnID")
			if threadID == "" {
				threadID = jsonStringField(turnObject, "threadId", "thread_id", "threadID")
			}
		}
	}
	return strings.TrimSpace(threadID), strings.TrimSpace(turnID)
}

func jsonStringField(object map[string]json.RawMessage, keys ...string) string {
	for _, key := range keys {
		candidate, ok := object[key]
		if !ok {
			continue
		}
		var value string
		if json.Unmarshal(candidate, &value) == nil && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func installedUsageLimitPayload(raw json.RawMessage) bool {
	value := strings.ToLower(string(raw))
	for _, marker := range []string{
		"usage_limit",
		"usage-limit",
		"usage limit",
		"ratelimit",
		"rate_limit",
		"rate-limit",
		"rate limit",
		"quota_exceeded",
		"quota-exceeded",
		"quota exceeded",
	} {
		if strings.Contains(value, marker) {
			return true
		}
	}
	return false
}

// TransitionInstalled performs the preferred live transition. The account
// manager atomically deploys the selected opaque snapshot only after a
// successful safe-boundary preflight. The installed app-server then reloads
// auth through account/read, invalidates its own account-bound transports,
// and reports the resulting identity.
func (c *Controller) TransitionInstalled(ctx context.Context, client *installedruntime.InstalledClient, accountID string) (InstalledTransitionResult, error) {
	if c == nil {
		return InstalledTransitionResult{}, errors.New("nil controller")
	}
	if client == nil {
		return InstalledTransitionResult{}, installedruntime.ErrAppServerUnavailable
	}
	if ctx == nil {
		ctx = context.Background()
	}
	accountID = strings.TrimSpace(accountID)
	if accountID == "" {
		return InstalledTransitionResult{}, errors.New("target account id is required")
	}
	if !client.HasCapability(installedruntime.CapabilityAuthReload) {
		return InstalledTransitionResult{}, fmt.Errorf("%w: account/read reloadAuthFromStorage is unavailable", installedruntime.ErrCapabilityUnsupported)
	}
	if _, err := c.AccountManager().Status(accountID); err != nil {
		return InstalledTransitionResult{}, err
	}
	previousID, err := c.Registry().ActiveID()
	if err != nil {
		return InstalledTransitionResult{}, err
	}
	previousAuth, hadPreviousAuth, err := readOptionalAuth(c.Config().AuthPath)
	if err != nil {
		return InstalledTransitionResult{}, err
	}

	// This request is intentionally made before changing auth.json. The
	// installed app-server is authoritative for active turns; a stale local
	// notification counter cannot authorize a write during a running turn.
	if _, err := client.ReloadAuth(ctx); err != nil {
		return InstalledTransitionResult{}, err
	}
	if err := contextError(ctx); err != nil {
		return InstalledTransitionResult{}, err
	}
	if _, err := c.AccountManager().Activate(ctx, accountID); err != nil {
		return InstalledTransitionResult{}, err
	}

	reloaded, reloadErr := client.ReloadAuth(ctx)
	if reloadErr != nil {
		return InstalledTransitionResult{}, c.rollbackInstalledTransition(accountID, previousID, previousAuth, hadPreviousAuth, reloadErr)
	}
	if accountID != previousID && !reloaded.AuthChanged {
		return InstalledTransitionResult{}, c.rollbackInstalledTransition(accountID, previousID, previousAuth, hadPreviousAuth, ErrInstalledAuthNotApplied)
	}

	verified, verifyErr := verifyInstalledIdentity(c.Config().AuthPath, accountID, reloaded.Account)
	if verifyErr != nil {
		return InstalledTransitionResult{}, c.rollbackInstalledTransition(accountID, previousID, previousAuth, hadPreviousAuth, verifyErr)
	}
	return InstalledTransitionResult{
		Mode:              InstalledTransitionLive,
		AccountID:         accountID,
		PreviousAccountID: previousID,
		Version:           client.Version(),
		AuthChanged:       reloaded.AuthChanged,
		IdentityVerified:  verified,
	}, nil
}

// TransitionInstalledOrRestart prefers TransitionInstalled and selects the
// controlled process boundary only when the installed app-server explicitly
// lacks live reload. The fallback never signals a process that the companion
// did not launch.
func (c *Controller) TransitionInstalledOrRestart(ctx context.Context, client *installedruntime.InstalledClient, supervisor *CompanionSupervisor, accountID string, resume CompanionResumeTarget) (InstalledTransitionResult, error) {
	result, err := c.TransitionInstalled(ctx, client, accountID)
	if err == nil {
		return result, nil
	}
	if errors.Is(err, installedruntime.ErrActiveTurn) {
		return result, err
	}
	if !errors.Is(err, installedruntime.ErrCapabilityUnsupported) && !errors.Is(err, installedruntime.ErrAppServerUnavailable) {
		return result, err
	}
	if supervisor == nil {
		return result, errors.Join(err, ErrInstalledFallbackUnavailable)
	}
	if resume.ThreadID == "" && !resume.Last {
		resume.Last = true
	}
	if client != nil {
		_ = client.Close()
	}
	if err := supervisor.RestartAndResume(ctx, resume, func(deployCtx context.Context) error {
		_, activateErr := c.AccountManager().Activate(deployCtx, accountID)
		return activateErr
	}); err != nil {
		return InstalledTransitionResult{}, err
	}
	return InstalledTransitionResult{
		Mode:             InstalledTransitionRestart,
		AccountID:        accountID,
		IdentityVerified: false,
		ResumedThread:    strings.TrimSpace(resume.ThreadID),
		ResumedLast:      resume.Last,
	}, nil
}

// rollbackInstalledTransition restores both controller authorities after a
// live reload failure. Account manager activation is deliberately not used
// here: it would make a second deployment look like a new transition.
func (c *Controller) rollbackInstalledTransition(accountID, previousID string, previousAuth []byte, hadPreviousAuth bool, cause error) error {
	var restoreErr error
	if hadPreviousAuth {
		restoreErr = credentials.WriteAtomically(c.Config().AuthPath, previousAuth)
	} else if err := os.Remove(c.Config().AuthPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		restoreErr = err
	}
	if previousID == "" {
		if err := c.Registry().ClearActive(); err != nil {
			restoreErr = errors.Join(restoreErr, err)
		}
	} else if err := c.Registry().SetActive(previousID); err != nil {
		restoreErr = errors.Join(restoreErr, err)
	}
	return errors.Join(cause, restoreErr)
}

func readOptionalAuth(path string) ([]byte, bool, error) {
	if strings.TrimSpace(path) == "" {
		return nil, false, errors.New("auth path is empty")
	}
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return raw, true, nil
}

func verifyInstalledIdentity(authPath, accountID string, reported json.RawMessage) (bool, error) {
	raw, err := os.ReadFile(authPath)
	if err != nil {
		return false, fmt.Errorf("read deployed Codex auth identity: %w", err)
	}
	deployedID, err := credentials.ExtractAccountID(raw)
	if err != nil {
		return false, err
	}
	if deployedID != "" && deployedID != accountID {
		return false, fmt.Errorf("%w: deployed=%q target=%q", ErrInstalledIdentityMismatch, deployedID, accountID)
	}
	if reportedID := extractReportedAccountID(reported); reportedID != "" && reportedID != accountID {
		return false, fmt.Errorf("%w: reported=%q target=%q", ErrInstalledIdentityMismatch, reportedID, accountID)
	}
	return true, nil
}

func extractReportedAccountID(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var object map[string]json.RawMessage
	if json.Unmarshal(raw, &object) != nil || object == nil {
		return ""
	}
	for _, key := range []string{"id", "accountId", "account_id", "accountID"} {
		var value string
		if candidate, ok := object[key]; ok && json.Unmarshal(candidate, &value) == nil && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func contextError(ctx context.Context) error {
	if ctx == nil {
		return nil
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
		return nil
	}
}
