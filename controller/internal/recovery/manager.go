// Package recovery coordinates Codext's runtime-owned UsageLimitExceeded
// continuation with an account transition.
//
// Codext creates and parks the synthetic recovery turn, including its prompt
// and conversation context.  This package never creates a prompt and never
// submits a continuation itself.  It persists only correlation metadata and
// releases one runtime recovery ID after the transition coordinator has
// verified the new identity and generation.
package recovery

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"codexmarathon/controller/internal/journal"
	"codexmarathon/controller/internal/runtime"
)

var (
	ErrNilManager           = errors.New("nil recovery manager")
	ErrRecoveryNotFound     = errors.New("recovery not found")
	ErrRuntimeUnavailable   = errors.New("recovery runtime is unavailable")
	ErrReleaseNotAuthorized = errors.New("recovery release is not authorized")
	ErrIdentityNotVerified  = errors.New("recovery identity is not verified")
	ErrRecoveryConflict     = errors.New("recovery metadata conflicts")
	ErrNoPendingRecovery    = errors.New("no pending recovery matches transition")
)

// Phase is the controller's durable view of one runtime-owned recovery.  A
// phase never contains prompt text or credential material.
type Phase string

const (
	PhaseParked           Phase = "parked"
	PhaseWaiting          Phase = "waiting_for_capacity"
	PhaseBound            Phase = "bound"
	PhaseReleaseRequested Phase = "release_requested"
	PhaseReleased         Phase = "released"
	PhaseStarted          Phase = "started"
	PhaseCompleted        Phase = "completed"
	PhaseFailed           Phase = "failed"
)

// State is the secret-free, restart-safe recovery record.  The runtime owns
// the synthetic user message; only IDs, account names, and generations are
// persisted here.
type State struct {
	RecoveryID         string    `json:"recovery_id"`
	RuntimeID          string    `json:"runtime_id,omitempty"`
	ThreadID           string    `json:"thread_id,omitempty"`
	TurnID             string    `json:"turn_id,omitempty"`
	SourceAccountID    string    `json:"source_account_id,omitempty"`
	TransitionID       string    `json:"transition_id,omitempty"`
	TargetAccountID    string    `json:"target_account_id,omitempty"`
	ExpectedGeneration uint64    `json:"expected_generation,omitempty"`
	Phase              Phase     `json:"phase"`
	Reason             string    `json:"reason,omitempty"`
	UpdatedAt          time.Time `json:"updated_at"`
}

// Runtime is the minimal runtime authority needed to release and reconcile a
// parked recovery.  runtime.Client and the integration fake implement it.
type Runtime interface {
	ReleaseRecovery(context.Context, runtime.RecoveryReleaseParams) (runtime.RecoveryReleaseResult, error)
	GetRuntimeState(context.Context) (runtime.RuntimeState, error)
}

// Config constructs a Manager. Journal may be nil for an in-memory test, but
// production wiring should always supply the controller's FileJournal.
type Config struct {
	Journal journal.Journal
	Runtime Runtime
	Now     func() time.Time
}

// TransitionCommit is the verified result supplied by the transition
// coordinator.  A caller must provide the committed target identity and
// generation; an uncertain or rejected transition cannot authorize release.
type TransitionCommit struct {
	TransitionID       string
	RuntimeID          string
	SourceAccountID    string
	TargetAccountID    string
	ExpectedGeneration uint64
	FinalGeneration    uint64
	Outcome            string
}

// Binding connects a parked recovery to one controller transition.  Binding
// is written before release authorization so a process crash can replay the
// relationship without inventing a second recovery prompt.
type Binding struct {
	RecoveryID         string
	RuntimeID          string
	ThreadID           string
	TurnID             string
	TransitionID       string
	TargetAccountID    string
	ExpectedGeneration uint64
}

// Manager owns recovery state and its append-only metadata journal.
type Manager struct {
	mu        sync.Mutex
	journal   journal.Journal
	runtime   Runtime
	now       func() time.Time
	states    map[string]State
	committed map[string]TransitionCommit
}

// New constructs and replays a recovery manager.  Replay is strict: a
// corrupt journal is returned to the caller instead of silently dropping a
// release intent and risking a duplicate continuation.
func New(config Config) (*Manager, error) {
	now := config.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	m := &Manager{
		journal:   config.Journal,
		runtime:   config.Runtime,
		now:       now,
		states:    make(map[string]State),
		committed: make(map[string]TransitionCommit),
	}
	if config.Journal == nil {
		return m, nil
	}
	events, err := config.Journal.ReadAll()
	if err != nil {
		return nil, err
	}
	for _, event := range events {
		if err := m.replayEvent(event); err != nil {
			return nil, err
		}
	}
	m.replayCommittedBindings()
	return m, nil
}

// NewManager is a descriptive constructor alias.
func NewManager(config Config) (*Manager, error) { return New(config) }

// SetRuntime installs a newly connected runtime. It does not release any
// recovery; callers must invoke Reconcile after verifying the runtime state.
func (m *Manager) SetRuntime(runtime Runtime) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.runtime = runtime
	m.mu.Unlock()
}

// Runtime returns the currently attached runtime authority.
func (m *Manager) Runtime() Runtime {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.runtime
}

