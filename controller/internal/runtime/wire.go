// Package runtime contains the typed controller-side boundary for a
// CodexMarathon runtime adapter. The package intentionally models the small
// Marathon contract, not Codext's internal AuthManager or recovery machinery.
package runtime

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
)

// ProtocolVersion is the currently supported Marathon wire version.
const ProtocolVersion = 1

// ProtocolName is the JSON-RPC notification namespace owned by Marathon.
const ProtocolName = "codexmarathon"

// Method is a namespaced JSON-RPC method name.
type Method string

const (
	// MethodNegotiateVersion selects one protocol version for a connection.
	MethodNegotiateVersion Method = "protocol/negotiate"
	// MethodGetRuntimeState reads identity and the authoritative turn count.
	MethodGetRuntimeState Method = "runtime/state/read"
	// MethodPrepareAuthTransition records intent before the controller deploys auth.
	MethodPrepareAuthTransition Method = "auth/transition/prepare"
	// MethodCommitAuthTransition asks Codext to reload the deployed auth at a safe boundary.
	MethodCommitAuthTransition Method = "auth/transition/commit"
	// MethodCancelAuthTransition abandons a pending transition.
	MethodCancelAuthTransition Method = "auth/transition/cancel"
	// MethodGetIdentity reads the runtime's observed identity.
	MethodGetIdentity Method = "runtime/identity/read"
	// MethodGetAuthGeneration reads the runtime's monotonic auth generation.
	MethodGetAuthGeneration Method = "runtime/authGeneration/read"
	// MethodLoginAccount starts a native Codex login flow in the embedded
	// runtime and returns the resulting opaque auth snapshot in memory. The
	// controller persists it only in its protected credential vault.
	MethodLoginAccount Method = "account/login"
	// MethodRefreshAccount asks the embedded runtime's native AuthManager/login
	// crate to refresh one opaque account snapshot.
	MethodRefreshAccount Method = "account/refresh"
	// MethodReadAuthSnapshot reads the active opaque AuthManager snapshot so
	// the controller can synchronize refreshed Account A credentials before a
	// live switch deploys Account B.
	MethodReadAuthSnapshot Method = "account/authSnapshot/read"
	// MethodReleaseRecovery releases one recovery turn that Codex parked after
	// UsageLimitExceeded. The runtime owns the queued prompt and the
	// conversation; the controller only authorizes its already-correlated
	// dispatch after an identity-changing transition is verified.
	MethodReleaseRecovery Method = "recovery/release"
)

// EventNotificationMethod is the JSON-RPC method used for runtime events.
const EventNotificationMethod Method = "codexmarathon/event"

// EventType identifies an event in an event notification's params object.
type EventType string

const (
	EventRuntimeReady        EventType = "runtime_ready"
	EventRateLimitsSnapshot  EventType = "rate_limits_snapshot"
	EventRateLimitsUpdated   EventType = "rate_limits_updated"
	EventTurnStarted         EventType = "turn_started"
	EventTurnCompleted       EventType = "turn_completed"
	EventSafeBoundaryReached EventType = "safe_boundary_reached"
	EventAuthReloadStarted   EventType = "auth_reload_started"
	EventAuthReloadSucceeded EventType = "auth_reload_succeeded"
	EventAuthReloadFailed    EventType = "auth_reload_failed"
	EventIdentityChanged     EventType = "identity_changed"
	EventRecoveryParked      EventType = "recovery_parked"
	EventRecoveryStarted     EventType = "recovery_started"
	EventRecoveryCompleted   EventType = "recovery_completed"
)

// TransitionOutcome describes the result observed by the runtime.
type TransitionOutcome string

const (
	TransitionCommitted TransitionOutcome = "committed"
	TransitionRejected  TransitionOutcome = "rejected"
	TransitionUncertain TransitionOutcome = "uncertain"
)

