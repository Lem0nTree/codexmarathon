package transitions

import (
	"context"
	"errors"
	"fmt"
	"time"

	"codexmarathon/controller/internal/credentials"
	"codexmarathon/controller/internal/journal"
	"codexmarathon/controller/internal/runtime"
)

// RequestTransition creates one controller-owned transition and drives it to
// a terminal result.  The transition ID and expected generation are allocated
// here; callers cannot supply either value and therefore cannot accidentally
// reuse an old command correlation.
//
// If the runtime reports an active turn, the method waits for the matching
// safe-boundary event (or a runtime state read showing zero active turns) while
// retaining the prepared transition.  A caller can also deliver the event via
// HandleEvent.  Context cancellation leaves the transition unresolved so a
// later Reconcile call can determine whether the runtime accepted the intent.
func (c *Coordinator) RequestTransition(ctx context.Context, targetAccountID string) (*TransitionResult, error) {
	if err := c.validateDependencies(); err != nil {
		return nil, err
	}
	if targetAccountID == "" {
		return nil, errors.New("target account id is required")
	}
	if ctx == nil {
		ctx = context.Background()
	}

	initial, err := c.runtime.GetRuntimeState(ctx)
	if err != nil {
		return nil, fmt.Errorf("read runtime state before transition: %w", err)
	}
	if err := validateRuntimeIdentity(initial.Identity); err != nil {
		return nil, err
	}
	if initial.Identity.AuthGeneration == ^uint64(0) {
		return nil, fmt.Errorf("%w: generation overflow", ErrGenerationMismatch)
	}

	c.mu.Lock()
	if pending := c.activeLocked(); pending != nil {
		c.mu.Unlock()
		return nil, fmt.Errorf("%w: %s", ErrTransitionInProgress, pending.state.TransitionID)
	}
	// A request for the identity already held by the runtime is a verified
	// no-op.  It does not consume a new generation or touch auth.json.
	if initial.Identity.AccountID != nil && *initial.Identity.AccountID == targetAccountID && initial.PendingTransition == nil {
		result := TransitionResult{
			TargetAccountID:    targetAccountID,
			AdoptedAccountID:   targetAccountID,
			ExpectedGeneration: initial.Identity.AuthGeneration,
			FinalGeneration:    initial.Identity.AuthGeneration,
			Outcome:            TransitionCommitted,
			CompletedAt:        c.nowUTC(),
			RuntimeID:          initial.Identity.RuntimeID,
			DiskAccountID:      targetAccountID,
			Reason:             "target identity is already active",
		}
		c.mu.Unlock()
		return &result, nil
	}
	if initial.PendingTransition != nil {
		pendingID := initial.PendingTransition.TransitionID
		c.mu.Unlock()
		return nil, fmt.Errorf("%w: runtime has pending transition %s", ErrTransitionInProgress, pendingID)
	}
	transitionID := c.nextTransitionID()
	params := runtime.AuthTransitionParams{
		TransitionID:       transitionID,
		TargetAccountID:    targetAccountID,
		ExpectedGeneration: initial.Identity.AuthGeneration + 1,
	}
	now := c.nowUTC()
	record := &transitionRecord{
		state: State{
			TransitionID:       transitionID,
			RuntimeID:          initial.Identity.RuntimeID,
			CurrentAccountID:   accountID(initial.Identity.AccountID),
			TargetAccountID:    targetAccountID,
			ExpectedGeneration: params.ExpectedGeneration,
			Phase:              PhasePrepared,
			CreatedAt:          now,
			UpdatedAt:          now,
			ActiveTurnCount:    initial.ActiveTurnCount,
		},
		oldIdentity: initial.Identity,
		params:      params,
	}
	record.state.DiskAccountID = record.state.CurrentAccountID
	c.transitions[transitionID] = record
	c.current = cloneRecordState(record)
	c.mu.Unlock()

	if err := c.append(journal.Event{
		Type:            journal.TransitionCreated,
		TransitionID:    transitionID,
		AuthGeneration:  initial.Identity.AuthGeneration,
		RuntimeID:       initial.Identity.RuntimeID,
		FromAccountID:   record.state.CurrentAccountID,
		TargetAccountID: targetAccountID,
	}); err != nil {
		return c.markUncertain(transitionID, fmt.Errorf("journal transition creation: %w", err))
	}

	prepared, err := c.runtime.PrepareAuthTransition(ctx, params)
	if err != nil {
		return c.markUncertain(transitionID, fmt.Errorf("prepare transition: %w", err))
	}
	if prepared.Outcome == runtime.TransitionRejected {
		return c.finishRejected(transitionID, prepared, errors.New("runtime rejected transition preparation"))
	}
	if prepared.Outcome == runtime.TransitionUncertain {
		return c.markUncertain(transitionID, errors.New("runtime returned uncertain preparation outcome"))
	}
	if err := c.validateTransitionAck(prepared, record, false); err != nil {
		// A typed rejected result is terminal only when the runtime gave us a
		// correlated response.  Correlation or generation failures leave the
		// state uncertain because the command may have been accepted.
		if prepared.Outcome == runtime.TransitionRejected {
			return c.finishRejected(transitionID, prepared, err)
		}
		return c.markUncertain(transitionID, err)
	}
	c.mu.Lock()
	record = c.transitions[transitionID]
	if record == nil {
		c.mu.Unlock()
		return nil, ErrTransitionNotFound
	}
	record.state.PrepareSent = true
	record.state.UpdatedAt = c.nowUTC()
	if c.current != nil && c.current.TransitionID == transitionID {
		copy := record.state
		c.current = &copy
	}
	c.mu.Unlock()
	if err := c.append(journal.Event{
		Type:            journal.TransitionPrepared,
		TransitionID:    transitionID,
		AuthGeneration:  initial.Identity.AuthGeneration,
		RuntimeID:       initial.Identity.RuntimeID,
		FromAccountID:   record.state.CurrentAccountID,
		TargetAccountID: targetAccountID,
	}); err != nil {
		return c.markUncertain(transitionID, fmt.Errorf("journal transition preparation: %w", err))
	}

	if err := c.waitForSafeBoundary(ctx, transitionID, initial.ActiveTurnCount); err != nil {
		return c.markUncertain(transitionID, err)
	}
	return c.deployAndCommit(ctx, transitionID)
}

