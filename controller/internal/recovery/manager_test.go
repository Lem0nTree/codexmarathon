package recovery

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"codexmarathon/controller/internal/journal"
	"codexmarathon/controller/internal/runtime"
)

type memoryJournal struct {
	mu     sync.Mutex
	events []journal.Event
}

func (j *memoryJournal) Append(event journal.Event) error {
	if event.At.IsZero() {
		event.At = time.Unix(1, 0).UTC()
	}
	if err := journal.ValidateEvent(event); err != nil {
		return err
	}
	j.mu.Lock()
	j.events = append(j.events, event)
	j.mu.Unlock()
	return nil
}

func (j *memoryJournal) ReadAll() ([]journal.Event, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	return append([]journal.Event(nil), j.events...), nil
}

type fakeRuntime struct {
	mu             sync.Mutex
	identity       runtime.Identity
	recoveries     []runtime.RecoveryState
	releaseCalls   []runtime.RecoveryReleaseParams
	release        runtime.RecoveryReleaseResult
	releaseErr     error
	applyBeforeErr bool
}

func (f *fakeRuntime) GetRuntimeState(context.Context) (runtime.RuntimeState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return runtime.RuntimeState{
		Identity:          f.identity,
		Recoveries:        append([]runtime.RecoveryState(nil), f.recoveries...),
		PendingTransition: nil,
	}, nil
}

func (f *fakeRuntime) ReleaseRecovery(_ context.Context, params runtime.RecoveryReleaseParams) (runtime.RecoveryReleaseResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.releaseCalls = append(f.releaseCalls, params)
	if f.applyBeforeErr {
		f.recoveries = []runtime.RecoveryState{{
			RecoveryID:         params.RecoveryID,
			ThreadID:           params.ThreadID,
			TransitionID:       params.TransitionID,
			TargetAccountID:    "acct-b",
			ExpectedGeneration: params.ExpectedGeneration,
			Phase:              runtime.RecoveryReleasedPhase,
		}}
	}
	if f.releaseErr != nil {
		return runtime.RecoveryReleaseResult{}, f.releaseErr
	}
	result := f.release
	if result.RecoveryID == "" {
		result.RecoveryID = params.RecoveryID
	}
	if result.ThreadID == "" {
		result.ThreadID = params.ThreadID
	}
	return result, nil
}

func (f *fakeRuntime) calls() []runtime.RecoveryReleaseParams {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]runtime.RecoveryReleaseParams(nil), f.releaseCalls...)
}

func parkedEvent(t *testing.T, id, thread, turn, source, runtimeID string, generation uint64) runtime.Event {
	t.Helper()
	data, err := json.Marshal(runtime.RecoveryParkedEvent{
		RecoveryID:      id,
		ThreadID:        thread,
		TurnID:          turn,
		SourceAccountID: source,
	})
	if err != nil {
		t.Fatal(err)
	}
	return runtime.Event{
		EventBase: runtime.EventBase{
			EventType:      runtime.EventRecoveryParked,
			OccurredAt:     1,
			RuntimeID:      runtimeID,
			AuthGeneration: generation,
		},
		Payload: data,
	}
}

func startedEvent(t *testing.T, id, thread, turn, runtimeID string, generation uint64) runtime.Event {
	t.Helper()
	data, err := json.Marshal(runtime.RecoveryStartedEvent{RecoveryID: id, ThreadID: thread, TurnID: turn})
	if err != nil {
		t.Fatal(err)
	}
	return runtime.Event{EventBase: runtime.EventBase{
		EventType: runtime.EventRecoveryStarted, OccurredAt: 2, RuntimeID: runtimeID, AuthGeneration: generation,
	}, Payload: data}
}

func newTestManager(t *testing.T, rt Runtime) (*Manager, *memoryJournal) {
	t.Helper()
	j := &memoryJournal{}
	m, err := New(Config{Journal: j, Runtime: rt, Now: func() time.Time { return time.Unix(100, 0).UTC() }})
	if err != nil {
		t.Fatal(err)
	}
	return m, j
}

func bindForTest(t *testing.T, m *Manager) {
	t.Helper()
	if err := m.Bind(Binding{
		RecoveryID: "rec-1", RuntimeID: "runtime-a", ThreadID: "thread-1", TurnID: "turn-1",
		TransitionID: "tx-1", TargetAccountID: "acct-b", ExpectedGeneration: 2,
	}); err != nil {
		t.Fatal(err)
	}
}