// Identity is the runtime-observed account identity and generation.
//
// AccountID is nullable because a runtime may be unauthenticated or may not
// yet have a backend identity. RuntimeID and AuthGeneration remain present in
// that state so controller reconciliation can still identify the runtime.
type Identity struct {
	RuntimeID      string  `json:"runtime_id"`
	AccountID      *string `json:"account_id"`
	AuthGeneration uint64  `json:"auth_generation"`
}

// PendingTransition is the runtime's prepared but not yet committed intent.
type PendingTransition struct {
	TransitionID       string `json:"transition_id"`
	TargetAccountID    string `json:"target_account_id"`
	ExpectedGeneration uint64 `json:"expected_generation"`
}

// RuntimeState is the minimum state needed to coordinate a safe transition.
type RuntimeState struct {
	Identity          Identity           `json:"identity"`
	ActiveTurnCount   int                `json:"active_turn_count"`
	PendingTransition *PendingTransition `json:"pending_transition"`
	// Recoveries is runtime-owned recovery state. It is optional for older
	// peers; when present it lets a restarted controller reconcile a release
	// acknowledgement without submitting a second continuation.
	Recoveries []RecoveryState `json:"recoveries,omitempty"`
}

// VersionNegotiationParams advertises versions supported by a peer.
type VersionNegotiationParams struct {
	SupportedVersions []int `json:"supported_versions"`
}

// VersionNegotiationResult is returned after selecting the common version.
type VersionNegotiationResult struct {
	ProtocolVersion int   `json:"protocol_version"`
	ServerVersions  []int `json:"server_versions"`
}

// AuthTransitionParams correlates prepare and commit with one controller-owned
// transition and rejects commands that target an old generation.
type AuthTransitionParams struct {
	TransitionID       string `json:"transition_id"`
	TargetAccountID    string `json:"target_account_id"`
	ExpectedGeneration uint64 `json:"expected_generation"`
}

// CancelAuthTransitionParams identifies a pending transition to cancel.
type CancelAuthTransitionParams struct {
	TransitionID       string `json:"transition_id"`
	ExpectedGeneration uint64 `json:"expected_generation"`
	Reason             string `json:"reason,omitempty"`
}

// TransitionResult is the typed acknowledgment returned by transition
// commands. Events provide the subsequent state changes and carry the same
// transition_id/runtime_id/auth_generation correlation fields.
type TransitionResult struct {
	TransitionID       string            `json:"transition_id"`
	RuntimeID          string            `json:"runtime_id"`
	ExpectedGeneration uint64            `json:"expected_generation,omitempty"`
	AuthGeneration     uint64            `json:"auth_generation"`
	Outcome            TransitionOutcome `json:"outcome"`
	AccountID          *string           `json:"account_id"`
	ErrorCode          *string           `json:"error_code,omitempty"`
	ErrorMessage       *string           `json:"error_message,omitempty"`
}

// AuthGenerationResult is returned by runtime/authGeneration/read.
type AuthGenerationResult struct {
	AuthGeneration uint64 `json:"auth_generation"`
}

// NativeLoginParams is the non-secret operator intent sent to the embedded
// Codex login runtime. The runtime owns OAuth/browser/device login.
type NativeLoginParams struct {
	AccountID string `json:"account_id,omitempty"`
	Alias     string `json:"alias,omitempty"`
	Overwrite bool   `json:"overwrite,omitempty"`
}

// NativeLoginResult is the in-memory handoff from the integrated runtime.
// AuthJSON is intentionally never logged or included in journal records.
type NativeLoginResult struct {
	AccountID string            `json:"account_id"`
	Alias     string            `json:"alias,omitempty"`
	AuthJSON  json.RawMessage   `json:"auth_json"`
	Metadata  map[string]string `json:"metadata,omitempty"`
}

// NativeRefreshParams supplies the target and opaque snapshot to the native
// runtime. This keeps provider-specific token parsing in Codex's login crate.
type NativeRefreshParams struct {
	AccountID string          `json:"account_id"`
	AuthJSON  json.RawMessage `json:"auth_json"`
}