// Request is a concise alias for RequestTransition.
func (c *Coordinator) Request(ctx context.Context, targetAccountID string) (*TransitionResult, error) {
	return c.RequestTransition(ctx, targetAccountID)
}

func (c *Coordinator) nextTransitionID() string {
	// idGenerator is configured at construction and is expected to be
	// side-effect free.  A local fallback protects a zero-value Coordinator
	// from panicking while still producing unique IDs.
	if c.idGenerator != nil {
		return c.idGenerator()
	}
	return fmt.Sprintf("tx-%d", c.sequence.Add(1))
}

func (c *Coordinator) activeLocked() *transitionRecord {
	for _, record := range c.transitions {
		switch record.state.Phase {
		case PhasePrepared, PhaseWaitingForBoundary, PhaseDeploying, PhaseCommitSent, PhaseUncertain:
			return record
		}
	}
	return nil
}

func cloneRecordState(record *transitionRecord) *State {
	if record == nil {
		return nil
	}
	state := record.state
	return &state
}

func accountID(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func validateRuntimeIdentity(identity runtime.Identity) error {
	if identity.RuntimeID == "" {
		return errors.New("runtime id is required")
	}
	return nil
}

func (c *Coordinator) validateTransitionAck(ack runtime.TransitionResult, record *transitionRecord, requireCommitted bool) error {
	if record == nil {
		return ErrTransitionNotFound
	}
	if ack.TransitionID != record.params.TransitionID {
		return fmt.Errorf("%w: expected %q, got %q", ErrTransitionIDMismatch, record.params.TransitionID, ack.TransitionID)
	}
	if ack.RuntimeID != "" && ack.RuntimeID != record.state.RuntimeID {
		return fmt.Errorf("%w: expected %q, got %q", ErrRuntimeIDMismatch, record.state.RuntimeID, ack.RuntimeID)
	}
	if ack.ExpectedGeneration != 0 && ack.ExpectedGeneration != record.params.ExpectedGeneration {
		return fmt.Errorf("%w: expected transition generation %d, got %d", ErrGenerationMismatch, record.params.ExpectedGeneration, ack.ExpectedGeneration)
	}
	if ack.Outcome == runtime.TransitionRejected {
		if requireCommitted {
			return fmt.Errorf("runtime rejected transition")
		}
		return nil
	}
	if requireCommitted && ack.Outcome != runtime.TransitionCommitted {
		return fmt.Errorf("runtime returned %q, want committed", ack.Outcome)
	}
	return nil
}

func (c *Coordinator) waitForSafeBoundary(ctx context.Context, transitionID string, initialActive int) error {
	c.mu.Lock()
	record := c.transitions[transitionID]
	if record == nil {
		c.mu.Unlock()
		return ErrTransitionNotFound
	}
	if initialActive <= 0 {
		record.state.ActiveTurnCount = 0
		record.state.Phase = PhaseDeploying
		record.state.UpdatedAt = c.nowUTC()
		if c.current != nil && c.current.TransitionID == transitionID {
			copy := record.state
			c.current = &copy
		}
		c.mu.Unlock()
		return nil
	}
	record.state.Phase = PhaseWaitingForBoundary
	record.state.UpdatedAt = c.nowUTC()
	boundary := c.boundary[transitionID]
	if boundary == nil {
		boundary = make(chan struct{}, 1)
		c.boundary[transitionID] = boundary
	}
	if c.current != nil && c.current.TransitionID == transitionID {
		copy := record.state
		c.current = &copy
	}
	c.mu.Unlock()

	var events <-chan runtime.Event
	if source, ok := c.runtime.(RuntimeEvents); ok {
		events = source.Events()
	}
	var timer *time.Timer
	if events == nil {
		timer = time.NewTimer(c.boundaryPollInterval)
		defer timer.Stop()
	}
	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("%w: %v", ErrSafeBoundaryPending, ctx.Err())
		case <-boundary:
			c.markBoundaryReached(transitionID)
			return nil
		case event, ok := <-events:
			if !ok {
				return fmt.Errorf("%w: runtime event stream closed", ErrSafeBoundaryPending)
			}
			matched, err := c.consumeBoundaryEvent(event, transitionID)
			if err != nil {
				return err
			}
			if matched {
				c.markBoundaryReached(transitionID)
				return nil
			}
		case <-timerC(timer):
			state, err := c.runtime.GetRuntimeState(ctx)
			if err != nil {
				return fmt.Errorf("read runtime state while waiting for safe boundary: %w", err)
			}
			if state.Identity.RuntimeID != c.runtimeIDFor(transitionID) {
				return fmt.Errorf("%w: runtime changed while waiting", ErrRuntimeIDMismatch)
			}
			if state.Identity.AuthGeneration != c.oldGenerationFor(transitionID) {
				return fmt.Errorf("%w: runtime generation changed while waiting", ErrGenerationMismatch)
			}
			if state.ActiveTurnCount <= 0 {
				c.markBoundaryReached(transitionID)
				return nil
			}
			c.mu.Lock()
			if record := c.transitions[transitionID]; record != nil {
				record.state.ActiveTurnCount = state.ActiveTurnCount
				record.state.UpdatedAt = c.nowUTC()
				if c.current != nil && c.current.TransitionID == transitionID {
					copy := record.state
					c.current = &copy
				}
			}
			c.mu.Unlock()
			resetTimer(timer, c.boundaryPollInterval)
		}
	}
}