func commitForTest() TransitionCommit {
	return TransitionCommit{
		TransitionID: "tx-1", RuntimeID: "runtime-a", SourceAccountID: "acct-a", TargetAccountID: "acct-b",
		ExpectedGeneration: 2, FinalGeneration: 2, Outcome: "committed",
	}
}

func TestManagerResumesSameThreadExactlyOnceAndPreservesQueueOwner(t *testing.T) {
	rt := &fakeRuntime{
		identity: runtime.Identity{RuntimeID: "runtime-a", AuthGeneration: 2, AccountID: stringPtr("acct-b")},
		release:  runtime.RecoveryReleaseResult{Outcome: runtime.RecoveryReleased},
	}
	m, journalLog := newTestManager(t, rt)
	if err := m.HandleEvent(parkedEvent(t, "rec-1", "thread-1", "turn-1", "acct-a", "runtime-a", 1)); err != nil {
		t.Fatal(err)
	}
	bindForTest(t, m)
	if err := m.OnTransitionCommitted(context.Background(), commitForTest()); err != nil {
		t.Fatal(err)
	}
	if err := m.OnTransitionCommitted(context.Background(), commitForTest()); err != nil {
		t.Fatal(err)
	}
	state, ok := m.State("rec-1")
	if !ok || state.Phase != PhaseReleased {
		t.Fatalf("state = %#v, ok=%v, want released", state, ok)
	}
	calls := rt.calls()
	if len(calls) != 1 {
		t.Fatalf("release calls = %d, want exactly one", len(calls))
	}
	if calls[0].ThreadID != "thread-1" || calls[0].RecoveryID != "rec-1" {
		t.Fatalf("release params = %#v, want same thread/recovery", calls[0])
	}
	// Runtime-owned input ordering is represented by started after release;
	// the controller never submits a second user message or synthetic prompt.
	if err := m.HandleEvent(startedEvent(t, "rec-1", "thread-1", "turn-2", "runtime-a", 2)); err != nil {
		t.Fatal(err)
	}
	state, _ = m.State("rec-1")
	if state.Phase != PhaseStarted {
		t.Fatalf("post-start phase = %s, want started", state.Phase)
	}
	for _, event := range journalLog.events {
		if event.Reason != "" && (event.Reason == "resume" || event.Reason == "prompt") {
			t.Fatalf("journal contains a synthetic prompt reason: %#v", event)
		}
	}
}

func TestManagerKeepsRecoveryParkedWhilePoolExhausted(t *testing.T) {
	rt := &fakeRuntime{
		identity: runtime.Identity{RuntimeID: "runtime-a", AuthGeneration: 1, AccountID: stringPtr("acct-a")},
		release:  runtime.RecoveryReleaseResult{Outcome: runtime.RecoveryReleased},
	}
	m, _ := newTestManager(t, rt)
	if err := m.HandleEvent(parkedEvent(t, "rec-1", "thread-1", "turn-1", "acct-a", "runtime-a", 1)); err != nil {
		t.Fatal(err)
	}
	if err := m.MarkWaiting("rec-1", "all accounts exhausted until earliest reset"); err != nil {
		t.Fatal(err)
	}
	if calls := rt.calls(); len(calls) != 0 {
		t.Fatalf("release calls while waiting = %d, want 0", len(calls))
	}
	state, _ := m.State("rec-1")
	if state.Phase != PhaseWaiting {
		t.Fatalf("waiting phase = %s, want %s", state.Phase, PhaseWaiting)
	}
	bindForTest(t, m)
	if err := m.OnTransitionCommitted(context.Background(), commitForTest()); err != nil {
		t.Fatal(err)
	}
	if calls := rt.calls(); len(calls) != 1 {
		t.Fatalf("release calls after verified switch = %d, want 1", len(calls))
	}
}