// NativeRefreshResult contains only token fields needed for controller-side
// write-back. It is never printed by the CLI.
type NativeRefreshResult struct {
	AccessToken  string `json:"access_token,omitempty"`
	IDToken      string `json:"id_token,omitempty"`
	RefreshToken string `json:"refresh_token,omitempty"`
	AccountID    string `json:"account_id,omitempty"`
}

// NativeAuthSnapshotResult is the in-memory handoff of the active native
// AuthManager snapshot. AuthJSON is never written to transition journals or
// normal logs; the coordinator writes it only to the protected account vault.
type NativeAuthSnapshotResult struct {
	AccountID string          `json:"account_id"`
	AuthJSON  json.RawMessage `json:"auth_json"`
}

// RecoveryPhase is the runtime's observable lifecycle for Codext's parked
// UsageLimitExceeded recovery. The controller never supplies prompt text;
// Codext retains and dispatches that native synthetic turn.
type RecoveryPhase string

const (
	RecoveryParkedPhase    RecoveryPhase = "parked"
	RecoveryReleasedPhase  RecoveryPhase = "released"
	RecoveryStartedPhase   RecoveryPhase = "started"
	RecoveryCompletedPhase RecoveryPhase = "completed"
)

// RecoveryState contains only correlation metadata. It intentionally has no
// prompt or credential fields so it is safe to persist in a recovery journal.
type RecoveryState struct {
	RecoveryID         string        `json:"recovery_id"`
	ThreadID           string        `json:"thread_id,omitempty"`
	TurnID             string        `json:"turn_id,omitempty"`
	SourceAccountID    string        `json:"source_account_id,omitempty"`
	TransitionID       string        `json:"transition_id,omitempty"`
	TargetAccountID    string        `json:"target_account_id,omitempty"`
	ExpectedGeneration uint64        `json:"expected_generation,omitempty"`
	Phase              RecoveryPhase `json:"phase"`
}

// RecoveryReleaseParams identifies the one native parked recovery to release.
// TransitionID and ExpectedGeneration bind the release to a verified account
// switch and prevent a stale controller from releasing under Account A.
type RecoveryReleaseParams struct {
	RecoveryID         string `json:"recovery_id"`
	ThreadID           string `json:"thread_id,omitempty"`
	TransitionID       string `json:"transition_id"`
	ExpectedGeneration uint64 `json:"expected_generation"`
}

// RecoveryReleaseOutcome is the runtime acknowledgement of a release request.
type RecoveryReleaseOutcome string

const (
	RecoveryReleased         RecoveryReleaseOutcome = "released"
	RecoveryAlreadyReleased  RecoveryReleaseOutcome = "already_released"
	RecoveryReleaseRejected  RecoveryReleaseOutcome = "rejected"
	RecoveryReleaseUncertain RecoveryReleaseOutcome = "uncertain"
)

// RecoveryReleaseResult is secret-free and idempotently repeatable for one
// recovery ID. A repeated request may return already_released without
// dispatching the native prompt again.
type RecoveryReleaseResult struct {
	RecoveryID   string                 `json:"recovery_id"`
	ThreadID     string                 `json:"thread_id,omitempty"`
	Outcome      RecoveryReleaseOutcome `json:"outcome"`
	ErrorCode    *string                `json:"error_code,omitempty"`
	ErrorMessage *string                `json:"error_message,omitempty"`
}

// RateLimitWindowWire is the intentionally narrow Marathon representation of
// an upstream rate-limit window. Unknown upstream fields are ignored by Go's
// JSON decoder and can be added later as real typed fields when consumed.
type RateLimitWindowWire struct {
	UsedPercent        float64 `json:"usedPercent"`
	WindowDurationMins *int64  `json:"windowDurationMins"`
	ResetsAt           *int64  `json:"resetsAt"`
}

