// Package fakeruntime is a deterministic in-process implementation of the
// Marathon runtime boundary. It models only observable runtime behavior; it
// does not parse or retain credential bytes.
package fakeruntime

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"codexmarathon/controller/internal/runtime"
)

var (
	// ErrAckLost simulates a state-changing commit whose response was lost on
	// the transport. The identity and generation are still updated first.
	ErrAckLost = errors.New("fake runtime commit acknowledgement lost")
	// ErrRuntimeDisconnected simulates a runtime that cannot service commands.
	ErrRuntimeDisconnected = errors.New("fake runtime is disconnected")
)

// Runtime is safe for concurrent controller calls and test assertions.
type Runtime struct {
	mu sync.Mutex

	identity        runtime.Identity
	activeTurnCount int
	pending         *runtime.PendingTransition
	completed       map[string]runtime.TransitionResult
	recoveries      map[string]runtime.RecoveryState

	events chan runtime.Event
	closed bool

	disconnected   bool
	prepareErr     error
	commitErr      error
	reloadErr      error
	reloadFailures int
	dropNextAck    bool
	reloadCount    int

	prepareCalls         []runtime.AuthTransitionParams
	commitCalls          []runtime.AuthTransitionParams
	cancelCalls          []runtime.CancelAuthTransitionParams
	recoveryReleaseCalls []runtime.RecoveryReleaseParams
	dropNextRecoveryAck  bool
}

// New creates a fake runtime with a stable runtime ID and the supplied
// observed account/generation. A blank account means unauthenticated.
func New(accountID string, generation uint64) *Runtime {
	var account *string
	if accountID != "" {
		copy := accountID
		account = &copy
	}
	return &Runtime{
		identity:   runtime.Identity{RuntimeID: "fake-runtime", AccountID: account, AuthGeneration: generation},
		events:     make(chan runtime.Event, 64),
		completed:  make(map[string]runtime.TransitionResult),
		recoveries: make(map[string]runtime.RecoveryState),
	}
}

// NewFakeRuntime is a descriptive alias for New.
func NewFakeRuntime(accountID string, generation uint64) *Runtime { return New(accountID, generation) }

// SetRuntimeID changes the runtime identity used for subsequent responses.
// Tests should use this to model reconnect-to-a-different-process behavior.
func (f *Runtime) SetRuntimeID(runtimeID string) {
	if f == nil {
		return
	}
	f.mu.Lock()
	f.identity.RuntimeID = runtimeID
	f.mu.Unlock()
}