// State returns an independent snapshot for one recovery ID.
func (m *Manager) State(recoveryID string) (State, bool) {
	if m == nil {
		return State{}, false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	state, ok := m.states[recoveryID]
	return state, ok
}

// States returns deterministic independent snapshots of all recoveries.
func (m *Manager) States() []State {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	result := make([]State, 0, len(m.states))
	for _, state := range m.states {
		result = append(result, state)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].RecoveryID < result[j].RecoveryID })
	return result
}

// HandleEvent consumes a runtime recovery notification. Duplicate or stale
// notifications are ignored. Runtime events are journalled before their
// state mutation so a successful return always means replay can reconstruct
// the same lifecycle.
func (m *Manager) HandleEvent(event runtime.Event) error {
	if m == nil {
		return ErrNilManager
	}
	if event.RuntimeID == "" {
		return errors.New("recovery event has no runtime_id")
	}
	decoded, err := event.DecodePayload()
	if err != nil {
		return err
	}
	switch event.EventType {
	case runtime.EventRecoveryParked:
		payload, ok := decoded.(*runtime.RecoveryParkedEvent)
		if !ok {
			return errors.New("recovery_parked payload type mismatch")
		}
		return m.observeParked(event, *payload)
	case runtime.EventRecoveryStarted:
		payload, ok := decoded.(*runtime.RecoveryStartedEvent)
		if !ok {
			return errors.New("recovery_started payload type mismatch")
		}
		return m.observeStarted(event, *payload)
	case runtime.EventRecoveryCompleted:
		payload, ok := decoded.(*runtime.RecoveryCompletedEvent)
		if !ok {
			return errors.New("recovery_completed payload type mismatch")
		}
		return m.observeCompleted(event, *payload)
	default:
		return nil
	}
}