func timerC(timer *time.Timer) <-chan time.Time {
	if timer == nil {
		return nil
	}
	return timer.C
}

func resetTimer(timer *time.Timer, duration time.Duration) {
	if timer == nil {
		return
	}
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
	timer.Reset(duration)
}

func (c *Coordinator) runtimeIDFor(transitionID string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if record := c.transitions[transitionID]; record != nil {
		return record.state.RuntimeID
	}
	return ""
}

func (c *Coordinator) oldGenerationFor(transitionID string) uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	if record := c.transitions[transitionID]; record != nil {
		return record.oldIdentity.AuthGeneration
	}
	return 0
}

func (c *Coordinator) markBoundaryReached(transitionID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if record := c.transitions[transitionID]; record != nil {
		if record.state.Phase == PhaseWaitingForBoundary || record.state.Phase == PhasePrepared {
			record.state.ActiveTurnCount = 0
			record.state.Phase = PhaseDeploying
			record.state.UpdatedAt = c.nowUTC()
			if c.current != nil && c.current.TransitionID == transitionID {
				copy := record.state
				c.current = &copy
			}
		}
	}
}

func (c *Coordinator) deployAndCommit(ctx context.Context, transitionID string) (*TransitionResult, error) {
	c.mu.Lock()
	record := c.transitions[transitionID]
	if record == nil {
		c.mu.Unlock()
		return nil, ErrTransitionNotFound
	}
	params := record.params
	c.mu.Unlock()

	deployed, err := c.deployer.Deploy(params.TargetAccountID)
	if err != nil {
		// Nothing after a failed deploy is assumed to have happened.  A
		// deployer may report an uncertain directory sync, however, so keep
		// the transition unresolved and let reconciliation inspect disk.
		return c.markUncertain(transitionID, fmt.Errorf("deploy target credentials: %w", err))
	}
	c.mu.Lock()
	record = c.transitions[transitionID]
	if record == nil {
		c.mu.Unlock()
		return nil, ErrTransitionNotFound
	}
	record.state.DiskAccountID = deployed.AccountID
	if record.state.DiskAccountID == "" {
		record.state.DiskAccountID = params.TargetAccountID
	}
	record.state.UpdatedAt = c.nowUTC()
	if c.current != nil && c.current.TransitionID == transitionID {
		copy := record.state
		c.current = &copy
	}
	diskAccountID := record.state.DiskAccountID
	runtimeID := record.state.RuntimeID
	oldGeneration := record.oldIdentity.AuthGeneration
	c.mu.Unlock()
	if err := c.append(journal.Event{
		Type:            journal.AuthDeployed,
		TransitionID:    transitionID,
		AuthGeneration:  params.ExpectedGeneration,
		RuntimeID:       runtimeID,
		AccountID:       diskAccountID,
		TargetAccountID: params.TargetAccountID,
		Reason:          "atomic credential deployment completed",
	}); err != nil {
		return c.markUncertain(transitionID, fmt.Errorf("journal credential deployment: %w", err))
	}

	c.mu.Lock()
	record = c.transitions[transitionID]
	if record == nil {
		c.mu.Unlock()
		return nil, ErrTransitionNotFound
	}
	record.state.CommitSent = true
	record.state.CommitAttempts++
	record.state.Phase = PhaseCommitSent
	record.state.UpdatedAt = c.nowUTC()
	if c.current != nil && c.current.TransitionID == transitionID {
		copy := record.state
		c.current = &copy
	}
	attempt := record.state.CommitAttempts
	c.mu.Unlock()
	if err := c.append(journal.Event{
		Type:            journal.CommitSent,
		TransitionID:    transitionID,
		AuthGeneration:  params.ExpectedGeneration,
		RuntimeID:       runtimeID,
		TargetAccountID: params.TargetAccountID,
		Reason:          fmt.Sprintf("commit attempt %d", attempt),
	}); err != nil {
		return c.markUncertain(transitionID, fmt.Errorf("journal commit request: %w", err))
	}

	ack, err := c.runtime.CommitAuthTransition(ctx, params)
	if err != nil {
		return c.markUncertain(transitionID, fmt.Errorf("commit transition: %w", err))
	}
	if err := c.validateTransitionAck(ack, record, true); err != nil {
		return c.markUncertain(transitionID, err)
	}
	result, err := c.verifyCommitted(ctx, transitionID, ack, oldGeneration)
	if err != nil {
		return c.markUncertain(transitionID, err)
	}
	return result, nil
}