// RateLimitSnapshotWire contains only fields currently used for policy and
// reset scheduling. The pointers preserve upstream null versus a value.
type RateLimitSnapshotWire struct {
	LimitID              *string              `json:"limitId"`
	LimitName            *string              `json:"limitName"`
	PlanType             *string              `json:"planType"`
	RateLimitReachedType *string              `json:"rateLimitReachedType"`
	Primary              *RateLimitWindowWire `json:"primary"`
	Secondary            *RateLimitWindowWire `json:"secondary"`
}

// GetAccountRateLimitsResponseWire is the full active-account snapshot. The
// by-ID map is optional on older runtimes; a nil map is not an empty update.
type GetAccountRateLimitsResponseWire struct {
	RateLimits     RateLimitSnapshotWire            `json:"rateLimits"`
	RateLimitsByID map[string]RateLimitSnapshotWire `json:"rateLimitsByLimitId"`
}

// AccountRateLimitsUpdatedWire is a sparse rolling update. Callers must merge
// it against prior state or request a full snapshot when attribution is
// ambiguous; null metadata does not clear a prior value.
type AccountRateLimitsUpdatedWire struct {
	RateLimits RateLimitSnapshotWire `json:"rateLimits"`
}

// EventBase is shared by all runtime events. Transition events additionally
// require a non-empty TransitionID.
type EventBase struct {
	EventType      EventType `json:"event_type"`
	OccurredAt     int64     `json:"occurred_at"`
	RuntimeID      string    `json:"runtime_id"`
	AuthGeneration uint64    `json:"auth_generation"`
	TransitionID   string    `json:"transition_id,omitempty"`
}

// Event is the forward-compatible event representation delivered by Client.
// Payload contains the event-specific params after the common fields have
// been removed. DecodePayload provides typed values for known v1 events.
type Event struct {
	EventBase
	Payload json.RawMessage `json:"-"`
}

// RuntimeReadyEvent announces the runtime identity and negotiated protocol.
type RuntimeReadyEvent struct {
	EventBase
	ProtocolVersion int      `json:"protocol_version"`
	AccountID       *string  `json:"account_id"`
	Capabilities    []string `json:"capabilities,omitempty"`
}

// RateLimitsSnapshotEvent is a complete active-account snapshot.
type RateLimitsSnapshotEvent struct {
	EventBase
	AccountID      *string                          `json:"account_id"`
	RateLimits     RateLimitSnapshotWire            `json:"rateLimits"`
	RateLimitsByID map[string]RateLimitSnapshotWire `json:"rateLimitsByLimitId"`
}

// RateLimitsUpdatedEvent is a sparse active-account update.
type RateLimitsUpdatedEvent struct {
	EventBase
	AccountID  *string               `json:"account_id"`
	RateLimits RateLimitSnapshotWire `json:"rateLimits"`
}

// TurnStartedEvent marks the beginning of a turn under Codext's turn guard.
type TurnStartedEvent struct {
	EventBase
	TurnID   string  `json:"turn_id"`
	ThreadID *string `json:"thread_id"`
}

// TurnCompletedEvent marks the end of a turn and its result.
type TurnCompletedEvent struct {
	EventBase
	TurnID    string  `json:"turn_id"`
	ThreadID  *string `json:"thread_id"`
	Outcome   string  `json:"outcome"`
	ErrorCode *string `json:"error_code,omitempty"`
}

// SafeBoundaryReachedEvent is emitted only when Codext's authoritative
// running-turn count reaches zero for a prepared transition.
type SafeBoundaryReachedEvent struct {
	EventBase
	Reason string  `json:"reason"`
	TurnID *string `json:"turn_id"`
}

// AuthReloadStartedEvent marks the beginning of AuthManager reload.
type AuthReloadStartedEvent struct {
	EventBase
	TargetAccountID string `json:"target_account_id"`
}

// AuthReloadSucceededEvent reports the observed identity after reload.
type AuthReloadSucceededEvent struct {
	EventBase
	AccountID         *string `json:"account_id"`
	PreviousAccountID *string `json:"previous_account_id"`
	Changed           bool    `json:"changed"`
}