func TestManagerReconcilesLostReleaseAcknowledgementWithoutDuplicate(t *testing.T) {
	rt := &fakeRuntime{
		identity:       runtime.Identity{RuntimeID: "runtime-a", AuthGeneration: 2, AccountID: stringPtr("acct-b")},
		applyBeforeErr: true,
		releaseErr:     errors.New("ack lost after native release"),
	}
	m, _ := newTestManager(t, rt)
	if err := m.HandleEvent(parkedEvent(t, "rec-1", "thread-1", "turn-1", "acct-a", "runtime-a", 1)); err != nil {
		t.Fatal(err)
	}
	bindForTest(t, m)
	if err := m.OnTransitionCommitted(context.Background(), commitForTest()); err == nil {
		t.Fatal("OnTransitionCommitted() error = nil, want lost acknowledgement")
	}
	state, _ := m.State("rec-1")
	if state.Phase != PhaseReleaseRequested {
		t.Fatalf("lost-ack phase = %s, want release_requested", state.Phase)
	}
	// The runtime state says it applied the release. Reconcile adopts that
	// result instead of calling the native queue a second time.
	if err := m.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	state, _ = m.State("rec-1")
	if state.Phase != PhaseReleased {
		t.Fatalf("reconciled phase = %s, want released", state.Phase)
	}
	if calls := rt.calls(); len(calls) != 1 {
		t.Fatalf("release calls after lost ack = %d, want 1", len(calls))
	}
}

func TestManagerReplaysReleaseIntentAcrossControllerCrash(t *testing.T) {
	j := &memoryJournal{}
	first, err := New(Config{Journal: j, Now: func() time.Time { return time.Unix(100, 0).UTC() }})
	if err != nil {
		t.Fatal(err)
	}
	if err := first.HandleEvent(parkedEvent(t, "rec-1", "thread-1", "turn-1", "acct-a", "runtime-a", 1)); err != nil {
		t.Fatal(err)
	}
	bindForTest(t, first)
	if err := first.OnTransitionCommitted(context.Background(), commitForTest()); !errors.Is(err, ErrRuntimeUnavailable) {
		t.Fatalf("detached commit error = %v, want ErrRuntimeUnavailable", err)
	}
	state, _ := first.State("rec-1")
	if state.Phase != PhaseReleaseRequested {
		t.Fatalf("pre-crash phase = %s, want release_requested", state.Phase)
	}
	secondRuntime := &fakeRuntime{
		identity: runtime.Identity{RuntimeID: "runtime-after-restart", AuthGeneration: 2, AccountID: stringPtr("acct-b")},
		recoveries: []runtime.RecoveryState{{
			RecoveryID: "rec-1", ThreadID: "thread-1", TransitionID: "tx-1", TargetAccountID: "acct-b",
			ExpectedGeneration: 2, Phase: runtime.RecoveryParkedPhase,
		}},
		release: runtime.RecoveryReleaseResult{Outcome: runtime.RecoveryAlreadyReleased},
	}
	second, err := New(Config{Journal: j, Runtime: secondRuntime, Now: func() time.Time { return time.Unix(200, 0).UTC() }})
	if err != nil {
		t.Fatal(err)
	}
	if err := second.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	state, _ = second.State("rec-1")
	if state.Phase != PhaseReleased {
		t.Fatalf("post-crash phase = %s, want released", state.Phase)
	}
	if calls := secondRuntime.calls(); len(calls) != 1 {
		t.Fatalf("post-crash release calls = %d, want 1", len(calls))
	}
	if calls := secondRuntime.calls(); calls[0].ExpectedGeneration != 2 || calls[0].TransitionID != "tx-1" {
		t.Fatalf("post-crash params = %#v, want persisted transition metadata", calls[0])
	}
}

func TestManagerRejectsMismatchedThreadBeforeRelease(t *testing.T) {
	rt := &fakeRuntime{
		identity: runtime.Identity{RuntimeID: "runtime-a", AuthGeneration: 2, AccountID: stringPtr("acct-b")},
		release:  runtime.RecoveryReleaseResult{Outcome: runtime.RecoveryReleased},
	}
	m, _ := newTestManager(t, rt)
	if err := m.HandleEvent(parkedEvent(t, "rec-1", "thread-1", "turn-1", "acct-a", "runtime-a", 1)); err != nil {
		t.Fatal(err)
	}
	if err := m.Bind(Binding{RecoveryID: "rec-1", RuntimeID: "runtime-a", ThreadID: "thread-2", TransitionID: "tx-1", TargetAccountID: "acct-b", ExpectedGeneration: 2}); !errors.Is(err, ErrRecoveryConflict) {
		t.Fatalf("Bind() error = %v, want ErrRecoveryConflict", err)
	}
	if calls := rt.calls(); len(calls) != 0 {
		t.Fatalf("release calls after mismatch = %d, want 0", len(calls))
	}
}

func stringPtr(value string) *string { return &value }