func (c *Coordinator) verifyCommitted(ctx context.Context, transitionID string, ack runtime.TransitionResult, oldGeneration uint64) (*TransitionResult, error) {
	c.mu.Lock()
	record := c.transitions[transitionID]
	if record == nil {
		c.mu.Unlock()
		return nil, ErrTransitionNotFound
	}
	target := record.state.TargetAccountID
	expected := record.state.ExpectedGeneration
	runtimeID := record.state.RuntimeID
	disk := record.state.DiskAccountID
	c.mu.Unlock()
	identity, err := c.runtime.GetIdentity(ctx)
	if err != nil {
		return nil, fmt.Errorf("verify runtime identity after commit: %w", err)
	}
	if identity.RuntimeID != runtimeID {
		return nil, fmt.Errorf("%w: expected %q, got %q", ErrRuntimeIDMismatch, runtimeID, identity.RuntimeID)
	}
	if identity.AuthGeneration != expected {
		return nil, fmt.Errorf("%w: committed generation %d, got %d", ErrGenerationMismatch, expected, identity.AuthGeneration)
	}
	if accountID(identity.AccountID) != target {
		return nil, fmt.Errorf("%w: committed account %q, got %q", ErrIdentityMismatch, target, accountID(identity.AccountID))
	}
	if disk != "" && disk != target {
		return nil, fmt.Errorf("%w: deployed account %q, expected %q", ErrIdentityMismatch, disk, target)
	}
	if ack.AuthGeneration != 0 && ack.AuthGeneration != expected {
		return nil, fmt.Errorf("%w: ack generation %d, expected %d", ErrGenerationMismatch, ack.AuthGeneration, expected)
	}
	if ack.AccountID != nil && *ack.AccountID != target {
		return nil, fmt.Errorf("%w: ack account %q, expected %q", ErrIdentityMismatch, *ack.AccountID, target)
	}
	result := TransitionResult{
		TransitionID:       transitionID,
		TargetAccountID:    target,
		AdoptedAccountID:   accountID(identity.AccountID),
		ExpectedGeneration: expected,
		FinalGeneration:    identity.AuthGeneration,
		Outcome:            TransitionCommitted,
		CompletedAt:        c.nowUTC(),
		RuntimeID:          identity.RuntimeID,
		DiskAccountID:      target,
		Reason:             "runtime identity confirmed after commit",
	}
	c.mu.Lock()
	if record = c.transitions[transitionID]; record != nil {
		record.state.AdoptedAccountID = result.AdoptedAccountID
		record.state.FinalGeneration = result.FinalGeneration
		record.state.DiskAccountID = target
		c.setStateLocked(record, PhaseCommitted, result.Reason)
	}
	c.mu.Unlock()
	if err := c.append(journal.Event{
		Type:            journal.IdentityConfirmed,
		TransitionID:    transitionID,
		AuthGeneration:  result.FinalGeneration,
		RuntimeID:       result.RuntimeID,
		AccountID:       result.AdoptedAccountID,
		TargetAccountID: target,
	}); err != nil {
		return c.markUncertain(transitionID, fmt.Errorf("journal identity confirmation: %w", err))
	}
	if err := c.append(journal.Event{
		Type:            journal.TransitionCommitted,
		TransitionID:    transitionID,
		AuthGeneration:  result.FinalGeneration,
		RuntimeID:       result.RuntimeID,
		AccountID:       result.AdoptedAccountID,
		TargetAccountID: target,
		Outcome:         string(result.Outcome),
	}); err != nil {
		return c.markUncertain(transitionID, fmt.Errorf("journal transition commit: %w", err))
	}
	_ = oldGeneration // retained in the signature for future monotonic checks.
	return &result, nil
}