// AuthReloadFailedEvent reports a reload failure without exposing credentials.
type AuthReloadFailedEvent struct {
	EventBase
	ErrorCode    string `json:"error_code"`
	ErrorMessage string `json:"error_message"`
}

// IdentityChangedEvent confirms the identity observed after a transition.
type IdentityChangedEvent struct {
	EventBase
	PreviousAccountID *string `json:"previous_account_id"`
	AccountID         *string `json:"account_id"`
}

// RecoveryParkedEvent observes runtime-owned recovery parking.
type RecoveryParkedEvent struct {
	EventBase
	RecoveryID      string `json:"recovery_id"`
	Reason          string `json:"reason"`
	ThreadID        string `json:"thread_id,omitempty"`
	TurnID          string `json:"turn_id,omitempty"`
	SourceAccountID string `json:"source_account_id,omitempty"`
}

// RecoveryStartedEvent observes dispatch of a parked runtime recovery.
type RecoveryStartedEvent struct {
	EventBase
	RecoveryID string `json:"recovery_id"`
	ThreadID   string `json:"thread_id,omitempty"`
	TurnID     string `json:"turn_id,omitempty"`
}

// RecoveryCompletedEvent observes completion of a runtime-owned recovery.
type RecoveryCompletedEvent struct {
	EventBase
	RecoveryID string  `json:"recovery_id"`
	Outcome    string  `json:"outcome"`
	ErrorCode  *string `json:"error_code,omitempty"`
	ThreadID   string  `json:"thread_id,omitempty"`
	TurnID     string  `json:"turn_id,omitempty"`
}

// Validate checks controller-owned transition fields before sending them.
func (p AuthTransitionParams) Validate() error {
	if p.TransitionID == "" {
		return errors.New("transition_id is required")
	}
	if p.TargetAccountID == "" {
		return errors.New("target_account_id is required")
	}
	if p.ExpectedGeneration == 0 {
		return errors.New("expected_generation must be greater than zero")
	}
	return nil
}

// Validate checks a cancellation request before sending it.
func (p CancelAuthTransitionParams) Validate() error {
	if p.TransitionID == "" {
		return errors.New("transition_id is required")
	}
	if p.ExpectedGeneration == 0 {
		return errors.New("expected_generation must be greater than zero")
	}
	return nil
}

// Validate checks a native recovery release request before sending it.
func (p RecoveryReleaseParams) Validate() error {
	if p.RecoveryID == "" {
		return errors.New("recovery_id is required")
	}
	if p.TransitionID == "" {
		return errors.New("transition_id is required")
	}
	if p.ExpectedGeneration == 0 {
		return errors.New("expected_generation must be greater than zero")
	}
	return nil
}

// Validate checks a version advertisement before sending it.
func (p VersionNegotiationParams) Validate() error {
	if len(p.SupportedVersions) == 0 {
		return errors.New("supported_versions must not be empty")
	}
	seen := make(map[int]struct{}, len(p.SupportedVersions))
	for _, version := range p.SupportedVersions {
		if version < 1 {
			return fmt.Errorf("supported version %d is invalid", version)
		}
		if _, ok := seen[version]; ok {
			return fmt.Errorf("supported version %d is duplicated", version)
		}
		seen[version] = struct{}{}
	}
	return nil
}

// DecodeEvent decodes the flat event params object while preserving unknown
// event-specific fields for forward-compatible logging or future adapters.
func DecodeEvent(raw []byte) (Event, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return Event{}, fmt.Errorf("decode event fields: %w", err)
	}
	if fields == nil {
		return Event{}, errors.New("event params must be an object")
	}
	for _, key := range []string{"event_type", "occurred_at", "runtime_id", "auth_generation"} {
		if _, ok := fields[key]; !ok {
			return Event{}, fmt.Errorf("event %s is required", key)
		}
	}
	var base EventBase
	if err := json.Unmarshal(raw, &base); err != nil {
		return Event{}, fmt.Errorf("decode event base: %w", err)
	}
	if base.EventType == "" {
		return Event{}, errors.New("event_type is required")
	}
	if base.RuntimeID == "" {
		return Event{}, errors.New("runtime_id is required")
	}
	if requiresTransitionID(base.EventType) {
		if base.TransitionID == "" {
			return Event{}, fmt.Errorf("event %s requires transition_id", base.EventType)
		}
	}
	for _, key := range []string{"event_type", "occurred_at", "runtime_id", "auth_generation", "transition_id"} {
		delete(fields, key)
	}
	payload, err := json.Marshal(fields)
	if err != nil {
		return Event{}, fmt.Errorf("encode event payload: %w", err)
	}
	return Event{EventBase: base, Payload: payload}, nil
}