func (m *Manager) observeParked(event runtime.Event, payload runtime.RecoveryParkedEvent) error {
	if strings.TrimSpace(payload.RecoveryID) == "" {
		return errors.New("recovery_parked has no recovery_id")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	existing, exists := m.states[payload.RecoveryID]
	if exists && phaseRank(existing.Phase) > phaseRank(PhaseParked) {
		return nil
	}
	state := existing
	if !exists {
		state = State{RecoveryID: payload.RecoveryID, Phase: PhaseParked}
	}
	state.RuntimeID = event.RuntimeID
	if payload.ThreadID != "" {
		state.ThreadID = payload.ThreadID
	}
	if payload.TurnID != "" {
		state.TurnID = payload.TurnID
	}
	if payload.SourceAccountID != "" {
		state.SourceAccountID = payload.SourceAccountID
	}
	state.Phase = PhaseParked
	// Do not persist arbitrary runtime reason text: Codext owns the prompt and
	// the controller journal is deliberately metadata-only.
	state.Reason = "runtime parked usage-limit recovery"
	state.UpdatedAt = m.nowUTC()
	if exists && stateEqual(existing, state) {
		return nil
	}
	if err := m.appendLocked(journal.Event{
		Type:           journal.RecoveryParked,
		RecoveryID:     state.RecoveryID,
		RuntimeID:      state.RuntimeID,
		ThreadID:       state.ThreadID,
		TurnID:         state.TurnID,
		AccountID:      state.SourceAccountID,
		AuthGeneration: event.AuthGeneration,
		Reason:         state.Reason,
	}); err != nil {
		return err
	}
	m.states[state.RecoveryID] = state
	return nil
}

func (m *Manager) observeStarted(event runtime.Event, payload runtime.RecoveryStartedEvent) error {
	if strings.TrimSpace(payload.RecoveryID) == "" {
		return errors.New("recovery_started has no recovery_id")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	existing, exists := m.states[payload.RecoveryID]
	if !exists {
		existing = State{RecoveryID: payload.RecoveryID}
	}
	if existing.RuntimeID != "" && existing.RuntimeID != event.RuntimeID {
		// A runtime restart can change its process ID. The recovery ID remains
		// the durable authority, so adopt the new runtime ID for reconciliation.
		existing.RuntimeID = event.RuntimeID
	} else if existing.RuntimeID == "" {
		existing.RuntimeID = event.RuntimeID
	}
	if payload.ThreadID != "" {
		if existing.ThreadID != "" && existing.ThreadID != payload.ThreadID {
			return fmt.Errorf("%w: thread_id changed for %s", ErrRecoveryConflict, payload.RecoveryID)
		}
		existing.ThreadID = payload.ThreadID
	}
	if payload.TurnID != "" {
		existing.TurnID = payload.TurnID
	}
	if phaseRank(existing.Phase) > phaseRank(PhaseStarted) {
		return nil
	}
	old := existing
	existing.Phase = PhaseStarted
	existing.UpdatedAt = m.nowUTC()
	if stateEqual(old, existing) {
		return nil
	}
	if err := m.appendLocked(journal.Event{
		Type:            journal.RecoveryStarted,
		RecoveryID:      existing.RecoveryID,
		RuntimeID:       existing.RuntimeID,
		ThreadID:        existing.ThreadID,
		TurnID:          existing.TurnID,
		TransitionID:    existing.TransitionID,
		TargetAccountID: existing.TargetAccountID,
		AuthGeneration:  existing.ExpectedGeneration,
	}); err != nil {
		return err
	}
	m.states[existing.RecoveryID] = existing
	return nil
}

func (m *Manager) observeCompleted(event runtime.Event, payload runtime.RecoveryCompletedEvent) error {
	if strings.TrimSpace(payload.RecoveryID) == "" {
		return errors.New("recovery_completed has no recovery_id")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	existing, exists := m.states[payload.RecoveryID]
	if !exists {
		existing = State{RecoveryID: payload.RecoveryID}
	}
	if existing.RuntimeID == "" {
		existing.RuntimeID = event.RuntimeID
	}
	if payload.ThreadID != "" {
		existing.ThreadID = payload.ThreadID
	}
	if payload.TurnID != "" {
		existing.TurnID = payload.TurnID
	}
	targetPhase := PhaseCompleted
	if strings.EqualFold(payload.Outcome, "failed") || strings.EqualFold(payload.Outcome, "skipped") {
		targetPhase = PhaseFailed
	}
	if phaseRank(existing.Phase) > phaseRank(targetPhase) {
		return nil
	}
	old := existing
	existing.Phase = targetPhase
	if targetPhase == PhaseFailed {
		existing.Reason = "runtime recovery completed without a continuation"
	} else {
		existing.Reason = "runtime recovery completed"
	}
	existing.UpdatedAt = m.nowUTC()
	if stateEqual(old, existing) {
		return nil
	}
	if err := m.appendLocked(journal.Event{
		Type:            journal.RecoveryCompleted,
		RecoveryID:      existing.RecoveryID,
		RuntimeID:       existing.RuntimeID,
		ThreadID:        existing.ThreadID,
		TurnID:          existing.TurnID,
		TransitionID:    existing.TransitionID,
		TargetAccountID: existing.TargetAccountID,
		AuthGeneration:  existing.ExpectedGeneration,
		Outcome:         strings.ToLower(payload.Outcome),
		Reason:          existing.Reason,
	}); err != nil {
		return err
	}
	m.states[existing.RecoveryID] = existing
	return nil
}

// MarkWaiting records that the recovery remains parked while every account
// is exhausted or until the earliest reset. It never invokes the runtime.
func (m *Manager) MarkWaiting(recoveryID, reason string) error {
	if m == nil {
		return ErrNilManager
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	state, ok := m.states[recoveryID]
	if !ok {
		return fmt.Errorf("%w: %s", ErrRecoveryNotFound, recoveryID)
	}
	if isTerminal(state.Phase) {
		return nil
	}
	old := state
	state.Phase = PhaseWaiting
	state.Reason = safeWaitingReason(reason)
	state.UpdatedAt = m.nowUTC()
	if stateEqual(old, state) {
		return nil
	}
	if err := m.appendLocked(journal.Event{
		Type:            journal.RecoveryWaiting,
		RecoveryID:      state.RecoveryID,
		RuntimeID:       state.RuntimeID,
		ThreadID:        state.ThreadID,
		TurnID:          state.TurnID,
		TransitionID:    state.TransitionID,
		TargetAccountID: state.TargetAccountID,
		AuthGeneration:  state.ExpectedGeneration,
		Reason:          state.Reason,
	}); err != nil {
		return err
	}
	m.states[recoveryID] = state
	return nil
}

// Bind associates a parked recovery with a specific transition. It is
// idempotent for identical metadata and rejects cross-thread/rebind attempts.
func (m *Manager) Bind(binding Binding) error {
	if m == nil {
		return ErrNilManager
	}
	if strings.TrimSpace(binding.RecoveryID) == "" || strings.TrimSpace(binding.TransitionID) == "" || strings.TrimSpace(binding.TargetAccountID) == "" {
		return errors.New("recovery binding requires recovery_id, transition_id, and target_account_id")
	}
	if binding.ExpectedGeneration == 0 {
		return errors.New("recovery binding requires expected_generation")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	state, ok := m.states[binding.RecoveryID]
	if !ok {
		return fmt.Errorf("%w: %s", ErrRecoveryNotFound, binding.RecoveryID)
	}
	if isTerminal(state.Phase) {
		return nil
	}
	if state.TransitionID != "" {
		if state.TransitionID == binding.TransitionID && state.TargetAccountID == binding.TargetAccountID && state.ExpectedGeneration == binding.ExpectedGeneration {
			return nil
		}
		return fmt.Errorf("%w: %s already belongs to transition %s", ErrRecoveryConflict, binding.RecoveryID, state.TransitionID)
	}
	if state.ThreadID != "" && binding.ThreadID != "" && state.ThreadID != binding.ThreadID {
		return fmt.Errorf("%w: recovery thread differs from binding", ErrRecoveryConflict)
	}
	state.RuntimeID = firstNonEmpty(binding.RuntimeID, state.RuntimeID)
	state.ThreadID = firstNonEmpty(state.ThreadID, binding.ThreadID)
	state.TurnID = firstNonEmpty(state.TurnID, binding.TurnID)
	state.TransitionID = binding.TransitionID
	state.TargetAccountID = binding.TargetAccountID
	state.ExpectedGeneration = binding.ExpectedGeneration
	state.Phase = PhaseBound
	state.Reason = "recovery bound to verified transition"
	state.UpdatedAt = m.nowUTC()
	if err := m.appendLocked(journal.Event{
		Type:               journal.RecoveryBound,
		RecoveryID:         state.RecoveryID,
		RuntimeID:          state.RuntimeID,
		ThreadID:           state.ThreadID,
		TurnID:             state.TurnID,
		TransitionID:       state.TransitionID,
		TargetAccountID:    state.TargetAccountID,
		AuthGeneration:     state.ExpectedGeneration,
		ExpectedGeneration: state.ExpectedGeneration,
		Reason:             state.Reason,
	}); err != nil {
		return err
	}
	m.states[state.RecoveryID] = state
	return nil
}

// PendingForTarget returns one deterministic parked recovery for a target.
// Callers should bind it before requesting a transition when they want the
// crash window between transition commit and binding to be zero. A false
// result is normal for ordinary transitions with no parked recovery.
func (m *Manager) PendingForTarget(targetAccountID, runtimeID, threadID string) (State, bool) {
	if m == nil {
		return State{}, false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	var chosen State
	found := false
	for _, state := range m.states {
		if isTerminal(state.Phase) || state.TargetAccountID != "" {
			continue
		}
		if runtimeID != "" && state.RuntimeID != "" && state.RuntimeID != runtimeID {
			continue
		}
		if threadID != "" && state.ThreadID != "" && state.ThreadID != threadID {
			continue
		}
		// An unbound recovery has no target yet; the target is supplied when
		// the verified transition commits. Keep the parameter for callers that
		// already know the intended target and reject only an empty request.
		if targetAccountID == "" {
			continue
		}
		if !found || state.RecoveryID < chosen.RecoveryID {
			chosen, found = state, true
		}
	}
	return chosen, found
}

// OnTransitionCommitted is the only release authorization path. It refuses
// uncertain/rejected results, persists the release intent before IPC, and
// treats an already-released acknowledgement as success. Repeating it with
// the same transition is safe and never creates a second prompt.
func (m *Manager) OnTransitionCommitted(ctx context.Context, commit TransitionCommit) error {
	if m == nil {
		return ErrNilManager
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if !strings.EqualFold(commit.Outcome, "committed") {
		return fmt.Errorf("%w: transition outcome is %q", ErrReleaseNotAuthorized, commit.Outcome)
	}
	if commit.TransitionID == "" || commit.TargetAccountID == "" || commit.ExpectedGeneration == 0 {
		return fmt.Errorf("%w: incomplete committed transition", ErrReleaseNotAuthorized)
	}
	if commit.FinalGeneration != 0 && commit.FinalGeneration != commit.ExpectedGeneration {
		return fmt.Errorf("%w: final generation %d does not match expected %d", ErrIdentityNotVerified, commit.FinalGeneration, commit.ExpectedGeneration)
	}
	// Snapshot matching records under lock. A normal transition with no parked
	// recovery is a successful no-op for this manager. When the caller could
	// not bind before requesting the transition, bind one unbound candidate
	// only when the runtime/source correlation is unambiguous.
	m.mu.Lock()
	ids := make([]string, 0)
	for id, state := range m.states {
		if state.TransitionID == commit.TransitionID {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		candidates := make([]State, 0)
		for _, state := range m.states {
			if isTerminal(state.Phase) || state.TransitionID != "" || state.TargetAccountID != "" {
				continue
			}
			if commit.RuntimeID != "" && state.RuntimeID != "" && state.RuntimeID != commit.RuntimeID {
				continue
			}
			if commit.SourceAccountID != "" && state.SourceAccountID != "" && state.SourceAccountID != commit.SourceAccountID {
				continue
			}
			candidates = append(candidates, state)
		}
		if len(candidates) == 1 {
			candidate := candidates[0]
			// Keep the lock while applying the binding so a concurrent commit
			// cannot select the same parked recovery.
			candidate.RuntimeID = firstNonEmpty(commit.RuntimeID, candidate.RuntimeID)
			candidate.TransitionID = commit.TransitionID
			candidate.TargetAccountID = commit.TargetAccountID
			candidate.ExpectedGeneration = commit.ExpectedGeneration
			candidate.Phase = PhaseBound
			candidate.Reason = "recovery bound to verified transition"
			candidate.UpdatedAt = m.nowUTC()
			if err := m.appendLocked(journal.Event{
				Type:               journal.RecoveryBound,
				RecoveryID:         candidate.RecoveryID,
				RuntimeID:          candidate.RuntimeID,
				ThreadID:           candidate.ThreadID,
				TurnID:             candidate.TurnID,
				TransitionID:       candidate.TransitionID,
				TargetAccountID:    candidate.TargetAccountID,
				AuthGeneration:     candidate.ExpectedGeneration,
				ExpectedGeneration: candidate.ExpectedGeneration,
				Reason:             candidate.Reason,
			}); err != nil {
				m.mu.Unlock()
				return err
			}
			m.states[candidate.RecoveryID] = candidate
			ids = append(ids, candidate.RecoveryID)
		}
	}
	sort.Strings(ids)
	if len(ids) == 0 {
		m.mu.Unlock()
		return nil
	}
	rt := m.runtime
	m.mu.Unlock()
	if rt == nil {
		// Persist the intent even when the runtime is temporarily detached.
		// This is the crash boundary between a verified commit and the next
		// IPC attempt; Reconcile will retry it after reconnecting.
		for _, id := range ids {
			if _, err := m.requestRelease(id, commit); err != nil {
				return err
			}
		}
		return ErrRuntimeUnavailable
	}
	for _, id := range ids {
		if err := m.releaseOne(ctx, id, commit, rt); err != nil {
			return err
		}
	}
	return nil
}

func (m *Manager) releaseOne(ctx context.Context, recoveryID string, commit TransitionCommit, rt Runtime) error {
	state, err := m.requestRelease(recoveryID, commit)
	if err != nil {
		if errors.Is(err, errRecoveryTerminal) {
			return nil
		}
		return err
	}

	result, err := rt.ReleaseRecovery(ctx, runtime.RecoveryReleaseParams{
		RecoveryID:         state.RecoveryID,
		ThreadID:           state.ThreadID,
		TransitionID:       state.TransitionID,
		ExpectedGeneration: state.ExpectedGeneration,
	})
	if err != nil {
		return m.markUncertain(recoveryID, fmt.Errorf("release recovery: %w", err))
	}
	if result.RecoveryID != "" && result.RecoveryID != recoveryID {
		return m.markUncertain(recoveryID, fmt.Errorf("release response recovery_id %q does not match %q", result.RecoveryID, recoveryID))
	}
	switch result.Outcome {
	case runtime.RecoveryReleased, runtime.RecoveryAlreadyReleased:
		return m.markReleased(recoveryID, result.Outcome)
	case runtime.RecoveryReleaseUncertain:
		return m.markUncertain(recoveryID, errors.New("runtime returned uncertain recovery release"))
	case runtime.RecoveryReleaseRejected:
		return m.markUncertain(recoveryID, errors.New("runtime rejected recovery release"))
	default:
		return m.markUncertain(recoveryID, fmt.Errorf("runtime returned unknown recovery release outcome %q", result.Outcome))
	}
}

var errRecoveryTerminal = errors.New("recovery already terminal")

// requestRelease durably records the authorization intent once. It is kept
// separate from the IPC call so a detached runtime or a controller crash
// cannot lose the fact that identity verification already happened.
func (m *Manager) requestRelease(recoveryID string, commit TransitionCommit) (State, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	state, ok := m.states[recoveryID]
	if !ok {
		return State{}, fmt.Errorf("%w: %s", ErrRecoveryNotFound, recoveryID)
	}
	if state.TransitionID != commit.TransitionID || state.TargetAccountID != commit.TargetAccountID || state.ExpectedGeneration != commit.ExpectedGeneration {
		return State{}, fmt.Errorf("%w: committed transition metadata differs for %s", ErrIdentityNotVerified, recoveryID)
	}
	if isTerminal(state.Phase) {
		return State{}, errRecoveryTerminal
	}
	if state.Phase == PhaseReleaseRequested {
		return state, nil
	}
	state.Phase = PhaseReleaseRequested
	state.Reason = "verified identity transition; recovery release requested"
	state.UpdatedAt = m.nowUTC()
	if err := m.appendLocked(journal.Event{
		Type:               journal.RecoveryReleaseRequested,
		RecoveryID:         state.RecoveryID,
		RuntimeID:          state.RuntimeID,
		ThreadID:           state.ThreadID,
		TurnID:             state.TurnID,
		TransitionID:       state.TransitionID,
		TargetAccountID:    state.TargetAccountID,
		AuthGeneration:     state.ExpectedGeneration,
		ExpectedGeneration: state.ExpectedGeneration,
		Reason:             state.Reason,
	}); err != nil {
		return State{}, err
	}
	m.states[recoveryID] = state
	return state, nil
}

func (m *Manager) markReleased(recoveryID string, outcome runtime.RecoveryReleaseOutcome) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	state, ok := m.states[recoveryID]
	if !ok {
		return fmt.Errorf("%w: %s", ErrRecoveryNotFound, recoveryID)
	}
	if state.Phase == PhaseReleased || state.Phase == PhaseStarted || state.Phase == PhaseCompleted || state.Phase == PhaseFailed {
		return nil
	}
	state.Phase = PhaseReleased
	state.Reason = "runtime acknowledged recovery release"
	state.UpdatedAt = m.nowUTC()
	if err := m.appendLocked(journal.Event{
		Type:               journal.RecoveryReleased,
		RecoveryID:         state.RecoveryID,
		RuntimeID:          state.RuntimeID,
		ThreadID:           state.ThreadID,
		TurnID:             state.TurnID,
		TransitionID:       state.TransitionID,
		TargetAccountID:    state.TargetAccountID,
		AuthGeneration:     state.ExpectedGeneration,
		ExpectedGeneration: state.ExpectedGeneration,
		Outcome:            string(outcome),
		Reason:             state.Reason,
	}); err != nil {
		return err
	}
	m.states[recoveryID] = state
	return nil
}

func (m *Manager) markUncertain(recoveryID string, reason error) error {
	if reason == nil {
		reason = errors.New("recovery release acknowledgement is uncertain")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	state, ok := m.states[recoveryID]
	if !ok {
		return fmt.Errorf("%w: %s", ErrRecoveryNotFound, recoveryID)
	}
	state.Phase = PhaseReleaseRequested
	state.Reason = safeFailureReason(reason.Error())
	state.UpdatedAt = m.nowUTC()
	if err := m.appendLocked(journal.Event{
		Type:               journal.RecoveryUncertain,
		RecoveryID:         state.RecoveryID,
		RuntimeID:          state.RuntimeID,
		ThreadID:           state.ThreadID,
		TurnID:             state.TurnID,
		TransitionID:       state.TransitionID,
		TargetAccountID:    state.TargetAccountID,
		AuthGeneration:     state.ExpectedGeneration,
		ExpectedGeneration: state.ExpectedGeneration,
		Outcome:            "uncertain",
		Reason:             state.Reason,
	}); err != nil {
		return err
	}
	m.states[recoveryID] = state
	return reason
}

// Reconcile reads runtime-owned recovery state after a controller or runtime
// restart. It imports a parked recovery not seen before, adopts observed
// started/completed phases, and retries only release intents whose verified
// transition metadata still matches the current runtime identity.
func (m *Manager) Reconcile(ctx context.Context) error {
	if m == nil {
		return ErrNilManager
	}
	if ctx == nil {
		ctx = context.Background()
	}
	m.mu.Lock()
	rt := m.runtime
	m.mu.Unlock()
	if rt == nil {
		return ErrRuntimeUnavailable
	}
	remote, err := rt.GetRuntimeState(ctx)
	if err != nil {
		return err
	}
	remoteByID := make(map[string]runtime.RecoveryState, len(remote.Recoveries))
	for _, item := range remote.Recoveries {
		if item.RecoveryID != "" {
			remoteByID[item.RecoveryID] = item
		}
	}
	// Import/reconcile the runtime view first. This also covers a controller
	// crash before it consumed the initial recovery_parked notification.
	for _, item := range remote.Recoveries {
		if err := m.mergeRemote(remote.Identity.RuntimeID, item); err != nil {
			return err
		}
	}
	m.mu.Lock()
	m.replayCommittedBindingsLocked()
	m.mu.Unlock()
	for _, state := range m.States() {
		observed, exists := remoteByID[state.RecoveryID]
		if exists {
			switch observed.Phase {
			case runtime.RecoveryStartedPhase:
				if err := m.observeRemoteStarted(remote.Identity.RuntimeID, observed); err != nil {
					return err
				}
			case runtime.RecoveryCompletedPhase:
				if err := m.observeRemoteCompleted(remote.Identity.RuntimeID, observed); err != nil {
					return err
				}
			case runtime.RecoveryReleasedPhase:
				if err := m.markReleased(state.RecoveryID, runtime.RecoveryAlreadyReleased); err != nil && !errors.Is(err, ErrRecoveryNotFound) {
					return err
				}
			}
		}
		current, ok := m.State(state.RecoveryID)
		if !ok || current.Phase != PhaseReleaseRequested {
			continue
		}
		// ReleaseRequested is durable evidence that the transition was already
		// verified before the prior IPC call. Recheck the live identity before
		// retrying so a stale controller cannot release under Account A.
		if current.TargetAccountID == "" || current.ExpectedGeneration == 0 || accountID(remote.Identity.AccountID) != current.TargetAccountID || remote.Identity.AuthGeneration != current.ExpectedGeneration {
			continue
		}
		if err := m.releaseOne(ctx, current.RecoveryID, TransitionCommit{
			TransitionID:       current.TransitionID,
			RuntimeID:          remote.Identity.RuntimeID,
			TargetAccountID:    current.TargetAccountID,
			ExpectedGeneration: current.ExpectedGeneration,
			FinalGeneration:    current.ExpectedGeneration,
			Outcome:            "committed",
		}, rt); err != nil {
			return err
		}
	}
	return nil
}

func (m *Manager) mergeRemote(runtimeID string, item runtime.RecoveryState) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	existing, exists := m.states[item.RecoveryID]
	if !exists {
		existing = State{RecoveryID: item.RecoveryID, RuntimeID: runtimeID, Phase: PhaseParked}
	}
	if runtimeID != "" {
		existing.RuntimeID = runtimeID
	}
	if item.ThreadID != "" {
		existing.ThreadID = item.ThreadID
	}
	if item.TurnID != "" {
		existing.TurnID = item.TurnID
	}
	if item.SourceAccountID != "" {
		existing.SourceAccountID = item.SourceAccountID
	}
	if item.TransitionID != "" {
		existing.TransitionID = item.TransitionID
	}
	if item.TargetAccountID != "" {
		existing.TargetAccountID = item.TargetAccountID
	}
	if item.ExpectedGeneration != 0 {
		existing.ExpectedGeneration = item.ExpectedGeneration
	}
	remotePhase := phaseFromRuntime(item.Phase)
	if !exists || phaseRank(remotePhase) >= phaseRank(existing.Phase) {
		existing.Phase = remotePhase
	}
	existing.UpdatedAt = m.nowUTC()
	if exists && stateEqual(existing, m.states[item.RecoveryID]) {
		return nil
	}
	eventType := journal.RecoveryParked
	if existing.Phase == PhaseBound {
		eventType = journal.RecoveryBound
	} else if existing.Phase == PhaseReleaseRequested {
		eventType = journal.RecoveryReleaseRequested
	} else if existing.Phase == PhaseReleased {
		eventType = journal.RecoveryReleased
	} else if existing.Phase == PhaseStarted {
		eventType = journal.RecoveryStarted
	} else if existing.Phase == PhaseCompleted {
		eventType = journal.RecoveryCompleted
	}
	if err := m.appendLocked(journal.Event{
		Type:               eventType,
		RecoveryID:         existing.RecoveryID,
		RuntimeID:          existing.RuntimeID,
		ThreadID:           existing.ThreadID,
		TurnID:             existing.TurnID,
		TransitionID:       existing.TransitionID,
		TargetAccountID:    existing.TargetAccountID,
		AuthGeneration:     existing.ExpectedGeneration,
		ExpectedGeneration: existing.ExpectedGeneration,
		Outcome:            string(existing.Phase),
		Reason:             "runtime recovery state reconciled",
	}); err != nil {
		return err
	}
	m.states[item.RecoveryID] = existing
	return nil
}

func (m *Manager) observeRemoteStarted(runtimeID string, item runtime.RecoveryState) error {
	return m.observeRemotePhase(runtimeID, item, PhaseStarted, journal.RecoveryStarted)
}

func (m *Manager) observeRemoteCompleted(runtimeID string, item runtime.RecoveryState) error {
	return m.observeRemotePhase(runtimeID, item, PhaseCompleted, journal.RecoveryCompleted)
}

func (m *Manager) observeRemotePhase(runtimeID string, item runtime.RecoveryState, phase Phase, eventType journal.EventType) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	state, ok := m.states[item.RecoveryID]
	if !ok {
		return nil
	}
	if phaseRank(state.Phase) > phaseRank(phase) {
		return nil
	}
	old := state
	state.RuntimeID = firstNonEmpty(runtimeID, state.RuntimeID)
	state.ThreadID = firstNonEmpty(item.ThreadID, state.ThreadID)
	state.TurnID = firstNonEmpty(item.TurnID, state.TurnID)
	state.Phase = phase
	state.Reason = "runtime recovery state observed after restart"
	state.UpdatedAt = m.nowUTC()
	if stateEqual(old, state) {
		return nil
	}
	if err := m.appendLocked(journal.Event{
		Type:               eventType,
		RecoveryID:         state.RecoveryID,
		RuntimeID:          state.RuntimeID,
		ThreadID:           state.ThreadID,
		TurnID:             state.TurnID,
		TransitionID:       state.TransitionID,
		TargetAccountID:    state.TargetAccountID,
		AuthGeneration:     state.ExpectedGeneration,
		ExpectedGeneration: state.ExpectedGeneration,
		Outcome:            string(phase),
		Reason:             state.Reason,
	}); err != nil {
		return err
	}
	m.states[state.RecoveryID] = state
	return nil
}

func (m *Manager) replayEvent(event journal.Event) error {
	if event.Type == journal.TransitionCommitted || event.Type == journal.TransitionReconciled {
		if strings.EqualFold(event.Outcome, "committed") {
			m.committed[event.TransitionID] = TransitionCommit{
				TransitionID:       event.TransitionID,
				RuntimeID:          event.RuntimeID,
				SourceAccountID:    event.FromAccountID,
				TargetAccountID:    event.TargetAccountID,
				ExpectedGeneration: firstNonZero(event.ExpectedGeneration, event.AuthGeneration),
				FinalGeneration:    event.AuthGeneration,
				Outcome:            "committed",
			}
		}
		return nil
	}
	if !isRecoveryEvent(event.Type) {
		return nil
	}
	if event.RecoveryID == "" {
		return fmt.Errorf("%w: recovery event %s has no recovery_id", journal.ErrCorruptJournal, event.Type)
	}
	state := m.states[event.RecoveryID]
	if state.RecoveryID == "" {
		state.RecoveryID = event.RecoveryID
	}
	state.RuntimeID = firstNonEmpty(event.RuntimeID, state.RuntimeID)
	state.ThreadID = firstNonEmpty(event.ThreadID, state.ThreadID)
	state.TurnID = firstNonEmpty(event.TurnID, state.TurnID)
	state.SourceAccountID = firstNonEmpty(event.AccountID, state.SourceAccountID)
	state.TransitionID = firstNonEmpty(event.TransitionID, state.TransitionID)
	state.TargetAccountID = firstNonEmpty(event.TargetAccountID, state.TargetAccountID)
	state.ExpectedGeneration = firstNonZero(event.ExpectedGeneration, event.AuthGeneration, state.ExpectedGeneration)
	switch event.Type {
	case journal.RecoveryParked:
		if phaseRank(state.Phase) <= phaseRank(PhaseParked) {
			state.Phase = PhaseParked
		}
		state.Reason = "runtime parked usage-limit recovery"
	case journal.RecoveryWaiting:
		if phaseRank(state.Phase) <= phaseRank(PhaseWaiting) {
			state.Phase = PhaseWaiting
		}
		state.Reason = safeWaitingReason(event.Reason)
	case journal.RecoveryBound:
		if phaseRank(state.Phase) <= phaseRank(PhaseBound) {
			state.Phase = PhaseBound
		}
		state.Reason = "recovery bound to verified transition"
	case journal.RecoveryReleaseRequested, journal.RecoveryUncertain:
		if phaseRank(state.Phase) <= phaseRank(PhaseReleaseRequested) || state.Phase == PhaseReleaseRequested {
			state.Phase = PhaseReleaseRequested
		}
		state.Reason = safeFailureReason(event.Reason)
	case journal.RecoveryReleased:
		if phaseRank(state.Phase) <= phaseRank(PhaseReleased) {
			state.Phase = PhaseReleased
		}
		state.Reason = "runtime acknowledged recovery release"
	case journal.RecoveryStarted:
		if phaseRank(state.Phase) <= phaseRank(PhaseStarted) {
			state.Phase = PhaseStarted
		}
	case journal.RecoveryCompleted:
		if strings.EqualFold(event.Outcome, "failed") || strings.EqualFold(event.Outcome, "skipped") {
			state.Phase = PhaseFailed
		} else {
			state.Phase = PhaseCompleted
		}
	}
	state.UpdatedAt = event.At
	m.states[event.RecoveryID] = state
	return nil
}

// replayCommittedBindings closes the crash window in which the transition
// coordinator durably committed an identity change but the controller had not
// yet written RecoveryBound. It only binds one unambiguous parked recovery;
// ambiguous records remain parked for an operator-visible reconciliation.
func (m *Manager) replayCommittedBindings() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.replayCommittedBindingsLocked()
}

func (m *Manager) replayCommittedBindingsLocked() {
	for _, commit := range m.committed {
		var candidates []State
		for _, state := range m.states {
			if isTerminal(state.Phase) || state.TransitionID != "" || state.TargetAccountID != "" {
				continue
			}
			if commit.RuntimeID != "" && state.RuntimeID != "" && state.RuntimeID != commit.RuntimeID {
				continue
			}
			if commit.SourceAccountID != "" && state.SourceAccountID != "" && state.SourceAccountID != commit.SourceAccountID {
				continue
			}
			candidates = append(candidates, state)
		}
		if len(candidates) != 1 {
			continue
		}
		state := candidates[0]
		state.RuntimeID = firstNonEmpty(commit.RuntimeID, state.RuntimeID)
		state.TransitionID = commit.TransitionID
		state.TargetAccountID = commit.TargetAccountID
		state.ExpectedGeneration = commit.ExpectedGeneration
		state.Phase = PhaseBound
		state.Reason = "recovery bound to replayed committed transition"
		state.UpdatedAt = m.nowUTC()
		m.states[state.RecoveryID] = state
	}
}

func (m *Manager) appendLocked(event journal.Event) error {
	if m.journal == nil {
		return nil
	}
	if event.At.IsZero() {
		event.At = m.nowUTC()
	}
	return m.journal.Append(event)
}

func (m *Manager) nowUTC() time.Time {
	if m == nil || m.now == nil {
		return time.Now().UTC()
	}
	return m.now().UTC()
}

func phaseRank(phase Phase) int {
	switch phase {
	case PhaseParked:
		return 1
	case PhaseWaiting:
		return 2
	case PhaseBound:
		return 3
	case PhaseReleaseRequested:
		return 4
	case PhaseReleased:
		return 5
	case PhaseStarted:
		return 6
	case PhaseCompleted, PhaseFailed:
		return 7
	default:
		return 0
	}
}

func isTerminal(phase Phase) bool {
	return phase == PhaseStarted || phase == PhaseCompleted || phase == PhaseFailed || phase == PhaseReleased
}

func phaseFromRuntime(phase runtime.RecoveryPhase) Phase {
	switch phase {
	case runtime.RecoveryReleasedPhase:
		return PhaseReleased
	case runtime.RecoveryStartedPhase:
		return PhaseStarted
	case runtime.RecoveryCompletedPhase:
		return PhaseCompleted
	default:
		return PhaseParked
	}
}

func isRecoveryEvent(eventType journal.EventType) bool {
	switch eventType {
	case journal.RecoveryParked, journal.RecoveryWaiting, journal.RecoveryBound,
		journal.RecoveryReleaseRequested, journal.RecoveryReleased,
		journal.RecoveryStarted, journal.RecoveryCompleted, journal.RecoveryUncertain:
		return true
	default:
		return false
	}
}

func stateEqual(left, right State) bool {
	return left.RecoveryID == right.RecoveryID && left.RuntimeID == right.RuntimeID && left.ThreadID == right.ThreadID && left.TurnID == right.TurnID && left.SourceAccountID == right.SourceAccountID && left.TransitionID == right.TransitionID && left.TargetAccountID == right.TargetAccountID && left.ExpectedGeneration == right.ExpectedGeneration && left.Phase == right.Phase && left.Reason == right.Reason
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func firstNonZero(values ...uint64) uint64 {
	for _, value := range values {
		if value != 0 {
			return value
		}
	}
	return 0
}

func accountID(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func safeWaitingReason(reason string) string {
	if strings.TrimSpace(reason) == "" {
		return "all accounts exhausted; recovery remains parked"
	}
	lower := strings.ToLower(reason)
	for _, marker := range []string{"token", "prompt", "auth.json", "bearer", "secret", "password", "api_key"} {
		if strings.Contains(lower, marker) {
			return "all accounts exhausted; recovery remains parked"
		}
	}
	if len(reason) > 160 || strings.ContainsAny(reason, "\r\n\x00") {
		return "all accounts exhausted; recovery remains parked"
	}
	return reason
}

func safeFailureReason(reason string) string {
	if strings.TrimSpace(reason) == "" {
		return "recovery release acknowledgement is uncertain"
	}
	return safeWaitingReason(reason)
}