func (c *Coordinator) finishRejected(transitionID string, ack runtime.TransitionResult, reason error) (*TransitionResult, error) {
	c.mu.Lock()
	record := c.transitions[transitionID]
	if record == nil {
		c.mu.Unlock()
		return nil, ErrTransitionNotFound
	}
	target := record.state.TargetAccountID
	runtimeID := record.state.RuntimeID
	expected := record.state.ExpectedGeneration
	result := TransitionResult{
		TransitionID:       transitionID,
		TargetAccountID:    target,
		AdoptedAccountID:   accountID(ack.AccountID),
		ExpectedGeneration: expected,
		FinalGeneration:    ack.AuthGeneration,
		Outcome:            TransitionRejected,
		CompletedAt:        c.nowUTC(),
		RuntimeID:          runtimeID,
		DiskAccountID:      record.state.DiskAccountID,
		Reason:             reason.Error(),
	}
	c.setStateLocked(record, PhaseRejected, result.Reason)
	c.mu.Unlock()
	return &result, nil
}

// markUncertain records an unresolved transition and returns the result plus
// the triggering error.  Keeping both values lets a caller surface transport
// failure while retaining the reconciliation handle.
func (c *Coordinator) markUncertain(transitionID string, reason error) (*TransitionResult, error) {
	if reason == nil {
		reason = ErrReconciliationNeeded
	}
	c.mu.Lock()
	record := c.transitions[transitionID]
	if record == nil {
		c.mu.Unlock()
		return nil, fmt.Errorf("%w: %s", ErrTransitionNotFound, transitionID)
	}
	state := record.state
	c.setStateLocked(record, PhaseUncertain, reason.Error())
	result := TransitionResult{
		TransitionID:       state.TransitionID,
		TargetAccountID:    state.TargetAccountID,
		AdoptedAccountID:   state.AdoptedAccountID,
		ExpectedGeneration: state.ExpectedGeneration,
		FinalGeneration:    state.FinalGeneration,
		Outcome:            TransitionUncertain,
		CompletedAt:        c.nowUTC(),
		RuntimeID:          state.RuntimeID,
		DiskAccountID:      state.DiskAccountID,
		Reason:             reason.Error(),
	}
	c.mu.Unlock()
	_ = c.append(journal.Event{
		Type:            journal.TransitionUncertain,
		TransitionID:    transitionID,
		AuthGeneration:  state.ExpectedGeneration,
		RuntimeID:       state.RuntimeID,
		AccountID:       state.DiskAccountID,
		TargetAccountID: state.TargetAccountID,
		Outcome:         string(TransitionUncertain),
		Reason:           reason.Error(),
	})
	return &result, reason
}