func requiresTransitionID(eventType EventType) bool {
	switch eventType {
	case EventSafeBoundaryReached,
		EventAuthReloadStarted,
		EventAuthReloadSucceeded,
		EventAuthReloadFailed,
		EventIdentityChanged:
		return true
	default:
		return false
	}
}

// DecodePayload converts a known event into its typed v1 payload. Unknown
// event types are returned as an error while DecodeEvent remains available for
// forward-compatible consumers.
func (e Event) DecodePayload() (any, error) {
	var target any
	switch e.EventType {
	case EventRuntimeReady:
		target = &RuntimeReadyEvent{}
	case EventRateLimitsSnapshot:
		target = &RateLimitsSnapshotEvent{}
	case EventRateLimitsUpdated:
		target = &RateLimitsUpdatedEvent{}
	case EventTurnStarted:
		target = &TurnStartedEvent{}
	case EventTurnCompleted:
		target = &TurnCompletedEvent{}
	case EventSafeBoundaryReached:
		target = &SafeBoundaryReachedEvent{}
	case EventAuthReloadStarted:
		target = &AuthReloadStartedEvent{}
	case EventAuthReloadSucceeded:
		target = &AuthReloadSucceededEvent{}
	case EventAuthReloadFailed:
		target = &AuthReloadFailedEvent{}
	case EventIdentityChanged:
		target = &IdentityChangedEvent{}
	case EventRecoveryParked:
		target = &RecoveryParkedEvent{}
	case EventRecoveryStarted:
		target = &RecoveryStartedEvent{}
	case EventRecoveryCompleted:
		target = &RecoveryCompletedEvent{}
	default:
		return nil, fmt.Errorf("unsupported event type %q", e.EventType)
	}
	if len(bytes.TrimSpace(e.Payload)) == 0 || bytes.Equal(bytes.TrimSpace(e.Payload), []byte("{}")) {
		return target, nil
	}
	if err := json.Unmarshal(e.Payload, target); err != nil {
		return nil, fmt.Errorf("decode %s payload: %w", e.EventType, err)
	}
	// Reapply common fields because DecodeEvent removes them before retaining
	// the event-specific payload.
	switch value := target.(type) {
	case *RuntimeReadyEvent:
		value.EventBase = e.EventBase
	case *RateLimitsSnapshotEvent:
		value.EventBase = e.EventBase
	case *RateLimitsUpdatedEvent:
		value.EventBase = e.EventBase
	case *TurnStartedEvent:
		value.EventBase = e.EventBase
	case *TurnCompletedEvent:
		value.EventBase = e.EventBase
	case *SafeBoundaryReachedEvent:
		value.EventBase = e.EventBase
	case *AuthReloadStartedEvent:
		value.EventBase = e.EventBase
	case *AuthReloadSucceededEvent:
		value.EventBase = e.EventBase
	case *AuthReloadFailedEvent:
		value.EventBase = e.EventBase
	case *IdentityChangedEvent:
		value.EventBase = e.EventBase
	case *RecoveryParkedEvent:
		value.EventBase = e.EventBase
	case *RecoveryStartedEvent:
		value.EventBase = e.EventBase
	case *RecoveryCompletedEvent:
		value.EventBase = e.EventBase
	}
	return target, nil
}
