package transitions

import (
	"testing"
	"time"

	"codexmarathon/controller/internal/journal"
)

func TestRestoreRehydratesUncertainTransitionIntent(t *testing.T) {
	now := time.Unix(100, 0).UTC()
	coordinator := NewCoordinator(Config{})
	events := []journal.Event{
		{
			At: now, Type: journal.TransitionCreated, TransitionID: "tx-1",
			AuthGeneration: 7, ExpectedGeneration: 8, RuntimeID: "runtime-1",
			FromAccountID: "account-a", TargetAccountID: "account-b",
		},
		{
			At: now.Add(time.Second), Type: journal.TransitionPrepared, TransitionID: "tx-1",
			AuthGeneration: 7, RuntimeID: "runtime-1", TargetAccountID: "account-b",
		},
		{
			At: now.Add(2 * time.Second), Type: journal.AuthDeployed, TransitionID: "tx-1",
			AuthGeneration: 8, RuntimeID: "runtime-1", AccountID: "account-b",
			TargetAccountID: "account-b",
		},
		{
			At: now.Add(3 * time.Second), Type: journal.CommitSent, TransitionID: "tx-1",
			AuthGeneration: 8, RuntimeID: "runtime-1", TargetAccountID: "account-b",
			Reason: "commit attempt 1",
		},
		{
			At: now.Add(4 * time.Second), Type: journal.TransitionUncertain, TransitionID: "tx-1",
			AuthGeneration: 8, RuntimeID: "runtime-1", TargetAccountID: "account-b",
			Reason: "controller disconnected after commit",
		},
	}
	if err := coordinator.Restore(events); err != nil {
		t.Fatalf("Restore() error = %v", err)
	}
	state, ok := coordinator.Pending()
	if !ok {
		t.Fatal("Pending() = false, want restored unresolved transition")
	}
	if state.TransitionID != "tx-1" || state.TargetAccountID != "account-b" {
		t.Fatalf("restored target = %#v", state)
	}
	if state.ExpectedGeneration != 8 || state.CommitAttempts != 1 || state.Phase != PhaseUncertain {
		t.Fatalf("restored correlation = %#v", state)
	}
}

func TestRestoreIgnoresNonTransitionEventsAndReplaysTerminalHistory(t *testing.T) {
	now := time.Unix(200, 0).UTC()
	coordinator := NewCoordinator(Config{})
	events := []journal.Event{
		{At: now, Type: journal.RuntimeConnected, RuntimeID: "runtime-1"},
		{At: now.Add(time.Second), Type: journal.TransitionCreated, TransitionID: "tx-1", AuthGeneration: 0, RuntimeID: "runtime-1", TargetAccountID: "account-a"},
		{At: now.Add(2 * time.Second), Type: journal.TransitionCommitted, TransitionID: "tx-1", AuthGeneration: 1, RuntimeID: "runtime-1", AccountID: "account-a", TargetAccountID: "account-a", Outcome: "committed"},
	}
	if err := coordinator.Restore(events); err != nil {
		t.Fatalf("Restore() error = %v", err)
	}
	if _, ok := coordinator.Pending(); ok {
		t.Fatal("Pending() = true, want terminal committed history")
	}
	state, ok := coordinator.State()
	if !ok || state.Phase != PhaseCommitted || state.FinalGeneration != 1 {
		t.Fatalf("terminal state = %#v, %v", state, ok)
	}
}
