package transitions

import (
	"fmt"
	"strconv"
	"strings"

	"codexmarathon/controller/internal/journal"
	"codexmarathon/controller/internal/runtime"
)

// Restore rehydrates transition intent from the append-only journal.
//
// A Coordinator is intentionally in-memory while it is attached to a
// runtime, but the transition intent it owns is durable.  Replaying the
// journal before a reconnect is what lets Reconcile inspect a transition that
// was interrupted by a controller restart.  This method restores only
// secret-free metadata; it never reads or reconstructs credential bytes.
//
// The journal is an ordered log, so the last lifecycle record for a
// transition wins.  Incomplete histories are rejected rather than guessed:
// silently inventing a source identity or generation would make a later
// commit unsafe.
func (c *Coordinator) Restore(events []journal.Event) error {
	if c == nil {
		return ErrNilCoordinator
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.transitions == nil {
		c.transitions = make(map[string]*transitionRecord)
	}
	if c.boundary == nil {
		c.boundary = make(map[string]chan struct{})
	}
	var latest *transitionRecord
	for _, event := range events {
		if !isTransitionJournalEvent(event.Type) {
			continue
		}
		if event.TransitionID == "" {
			return fmt.Errorf("transition journal event %s has no transition_id", event.Type)
		}
		if event.RuntimeID == "" {
			return fmt.Errorf("transition journal event %s/%s has no runtime_id", event.Type, event.TransitionID)
		}

		record := c.transitions[event.TransitionID]
		if event.Type == journal.TransitionCreated {
			if record == nil {
				expected, err := replayExpectedGeneration(event)
				if err != nil {
					return fmt.Errorf("restore transition %s: %w", event.TransitionID, err)
				}
				if event.TargetAccountID == "" {
					return fmt.Errorf("restore transition %s: target account is empty", event.TransitionID)
				}
				var source *string
				if event.FromAccountID != "" {
					value := event.FromAccountID
					source = &value
				}
				record = &transitionRecord{
					state: State{
						TransitionID:       event.TransitionID,
						RuntimeID:          event.RuntimeID,
						CurrentAccountID:   event.FromAccountID,
						TargetAccountID:    event.TargetAccountID,
						DiskAccountID:      event.FromAccountID,
						ExpectedGeneration: expected,
						Phase:              PhasePrepared,
						CreatedAt:          event.At,
						UpdatedAt:          event.At,
					},
					oldIdentity: runtime.Identity{
						RuntimeID:      event.RuntimeID,
						AccountID:      source,
						AuthGeneration: event.AuthGeneration,
					},
					params: runtime.AuthTransitionParams{
						TransitionID:       event.TransitionID,
						TargetAccountID:    event.TargetAccountID,
						ExpectedGeneration: expected,
					},
				}
				c.transitions[event.TransitionID] = record
			} else if err := validateReplayCreated(record, event); err != nil {
				return err
			}
			latest = record
			continue
		}
		if record == nil {
			return fmt.Errorf("restore transition %s: lifecycle event %s precedes TransitionCreated", event.TransitionID, event.Type)
		}
		if err := validateReplayCorrelation(record, event); err != nil {
			return err
		}
		applyReplayEvent(record, event)
		latest = record
	}

	var unresolved *transitionRecord
	for _, record := range c.transitions {
		if record == nil {
			continue
		}
		if record.state.Phase == PhaseCommitted || record.state.Phase == PhaseRejected || record.state.Phase == PhaseIdle {
			continue
		}
		if unresolved != nil {
			return fmt.Errorf("restore transition journal: multiple unresolved transitions (%s and %s)", unresolved.state.TransitionID, record.state.TransitionID)
		}
		unresolved = record
	}
	if unresolved != nil {
		c.current = cloneRecordState(unresolved)
	} else {
		c.current = cloneRecordState(latest)
	}
	return nil
}

func isTransitionJournalEvent(eventType journal.EventType) bool {
	switch eventType {
	case journal.TransitionCreated, journal.TransitionPrepared, journal.AuthDeployed,
		journal.CommitSent, journal.IdentityConfirmed, journal.TransitionCommitted,
		journal.TransitionUncertain, journal.TransitionReconciled:
		return true
	default:
		return false
	}
}

func replayExpectedGeneration(event journal.Event) (uint64, error) {
	if event.ExpectedGeneration != 0 {
		if event.ExpectedGeneration <= event.AuthGeneration {
			return 0, fmt.Errorf("expected generation %d is not greater than source generation %d", event.ExpectedGeneration, event.AuthGeneration)
		}
		return event.ExpectedGeneration, nil
	}
	if event.AuthGeneration == ^uint64(0) {
		return 0, fmt.Errorf("source generation overflows target generation")
	}
	return event.AuthGeneration + 1, nil
}

func validateReplayCreated(record *transitionRecord, event journal.Event) error {
	if record.state.RuntimeID != event.RuntimeID || record.state.TargetAccountID != event.TargetAccountID || record.state.CurrentAccountID != event.FromAccountID {
		return fmt.Errorf("restore transition %s: duplicate TransitionCreated conflicts with existing intent", event.TransitionID)
	}
	return nil
}

func validateReplayCorrelation(record *transitionRecord, event journal.Event) error {
	if record.state.RuntimeID != event.RuntimeID {
		return fmt.Errorf("restore transition %s: event %s runtime_id %q conflicts with %q", event.TransitionID, event.Type, event.RuntimeID, record.state.RuntimeID)
	}
	if event.TargetAccountID != "" && event.TargetAccountID != record.state.TargetAccountID {
		return fmt.Errorf("restore transition %s: event %s target account %q conflicts with %q", event.TransitionID, event.Type, event.TargetAccountID, record.state.TargetAccountID)
	}
	return nil
}

func applyReplayEvent(record *transitionRecord, event journal.Event) {
	state := &record.state
	if event.At.After(state.UpdatedAt) {
		state.UpdatedAt = event.At
	}
	switch event.Type {
	case journal.TransitionPrepared:
		state.PrepareSent = true
		if state.Phase == PhaseIdle || state.Phase == PhasePrepared {
			state.Phase = PhasePrepared
		}
	case journal.AuthDeployed:
		state.DiskAccountID = firstNonEmpty(event.AccountID, event.TargetAccountID)
		if state.Phase != PhaseCommitted && state.Phase != PhaseRejected {
			state.Phase = PhaseDeploying
		}
	case journal.CommitSent:
		state.CommitSent = true
		state.CommitAttempts++
		state.Phase = PhaseCommitSent
		if attempt, err := parseCommitAttempt(event.Reason); err == nil && attempt > state.CommitAttempts {
			state.CommitAttempts = attempt
		}
	case journal.IdentityConfirmed:
		state.AdoptedAccountID = firstNonEmpty(event.AccountID, event.TargetAccountID)
		state.FinalGeneration = event.AuthGeneration
		// Identity confirmation is durable evidence that the runtime had
		// already verified the commit, but TransitionCommitted is a separate
		// append. Keep the record unresolved until that terminal marker exists.
		if state.Phase != PhaseCommitted && state.Phase != PhaseRejected {
			state.Phase = PhaseCommitSent
		}
	case journal.TransitionCommitted:
		state.AdoptedAccountID = firstNonEmpty(event.AccountID, event.TargetAccountID)
		state.DiskAccountID = firstNonEmpty(state.DiskAccountID, event.TargetAccountID)
		state.FinalGeneration = event.AuthGeneration
		state.Phase = PhaseCommitted
		state.CompletedAt = event.At
	case journal.TransitionUncertain:
		state.Phase = PhaseUncertain
		state.Reason = event.Reason
	case journal.TransitionReconciled:
		state.FinalGeneration = event.AuthGeneration
		state.AdoptedAccountID = firstNonEmpty(event.AccountID, event.TargetAccountID)
		if strings.EqualFold(event.Outcome, string(TransitionCommitted)) || event.Outcome == "committed" {
			state.Phase = PhaseCommitted
		} else if strings.EqualFold(event.Outcome, string(TransitionRejected)) || event.Outcome == "rejected" {
			state.Phase = PhaseRejected
		}
		if state.Phase == PhaseCommitted || state.Phase == PhaseRejected {
			state.CompletedAt = event.At
		}
		state.Reason = event.Reason
	}
}

func parseCommitAttempt(reason string) (uint32, error) {
	const prefix = "commit attempt "
	if !strings.HasPrefix(strings.ToLower(reason), prefix) {
		return 0, fmt.Errorf("commit attempt is not encoded")
	}
	n, err := strconv.ParseUint(strings.TrimSpace(reason[len(prefix):]), 10, 32)
	return uint32(n), err
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
