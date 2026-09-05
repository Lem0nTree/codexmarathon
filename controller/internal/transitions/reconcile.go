package transitions

import (
	"context"
	"errors"
	"fmt"

	"codexmarathon/controller/internal/journal"
	"codexmarathon/controller/internal/runtime"
)

// ReconciliationDecision classifies the independently observed disk and
// runtime state.  The values are deliberately descriptive because operators
// should be able to distinguish a safe commit from a split-brain condition.
type ReconciliationDecision string

const (
	ReconcileCommitted          ReconciliationDecision = "committed"
	ReconcileRuntimeReload      ReconciliationDecision = "runtime_reload_required"
	ReconcileRejected           ReconciliationDecision = "rejected"
	ReconcileSplitBrain         ReconciliationDecision = "split_brain"
	ReconcileGenerationMismatch ReconciliationDecision = "generation_mismatch"
	ReconcileUnknown            ReconciliationDecision = "unknown"
)

// IdentityObservation is the complete non-secret comparison input.  Empty
// account IDs mean unauthenticated/unknown, never an inferred account.
type IdentityObservation struct {
	DesiredAccountID     string
	PreviousAccountID    string
	DiskAccountID        string
	RuntimeAccountID     string
	ExpectedRuntimeID    string
	RuntimeID            string
	ControllerGeneration uint64
	RuntimeGeneration    uint64
	ExpectedGeneration   uint64
}

// ReconciliationObservation is a compatibility alias.
type ReconciliationObservation = IdentityObservation

// CompareIdentity compares the disk, runtime, and generation authorities.  It
// performs no I/O and never treats a matching account with a mismatching
// generation as committed.
func CompareIdentity(observation IdentityObservation) ReconciliationDecision {
	if observation.DesiredAccountID == "" {
		return ReconcileUnknown
	}
	if observation.ExpectedRuntimeID != "" && observation.RuntimeID != observation.ExpectedRuntimeID {
		return ReconcileSplitBrain
	}
	expected := observation.ExpectedGeneration
	if expected == 0 && observation.ControllerGeneration != ^uint64(0) {
		expected = observation.ControllerGeneration + 1
	}
	old := observation.ControllerGeneration
	if old == 0 && expected > 0 {
		old = expected - 1
	}
	runtimeAtTarget := observation.RuntimeAccountID == observation.DesiredAccountID
	diskAtTarget := observation.DiskAccountID == observation.DesiredAccountID
	runtimeAtPrevious := observation.RuntimeAccountID == observation.PreviousAccountID || (observation.PreviousAccountID == "" && observation.RuntimeAccountID == "")
	diskAtPrevious := observation.DiskAccountID == observation.PreviousAccountID || (observation.PreviousAccountID == "" && observation.DiskAccountID == "")

	controllerMatchesCommit := observation.ControllerGeneration == 0 || observation.ControllerGeneration == expected
	// A lost acknowledgement can leave the controller's last durable value at
	// the source generation while disk and runtime have already advanced. That
	// one-generation lag is observable and safe to adopt after the runtime
	// identity/generation checks below.
	if expected > 0 && observation.ControllerGeneration+1 == expected {
		controllerMatchesCommit = true
	}
	if runtimeAtTarget && diskAtTarget && expected > 0 && observation.RuntimeGeneration == expected && controllerMatchesCommit {
		return ReconcileCommitted
	}
	// The target is durably on disk but the runtime still reports the prior
	// generation.  Retrying the same correlated commit is safe and is not a
	// second transition.
	if diskAtTarget && runtimeAtPrevious && expected > 0 && observation.RuntimeGeneration == old {
		return ReconcileRuntimeReload
	}
	// A failed/never-applied deploy leaves both authorities at the source.  It
	// is safe to close the transition as rejected because no target state is
	// observable.  The caller may issue a fresh transition later.
	if diskAtPrevious && runtimeAtPrevious && (expected == 0 || observation.RuntimeGeneration == old) {
		return ReconcileRejected
	}
	// The runtime adopted the target but the disk is still on the source, or
	// both sides claim the target with a stale generation.  These are explicit
	// split-brain/ordering failures and must not be guessed into committed.
	if runtimeAtTarget != diskAtTarget {
		return ReconcileSplitBrain
	}
	if runtimeAtTarget && diskAtTarget {
		return ReconcileGenerationMismatch
	}
	return ReconcileUnknown
}

// Compare is a short alias for CompareIdentity.
func Compare(observation IdentityObservation) ReconciliationDecision {
	return CompareIdentity(observation)
}