// RuntimeID returns the current fake process ID.
func (f *Runtime) RuntimeID() string {
	if f == nil {
		return ""
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.identity.RuntimeID
}

// Identity returns a defensive copy of the fake's identity.
func (f *Runtime) Identity() runtime.Identity {
	if f == nil {
		return runtime.Identity{}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return cloneIdentity(f.identity)
}

// SetIdentity overwrites the observed identity for a test fixture. It is
// intentionally explicit so a test can model a process restart or split-brain
// observation without touching credential material.
func (f *Runtime) SetIdentity(accountID string, generation uint64) {
	if f == nil {
		return
	}
	f.mu.Lock()
	if accountID == "" {
		f.identity.AccountID = nil
	} else {
		copy := accountID
		f.identity.AccountID = &copy
	}
	f.identity.AuthGeneration = generation
	f.mu.Unlock()
}

// SetAccountID changes only the observed account while preserving generation.
func (f *Runtime) SetAccountID(accountID string) {
	if f == nil {
		return
	}
	f.mu.Lock()
	if accountID == "" {
		f.identity.AccountID = nil
	} else {
		copy := accountID
		f.identity.AccountID = &copy
	}
	f.mu.Unlock()
}

// SetAuthGeneration changes only the observed generation.
func (f *Runtime) SetAuthGeneration(generation uint64) {
	if f == nil {
		return
	}
	f.mu.Lock()
	f.identity.AuthGeneration = generation
	f.mu.Unlock()
}

// ReloadCount reports how many times the fake actually adopted a new
// identity. Idempotent duplicate commit commands do not increase this count.
func (f *Runtime) ReloadCount() int {
	if f == nil {
		return 0
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reloadCount
}

// Events exposes runtime notifications. The channel is closed by Close.
func (f *Runtime) Events() <-chan runtime.Event {
	if f == nil {
		return nil
	}
	return f.events
}

// Close ends the notification stream. It is idempotent.
func (f *Runtime) Close() {
	if f == nil {
		return
	}
	f.mu.Lock()
	if !f.closed {
		f.closed = true
		close(f.events)
	}
	f.mu.Unlock()
}

// Disconnect toggles command failures without discarding runtime state.
func (f *Runtime) Disconnect() {
	if f == nil {
		return
	}
	f.mu.Lock()
	f.disconnected = true
	f.mu.Unlock()
}

// Reconnect restores command servicing while preserving identity and pending
// transition state.
func (f *Runtime) Reconnect() {
	if f == nil {
		return
	}
	f.mu.Lock()
	f.disconnected = false
	f.mu.Unlock()
}

// SetPrepareError injects an error returned before prepare mutates state.
func (f *Runtime) SetPrepareError(err error) {
	if f == nil {
		return
	}
	f.mu.Lock()
	f.prepareErr = err
	f.mu.Unlock()
}

// SetCommitError injects an error returned by commit before applying it.
func (f *Runtime) SetCommitError(err error) {
	if f == nil {
		return
	}
	f.mu.Lock()
	f.commitErr = err
	f.mu.Unlock()
}

// SetReloadFailure injects count reload failures. The error is one-shot per
// call, which lets reconciliation recover after a transient failure.
func (f *Runtime) SetReloadFailure(err error, count ...int) {
	if f == nil {
		return
	}
	f.mu.Lock()
	if err == nil {
		err = errors.New("fake auth reload failed")
	}
	f.reloadErr = err
	f.reloadFailures = 1
	if len(count) > 0 && count[0] > 0 {
		f.reloadFailures = count[0]
	}
	f.mu.Unlock()
}

// DropNextCommitAck applies one commit and then returns ErrAckLost. It is the
// canonical lost-ACK injection used by integration tests.
func (f *Runtime) DropNextCommitAck() {
	if f == nil {
		return
	}
	f.mu.Lock()
	f.dropNextAck = true
	f.mu.Unlock()
}

// SetDropCommitAck sets or clears one-shot lost-ACK injection.
func (f *Runtime) SetDropCommitAck(drop bool) {
	if f == nil {
		return
	}
	f.mu.Lock()
	f.dropNextAck = drop
	f.mu.Unlock()
}

// SetActiveTurnCount sets the runtime's authoritative running-turn count.
// Counts below zero are normalized to zero.
func (f *Runtime) SetActiveTurnCount(count int) {
	if f == nil {
		return
	}
	if count < 0 {
		count = 0
	}
	f.mu.Lock()
	f.activeTurnCount = count
	identity := cloneIdentity(f.identity)
	pending := clonePending(f.pending)
	f.mu.Unlock()
	if count == 0 && pending != nil {
		f.emit(runtime.Event{EventBase: runtime.EventBase{
			EventType:      runtime.EventSafeBoundaryReached,
			OccurredAt:     time.Now().Unix(),
			RuntimeID:      identity.RuntimeID,
			AuthGeneration: identity.AuthGeneration,
			TransitionID:   pending.TransitionID,
		}, Payload: mustJSON(runtime.SafeBoundaryReachedEvent{Reason: "running turn count reached zero"})})
	}
}

// ActiveTurnCount returns the current running-turn count.
func (f *Runtime) ActiveTurnCount() int {
	if f == nil {
		return 0
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.activeTurnCount
}

// BeginTurn increments the authoritative running-turn count and emits a
// correlated turn_started notification.
func (f *Runtime) BeginTurn(turnID string) {
	if f == nil {
		return
	}
	f.mu.Lock()
	f.activeTurnCount++
	identity := cloneIdentity(f.identity)
	f.mu.Unlock()
	f.emit(runtime.Event{EventBase: runtime.EventBase{
		EventType:      runtime.EventTurnStarted,
		OccurredAt:     time.Now().Unix(),
		RuntimeID:      identity.RuntimeID,
		AuthGeneration: identity.AuthGeneration,
	}, Payload: mustJSON(runtime.TurnStartedEvent{TurnID: turnID})})
}

// FinishTurn decrements the running-turn count and emits exactly one safe
// boundary event when a prepared transition reaches zero.
func (f *Runtime) FinishTurn(turnID string) {
	if f == nil {
		return
	}
	f.mu.Lock()
	if f.activeTurnCount > 0 {
		f.activeTurnCount--
	}
	identity := cloneIdentity(f.identity)
	pending := clonePending(f.pending)
	zero := f.activeTurnCount == 0
	f.mu.Unlock()
	f.emit(runtime.Event{EventBase: runtime.EventBase{
		EventType:      runtime.EventTurnCompleted,
		OccurredAt:     time.Now().Unix(),
		RuntimeID:      identity.RuntimeID,
		AuthGeneration: identity.AuthGeneration,
	}, Payload: mustJSON(runtime.TurnCompletedEvent{TurnID: turnID, Outcome: "completed"})})
	if zero && pending != nil {
		f.emit(runtime.Event{EventBase: runtime.EventBase{
			EventType:      runtime.EventSafeBoundaryReached,
			OccurredAt:     time.Now().Unix(),
			RuntimeID:      identity.RuntimeID,
			AuthGeneration: identity.AuthGeneration,
			TransitionID:   pending.TransitionID,
		}, Payload: mustJSON(runtime.SafeBoundaryReachedEvent{Reason: "running turn count reached zero"})})
	}
}

// ReachSafeBoundary emits the runtime-authoritative safe-boundary event for
// the current pending transition without fabricating a turn completion.
func (f *Runtime) ReachSafeBoundary() {
	if f == nil {
		return
	}
	f.mu.Lock()
	identity := cloneIdentity(f.identity)
	pending := clonePending(f.pending)
	f.activeTurnCount = 0
	f.mu.Unlock()
	if pending == nil {
		return
	}
	f.emit(runtime.Event{EventBase: runtime.EventBase{
		EventType:      runtime.EventSafeBoundaryReached,
		OccurredAt:     time.Now().Unix(),
		RuntimeID:      identity.RuntimeID,
		AuthGeneration: identity.AuthGeneration,
		TransitionID:   pending.TransitionID,
	}, Payload: mustJSON(runtime.SafeBoundaryReachedEvent{Reason: "explicit fake boundary"})})
}

// PendingTransition returns a defensive pending intent, if any.
func (f *Runtime) PendingTransition() (runtime.PendingTransition, bool) {
	if f == nil {
		return runtime.PendingTransition{}, false
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.pending == nil {
		return runtime.PendingTransition{}, false
	}
	return *f.pending, true
}

// PrepareCalls returns a copy of all prepare requests, useful for asserting
// exactly-once transition correlation.
func (f *Runtime) PrepareCalls() []runtime.AuthTransitionParams {
	if f == nil {
		return nil
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]runtime.AuthTransitionParams(nil), f.prepareCalls...)
}

// CommitCalls returns a copy of all commit requests.
func (f *Runtime) CommitCalls() []runtime.AuthTransitionParams {
	if f == nil {
		return nil
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]runtime.AuthTransitionParams(nil), f.commitCalls...)
}

// CancelCalls returns a copy of all cancellation requests.
func (f *Runtime) CancelCalls() []runtime.CancelAuthTransitionParams {
	if f == nil {
		return nil
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]runtime.CancelAuthTransitionParams(nil), f.cancelCalls...)
}

// SetRecovery installs a runtime-owned parked recovery for an integration
// fixture. The fixture stores only correlation metadata, matching the real
// adapter contract; prompt and credential bytes are never represented.
func (f *Runtime) SetRecovery(recoveryID, threadID, turnID, sourceAccountID string) {
	if f == nil || recoveryID == "" {
		return
	}
	f.mu.Lock()
	f.recoveries[recoveryID] = runtime.RecoveryState{
		RecoveryID: recoveryID, ThreadID: threadID, TurnID: turnID,
		SourceAccountID: sourceAccountID, Phase: runtime.RecoveryParkedPhase,
	}
	f.mu.Unlock()
}

// RecoveryReleaseCalls returns the controller-to-runtime release requests in
// order, allowing integration tests to assert exactly-once dispatch.
func (f *Runtime) RecoveryReleaseCalls() []runtime.RecoveryReleaseParams {
	if f == nil {
		return nil
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]runtime.RecoveryReleaseParams(nil), f.recoveryReleaseCalls...)
}

// DropNextRecoveryReleaseAck applies one release and drops its acknowledgement.
func (f *Runtime) DropNextRecoveryReleaseAck() {
	if f == nil {
		return
	}
	f.mu.Lock()
	f.dropNextRecoveryAck = true
	f.mu.Unlock()
}

// GetRuntimeState implements transitions.Runtime.
func (f *Runtime) GetRuntimeState(ctx context.Context) (runtime.RuntimeState, error) {
	if err := f.check(ctx); err != nil {
		return runtime.RuntimeState{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return runtime.RuntimeState{
		Identity:          cloneIdentity(f.identity),
		ActiveTurnCount:   f.activeTurnCount,
		PendingTransition: clonePending(f.pending),
		Recoveries:        cloneRecoveries(f.recoveries),
	}, nil
}

// ReleaseRecovery implements recovery.Runtime. It authorizes one already
// parked runtime recovery and never constructs or submits a prompt.
func (f *Runtime) ReleaseRecovery(ctx context.Context, params runtime.RecoveryReleaseParams) (runtime.RecoveryReleaseResult, error) {
	if err := f.check(ctx); err != nil {
		return runtime.RecoveryReleaseResult{}, err
	}
	if err := params.Validate(); err != nil {
		return runtime.RecoveryReleaseResult{}, err
	}
	f.mu.Lock()
	f.recoveryReleaseCalls = append(f.recoveryReleaseCalls, params)
	state, ok := f.recoveries[params.RecoveryID]
	if !ok {
		f.mu.Unlock()
		code, message := "recovery_not_found", "recovery not found"
		return runtime.RecoveryReleaseResult{RecoveryID: params.RecoveryID, ThreadID: params.ThreadID, Outcome: runtime.RecoveryReleaseRejected, ErrorCode: &code, ErrorMessage: &message}, nil
	}
	if params.ExpectedGeneration != f.identity.AuthGeneration {
		f.mu.Unlock()
		code, message := "stale_generation", "runtime identity generation does not match release"
		return runtime.RecoveryReleaseResult{RecoveryID: params.RecoveryID, ThreadID: state.ThreadID, Outcome: runtime.RecoveryReleaseRejected, ErrorCode: &code, ErrorMessage: &message}, nil
	}
	if state.ThreadID != "" && params.ThreadID != "" && state.ThreadID != params.ThreadID {
		f.mu.Unlock()
		code, message := "thread_id_mismatch", "recovery belongs to another thread"
		return runtime.RecoveryReleaseResult{RecoveryID: params.RecoveryID, ThreadID: state.ThreadID, Outcome: runtime.RecoveryReleaseRejected, ErrorCode: &code, ErrorMessage: &message}, nil
	}
	if state.Phase == runtime.RecoveryReleasedPhase || state.Phase == runtime.RecoveryStartedPhase || state.Phase == runtime.RecoveryCompletedPhase {
		f.mu.Unlock()
		return runtime.RecoveryReleaseResult{RecoveryID: params.RecoveryID, ThreadID: state.ThreadID, Outcome: runtime.RecoveryAlreadyReleased}, nil
	}
	state.Phase = runtime.RecoveryReleasedPhase
	state.TransitionID = params.TransitionID
	state.ExpectedGeneration = params.ExpectedGeneration
	f.recoveries[params.RecoveryID] = state
	dropAck := f.dropNextRecoveryAck
	f.dropNextRecoveryAck = false
	f.mu.Unlock()
	if dropAck {
		return runtime.RecoveryReleaseResult{}, ErrAckLost
	}
	return runtime.RecoveryReleaseResult{RecoveryID: params.RecoveryID, ThreadID: state.ThreadID, Outcome: runtime.RecoveryReleased}, nil
}

// PrepareAuthTransition records intent without changing identity.
func (f *Runtime) PrepareAuthTransition(ctx context.Context, params runtime.AuthTransitionParams) (runtime.TransitionResult, error) {
	if err := f.check(ctx); err != nil {
		return runtime.TransitionResult{}, err
	}
	if err := params.Validate(); err != nil {
		return runtime.TransitionResult{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.prepareCalls = append(f.prepareCalls, params)
	if f.prepareErr != nil {
		err := f.prepareErr
		f.prepareErr = nil
		return runtime.TransitionResult{}, err
	}
	if params.ExpectedGeneration != f.identity.AuthGeneration+1 {
		return f.rejectedLocked(params, "stale_generation", "expected generation is not newer than runtime"), nil
	}
	if f.pending != nil {
		if f.pending.TransitionID == params.TransitionID && f.pending.TargetAccountID == params.TargetAccountID && f.pending.ExpectedGeneration == params.ExpectedGeneration {
			return f.acceptedLocked(params), nil
		}
		return f.rejectedLocked(params, "transition_in_progress", "another transition is pending"), nil
	}
	pending := runtime.PendingTransition{TransitionID: params.TransitionID, TargetAccountID: params.TargetAccountID, ExpectedGeneration: params.ExpectedGeneration}
	f.pending = &pending
	return f.acceptedLocked(params), nil
}

// CommitAuthTransition applies the prepared identity at the fake safe
// boundary. Calls with the same transition ID after a successful apply are
// idempotent and return the original committed result.
func (f *Runtime) CommitAuthTransition(ctx context.Context, params runtime.AuthTransitionParams) (runtime.TransitionResult, error) {
	if err := f.check(ctx); err != nil {
		return runtime.TransitionResult{}, err
	}
	if err := params.Validate(); err != nil {
		return runtime.TransitionResult{}, err
	}
	f.mu.Lock()
	f.commitCalls = append(f.commitCalls, params)
	if result, ok := f.completed[params.TransitionID]; ok {
		f.mu.Unlock()
		return result, nil
	}
	if f.commitErr != nil {
		err := f.commitErr
		f.commitErr = nil
		f.mu.Unlock()
		return runtime.TransitionResult{}, err
	}
	if f.pending == nil || f.pending.TransitionID != params.TransitionID || f.pending.ExpectedGeneration != params.ExpectedGeneration || f.pending.TargetAccountID != params.TargetAccountID {
		result := f.rejectedLocked(params, "stale_transition", "pending transition does not match commit")
		f.mu.Unlock()
		return result, nil
	}
	if params.ExpectedGeneration != f.identity.AuthGeneration+1 {
		result := f.rejectedLocked(params, "stale_generation", "commit generation is stale")
		f.mu.Unlock()
		return result, nil
	}
	if f.activeTurnCount > 0 {
		result := f.rejectedLocked(params, "active_turn", "commit requested before safe boundary")
		f.mu.Unlock()
		return result, nil
	}
	if f.reloadFailures > 0 {
		f.reloadFailures--
		err := f.reloadErr
		if err == nil {
			err = errors.New("fake auth reload failed")
		}
		f.mu.Unlock()
		return runtime.TransitionResult{}, err
	}
	previous := cloneIdentity(f.identity)
	target := params.TargetAccountID
	f.identity.AccountID = &target
	f.identity.AuthGeneration = params.ExpectedGeneration
	f.reloadCount++
	f.pending = nil
	result := runtime.TransitionResult{
		TransitionID:       params.TransitionID,
		RuntimeID:          f.identity.RuntimeID,
		ExpectedGeneration: params.ExpectedGeneration,
		AuthGeneration:     f.identity.AuthGeneration,
		Outcome:            runtime.TransitionCommitted,
		AccountID:          cloneString(f.identity.AccountID),
	}
	f.completed[params.TransitionID] = result
	identity := cloneIdentity(f.identity)
	dropAck := f.dropNextAck
	f.dropNextAck = false
	f.mu.Unlock()

	f.emit(reloadStartedEvent(identity, params))
	f.emit(reloadSucceededEvent(identity, previous, params))
	f.emit(identityChangedEvent(identity, previous, params))
	if dropAck {
		return runtime.TransitionResult{}, ErrAckLost
	}
	return result, nil
}

// CancelAuthTransition removes a matching pending intent without touching
// identity or generation.
func (f *Runtime) CancelAuthTransition(ctx context.Context, params runtime.CancelAuthTransitionParams) (runtime.TransitionResult, error) {
	if err := f.check(ctx); err != nil {
		return runtime.TransitionResult{}, err
	}
	if err := params.Validate(); err != nil {
		return runtime.TransitionResult{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cancelCalls = append(f.cancelCalls, params)
	if f.pending == nil || f.pending.TransitionID != params.TransitionID || f.pending.ExpectedGeneration != params.ExpectedGeneration {
		return f.rejectedLocked(runtime.AuthTransitionParams{TransitionID: params.TransitionID, TargetAccountID: "unknown", ExpectedGeneration: params.ExpectedGeneration}, "stale_transition", "pending transition does not match cancellation"), nil
	}
	f.pending = nil
	return runtime.TransitionResult{
		TransitionID:       params.TransitionID,
		RuntimeID:          f.identity.RuntimeID,
		ExpectedGeneration: params.ExpectedGeneration,
		AuthGeneration:     f.identity.AuthGeneration,
		Outcome:            runtime.TransitionCommitted,
		AccountID:          cloneString(f.identity.AccountID),
	}, nil
}

// GetIdentity implements transitions.Runtime.
func (f *Runtime) GetIdentity(ctx context.Context) (runtime.Identity, error) {
	if err := f.check(ctx); err != nil {
		return runtime.Identity{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return cloneIdentity(f.identity), nil
}

// GetAuthGeneration implements transitions.Runtime.
func (f *Runtime) GetAuthGeneration(ctx context.Context) (runtime.AuthGenerationResult, error) {
	if err := f.check(ctx); err != nil {
		return runtime.AuthGenerationResult{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return runtime.AuthGenerationResult{AuthGeneration: f.identity.AuthGeneration}, nil
}

// Emit publishes an arbitrary event for tests that need to model stale or
// duplicated notifications.
func (f *Runtime) Emit(event runtime.Event) { f.emit(event) }

func (f *Runtime) emit(event runtime.Event) {
	if f == nil {
		return
	}
	f.mu.Lock()
	closed := f.closed
	channel := f.events
	f.mu.Unlock()
	if closed {
		return
	}
	select {
	case channel <- event:
	default:
		// Tests should never deadlock because a notification consumer is slow;
		// retain deterministic state and drop only excess diagnostics.
	}
}

func (f *Runtime) check(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	if f == nil {
		return errors.New("nil fake runtime")
	}
	f.mu.Lock()
	disconnected := f.disconnected
	f.mu.Unlock()
	if disconnected {
		return ErrRuntimeDisconnected
	}
	return nil
}

func (f *Runtime) acceptedLocked(params runtime.AuthTransitionParams) runtime.TransitionResult {
	return runtime.TransitionResult{
		TransitionID:       params.TransitionID,
		RuntimeID:          f.identity.RuntimeID,
		ExpectedGeneration: params.ExpectedGeneration,
		AuthGeneration:     f.identity.AuthGeneration,
		Outcome:            runtime.TransitionCommitted,
		AccountID:          cloneString(f.identity.AccountID),
	}
}

func (f *Runtime) rejectedLocked(params runtime.AuthTransitionParams, code, message string) runtime.TransitionResult {
	codeCopy, messageCopy := code, message
	return runtime.TransitionResult{
		TransitionID:       params.TransitionID,
		RuntimeID:          f.identity.RuntimeID,
		ExpectedGeneration: params.ExpectedGeneration,
		AuthGeneration:     f.identity.AuthGeneration,
		Outcome:            runtime.TransitionRejected,
		AccountID:          cloneString(f.identity.AccountID),
		ErrorCode:          &codeCopy,
		ErrorMessage:       &messageCopy,
	}
}

func cloneIdentity(identity runtime.Identity) runtime.Identity {
	identity.AccountID = cloneString(identity.AccountID)
	return identity
}

func cloneString(value *string) *string {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func clonePending(value *runtime.PendingTransition) *runtime.PendingTransition {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func cloneRecoveries(values map[string]runtime.RecoveryState) []runtime.RecoveryState {
	if len(values) == 0 {
		return nil
	}
	result := make([]runtime.RecoveryState, 0, len(values))
	for _, value := range values {
		result = append(result, value)
	}
	return result
}

func mustJSON(value any) []byte {
	payload, _ := json.Marshal(value)
	return payload
}

func reloadStartedEvent(identity runtime.Identity, params runtime.AuthTransitionParams) runtime.Event {
	return runtime.Event{EventBase: runtime.EventBase{EventType: runtime.EventAuthReloadStarted, OccurredAt: time.Now().Unix(), RuntimeID: identity.RuntimeID, AuthGeneration: identity.AuthGeneration, TransitionID: params.TransitionID}, Payload: mustJSON(runtime.AuthReloadStartedEvent{TargetAccountID: params.TargetAccountID})}
}

func reloadSucceededEvent(identity, previous runtime.Identity, params runtime.AuthTransitionParams) runtime.Event {
	return runtime.Event{EventBase: runtime.EventBase{EventType: runtime.EventAuthReloadSucceeded, OccurredAt: time.Now().Unix(), RuntimeID: identity.RuntimeID, AuthGeneration: identity.AuthGeneration, TransitionID: params.TransitionID}, Payload: mustJSON(runtime.AuthReloadSucceededEvent{AccountID: cloneString(identity.AccountID), PreviousAccountID: cloneString(previous.AccountID), Changed: true})}
}

func identityChangedEvent(identity, previous runtime.Identity, params runtime.AuthTransitionParams) runtime.Event {
	return runtime.Event{EventBase: runtime.EventBase{EventType: runtime.EventIdentityChanged, OccurredAt: time.Now().Unix(), RuntimeID: identity.RuntimeID, AuthGeneration: identity.AuthGeneration, TransitionID: params.TransitionID}, Payload: mustJSON(runtime.IdentityChangedEvent{AccountID: cloneString(identity.AccountID), PreviousAccountID: cloneString(previous.AccountID)})}
}