func (c *Coordinator) append(event journal.Event) error {
	if c == nil || c.journal == nil {
		return nil
	}
	return c.journal.Append(event)
}

// CancelTransition explicitly abandons a prepared transition.  It is useful
// when an operator chooses not to reconcile a context-cancelled request.  A
// lost cancellation acknowledgement is represented as uncertain as with any
// other state-changing command.
func (c *Coordinator) CancelTransition(ctx context.Context, transitionID, reason string) (*TransitionResult, error) {
	if err := c.validateDependencies(); err != nil {
		return nil, err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	c.mu.Lock()
	record := c.transitions[transitionID]
	if record == nil {
		c.mu.Unlock()
		return nil, fmt.Errorf("%w: %s", ErrTransitionNotFound, transitionID)
	}
	params := runtime.CancelAuthTransitionParams{TransitionID: transitionID, ExpectedGeneration: record.params.ExpectedGeneration, Reason: reason}
	c.mu.Unlock()
	ack, err := c.runtime.CancelAuthTransition(ctx, params)
	if err != nil {
		return c.markUncertain(transitionID, fmt.Errorf("cancel transition: %w", err))
	}
	if ack.TransitionID != transitionID {
		return c.markUncertain(transitionID, fmt.Errorf("%w: cancel response %q", ErrTransitionIDMismatch, ack.TransitionID))
	}
	if ack.Outcome == runtime.TransitionRejected || ack.Outcome == runtime.TransitionUncertain {
		return c.markUncertain(transitionID, errors.New("runtime did not confirm cancellation"))
	}
	c.mu.Lock()
	record = c.transitions[transitionID]
	target := ""
	if record != nil {
		target = record.state.TargetAccountID
		c.setStateLocked(record, PhaseRejected, reason)
	}
	c.mu.Unlock()
	result := &TransitionResult{TransitionID: transitionID, TargetAccountID: target, Outcome: TransitionRejected, CompletedAt: c.nowUTC(), Reason: reason}
	return result, nil
}

// Cancel is an alias for CancelTransition.
func (c *Coordinator) Cancel(ctx context.Context, transitionID, reason string) (*TransitionResult, error) {
	return c.CancelTransition(ctx, transitionID, reason)
}

// ensure imported package remains part of the public seam even when callers
// provide a custom CredentialDeployer implementation.
var _ CredentialDeployer = (*credentials.AtomicDeployer)(nil)