// Reconcile compares the stored transition intent with fresh runtime and disk
// observations.  A resolved transition is returned idempotently; an
// unresolved transition remains the coordinator's active state and blocks new
// requests.
func (c *Coordinator) Reconcile(ctx context.Context, transitionID string) (*TransitionResult, error) {
	if err := c.validateDependencies(); err != nil {
		return nil, err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	c.mu.Lock()
	if transitionID == "" {
		if active := c.activeLocked(); active != nil {
			transitionID = active.state.TransitionID
		}
	}
	record := c.transitions[transitionID]
	if record == nil {
		c.mu.Unlock()
		return nil, fmt.Errorf("%w: %s", ErrTransitionNotFound, transitionID)
	}
	if record.state.Phase == PhaseCommitted || record.state.Phase == PhaseRejected {
		result := resultFromState(record.state)
		c.mu.Unlock()
		return &result, nil
	}
	c.mu.Unlock()

	state, err := c.runtime.GetRuntimeState(ctx)
	if err != nil {
		return c.markUncertain(transitionID, fmt.Errorf("reconcile runtime state: %w", err))
	}
	if err := validateRuntimeIdentity(state.Identity); err != nil {
		return c.markUncertain(transitionID, err)
	}
	c.mu.Lock()
	record = c.transitions[transitionID]
	if record == nil {
		c.mu.Unlock()
		return nil, ErrTransitionNotFound
	}
	// A runtime restart can preserve an identity but not the connection's
	// runtime ID.  Treat that as unresolved rather than applying a command to a
	// different process.
	if state.Identity.RuntimeID != record.state.RuntimeID {
		c.mu.Unlock()
		return c.markUncertain(transitionID, fmt.Errorf("%w: expected %q, got %q", ErrRuntimeIDMismatch, record.state.RuntimeID, state.Identity.RuntimeID))
	}
	previous := record.oldIdentity
	desired := record.state.TargetAccountID
	expected := record.state.ExpectedGeneration
	storedDisk := record.state.DiskAccountID
	c.mu.Unlock()

	disk := storedDisk
	if c.disk != nil {
		disk, err = c.disk.ReadIdentity()
		if err != nil {
			return c.markUncertain(transitionID, fmt.Errorf("read deployed auth identity: %w", err))
		}
		c.mu.Lock()
		if record = c.transitions[transitionID]; record != nil {
			record.state.DiskAccountID = disk
			record.state.UpdatedAt = c.nowUTC()
			if c.current != nil && c.current.TransitionID == transitionID {
				copy := record.state
				c.current = &copy
			}
		}
		c.mu.Unlock()
	}

	decision := CompareIdentity(IdentityObservation{
		DesiredAccountID:     desired,
		PreviousAccountID:    accountID(previous.AccountID),
		DiskAccountID:        disk,
		RuntimeAccountID:     accountID(state.Identity.AccountID),
		ExpectedRuntimeID:    recordRuntimeID(c, transitionID),
		RuntimeID:            state.Identity.RuntimeID,
		ControllerGeneration: previous.AuthGeneration,
		RuntimeGeneration:    state.Identity.AuthGeneration,
		ExpectedGeneration:   expected,
	})

	switch decision {
	case ReconcileCommitted:
		return c.finishReconciled(transitionID, TransitionCommitted, state.Identity, disk, "runtime and disk already agree at expected generation")
	case ReconcileRejected:
		return c.finishReconciled(transitionID, TransitionRejected, state.Identity, disk, "runtime and disk remain on prior identity")
	case ReconcileRuntimeReload:
		return c.retryCommitForReconciliation(ctx, transitionID, state.Identity, disk)
	case ReconcileSplitBrain:
		return c.markUncertain(transitionID, ErrSplitBrain)
	case ReconcileGenerationMismatch:
		return c.markUncertain(transitionID, ErrGenerationMismatch)
	default:
		return c.markUncertain(transitionID, ErrReconciliationNeeded)
	}
}

// ReconcileTransition is an alias for Reconcile.
func (c *Coordinator) ReconcileTransition(ctx context.Context, transitionID string) (*TransitionResult, error) {
	return c.Reconcile(ctx, transitionID)
}

func recordRuntimeID(c *Coordinator, transitionID string) string {
	if c == nil {
		return ""
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if record := c.transitions[transitionID]; record != nil {
		return record.state.RuntimeID
	}
	return ""
}

func resultFromState(state State) TransitionResult {
	outcome := TransitionUncertain
	switch state.Phase {
	case PhaseCommitted:
		outcome = TransitionCommitted
	case PhaseRejected:
		outcome = TransitionRejected
	}
	return TransitionResult{
		TransitionID:       state.TransitionID,
		TargetAccountID:    state.TargetAccountID,
		AdoptedAccountID:   state.AdoptedAccountID,
		ExpectedGeneration: state.ExpectedGeneration,
		FinalGeneration:    state.FinalGeneration,
		Outcome:            outcome,
		CompletedAt:        state.CompletedAt,
		RuntimeID:          state.RuntimeID,
		DiskAccountID:      state.DiskAccountID,
		Reason:             state.Reason,
	}
}

func (c *Coordinator) finishReconciled(transitionID string, outcome TransitionOutcome, identity runtime.Identity, disk, reason string) (*TransitionResult, error) {
	c.mu.Lock()
	record := c.transitions[transitionID]
	if record == nil {
		c.mu.Unlock()
		return nil, ErrTransitionNotFound
	}
	result := TransitionResult{
		TransitionID:       transitionID,
		TargetAccountID:    record.state.TargetAccountID,
		ExpectedGeneration: record.state.ExpectedGeneration,
		FinalGeneration:    identity.AuthGeneration,
		Outcome:            outcome,
		CompletedAt:        c.nowUTC(),
		RuntimeID:          identity.RuntimeID,
		DiskAccountID:      disk,
		Reason:             reason,
	}
	if identity.AccountID != nil {
		result.AdoptedAccountID = *identity.AccountID
	}
	record.state.AdoptedAccountID = result.AdoptedAccountID
	record.state.FinalGeneration = result.FinalGeneration
	record.state.DiskAccountID = disk
	phase := PhaseRejected
	if outcome == TransitionCommitted {
		phase = PhaseCommitted
	}
	c.setStateLocked(record, phase, reason)
	c.mu.Unlock()

	if err := c.append(journal.Event{
		Type:            journal.TransitionReconciled,
		TransitionID:    transitionID,
		AuthGeneration:  result.FinalGeneration,
		RuntimeID:       result.RuntimeID,
		AccountID:       result.AdoptedAccountID,
		TargetAccountID: result.TargetAccountID,
		Outcome:         string(outcome),
		Reason:          reason,
	}); err != nil {
		return c.markUncertain(transitionID, fmt.Errorf("journal reconciliation: %w", err))
	}
	return &result, nil
}

func (c *Coordinator) retryCommitForReconciliation(ctx context.Context, transitionID string, identity runtime.Identity, disk string) (*TransitionResult, error) {
	c.mu.Lock()
	record := c.transitions[transitionID]
	if record == nil {
		c.mu.Unlock()
		return nil, ErrTransitionNotFound
	}
	params := record.params
	record.state.CommitSent = true
	record.state.CommitAttempts++
	record.state.Phase = PhaseCommitSent
	record.state.UpdatedAt = c.nowUTC()
	attempt := record.state.CommitAttempts
	runtimeID := record.state.RuntimeID
	target := record.state.TargetAccountID
	expected := record.state.ExpectedGeneration
	if c.current != nil && c.current.TransitionID == transitionID {
		copy := record.state
		c.current = &copy
	}
	c.mu.Unlock()

	if err := c.append(journal.Event{
		Type:            journal.CommitSent,
		TransitionID:    transitionID,
		AuthGeneration:  expected,
		RuntimeID:       runtimeID,
		TargetAccountID: target,
		Reason:          fmt.Sprintf("reconciliation commit attempt %d", attempt),
	}); err != nil {
		return c.markUncertain(transitionID, fmt.Errorf("journal reconciliation commit: %w", err))
	}
	ack, err := c.runtime.CommitAuthTransition(ctx, params)
	if err != nil {
		return c.markUncertain(transitionID, fmt.Errorf("reconciliation commit: %w", err))
	}
	if ack.Outcome == runtime.TransitionRejected {
		return c.markUncertain(transitionID, errors.New("runtime rejected reconciliation commit"))
	}
	if ack.Outcome == runtime.TransitionUncertain {
		return c.markUncertain(transitionID, errors.New("runtime returned uncertain reconciliation commit"))
	}
	if err := c.validateTransitionAck(ack, record, true); err != nil {
		return c.markUncertain(transitionID, err)
	}
	result, err := c.verifyCommitted(ctx, transitionID, ack, identity.AuthGeneration)
	if err != nil {
		return c.markUncertain(transitionID, err)
	}
	result.DiskAccountID = disk
	// verifyCommitted already establishes a terminal state.  Add a distinct
	// reconciliation record so recovery tooling can tell this was recovered
	// after an uncertain command.
	if journalErr := c.append(journal.Event{
		Type:            journal.TransitionReconciled,
		TransitionID:    transitionID,
		AuthGeneration:  result.FinalGeneration,
		RuntimeID:       result.RuntimeID,
		AccountID:       result.AdoptedAccountID,
		TargetAccountID: result.TargetAccountID,
		Outcome:         string(result.Outcome),
		Reason:          "runtime reload completed during reconciliation",
	}); journalErr != nil {
		return result, journalErr
	}
	return result, nil
}
