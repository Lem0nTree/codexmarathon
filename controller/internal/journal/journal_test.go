package journal

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestFileJournalAppendsDurablyAndReplaysInOrder(t *testing.T) {
	path := filepath.Join(t.TempDir(), "journal", "events.jsonl")
	j := NewFileJournal(path)
	firstAt := time.Date(2026, 9, 4, 12, 0, 0, 0, time.FixedZone("UTC+2", 2*60*60))
	j.SetClock(func() time.Time { return firstAt })
	events := []Event{
		{Type: TransitionCreated, TransitionID: "tx-1", AuthGeneration: 1, RuntimeID: "runtime-1", TargetAccountID: "acct-b"},
		{Type: AuthDeployed, TransitionID: "tx-1", AuthGeneration: 1, RuntimeID: "runtime-1", AccountID: "acct-b", Reason: "atomic deployment completed"},
		{Type: TransitionCommitted, TransitionID: "tx-1", AuthGeneration: 1, RuntimeID: "runtime-1", AccountID: "acct-b", Outcome: "committed"},
	}
	for _, event := range events {
		if err := j.Append(event); err != nil {
			t.Fatalf("Append(%s) error = %v", event.Type, err)
		}
	}
	got, err := j.ReadAll()
	if err != nil {
		t.Fatalf("ReadAll() error = %v", err)
	}
	if len(got) != len(events) {
		t.Fatalf("ReadAll() length = %d, want %d", len(got), len(events))
	}
	for i := range got {
		if got[i].Type != events[i].Type || got[i].TransitionID != events[i].TransitionID || !got[i].At.Equal(firstAt.UTC()) {
			t.Fatalf("event[%d] = %#v, want type/transition/time from %#v", i, got[i], events[i])
		}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "token") || strings.Contains(string(data), "auth.json") {
		t.Fatalf("journal contains forbidden credential markers: %s", data)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("journal permissions = %o, want owner-only", info.Mode().Perm())
	}
}

func TestFileJournalRejectsInvalidOrSensitiveEvents(t *testing.T) {
	j := NewFileJournal(filepath.Join(t.TempDir(), "events.jsonl"))
	base := Event{Type: TransitionCommitted, AuthGeneration: 1, RuntimeID: "runtime-1", TransitionID: "tx-1"}
	if err := j.Append(base); err != nil {
		t.Fatalf("Append(base) error = %v", err)
	}
	for _, event := range []Event{
		{Type: TransitionCommitted, RuntimeID: "runtime-1", TransitionID: "tx-2"},
		{Type: TransitionCommitted, AuthGeneration: 1, RuntimeID: "runtime-1", TransitionID: "tx-2", Reason: "refresh_token=do-not-log"},
		{Type: TransitionCommitted, AuthGeneration: 1, RuntimeID: "runtime-1", TransitionID: "tx-2", Reason: "line\nfeed"},
	} {
		if err := j.Append(event); err == nil {
			t.Fatalf("Append(%#v) error = nil, want validation error", event)
		}
	}
}

func TestFileJournalDetectsCorruptionAndKeepsAppendOnlyBytes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	j := NewFileJournal(path)
	if err := j.Append(Event{Type: RuntimeConnected, RuntimeID: "runtime-1"}); err != nil {
		t.Fatal(err)
	}
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString("not-json\n"); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := j.ReadAll(); !errors.Is(err, ErrCorruptJournal) {
		t.Fatalf("ReadAll(corrupt) error = %v, want ErrCorruptJournal", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(after), string(original)) {
		t.Fatal("journal bytes were rewritten instead of remaining append-only")
	}
}

func TestNewJournalAliasesFileJournal(t *testing.T) {
	j := NewJournal(filepath.Join(t.TempDir(), "events.jsonl"))
	if j == nil || j.Path() == "" {
		t.Fatalf("NewJournal() = %#v", j)
	}
	if _, err := j.Events(); err != nil {
		t.Fatalf("Events(empty) error = %v", err)
	}
}

func TestJournalPreservesInitialZeroGeneration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	j := NewFileJournal(path)
	if err := j.Append(Event{
		Type:           TransitionCreated,
		TransitionID:   "tx-initial",
		RuntimeID:      "runtime-1",
		AuthGeneration: 0,
	}); err != nil {
		t.Fatalf("Append(initial generation) error = %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"auth_generation":0`) {
		t.Fatalf("journal omitted zero generation: %s", data)
	}
}

func TestJournalAcceptsSecretFreeRecoveryLifecycle(t *testing.T) {
	j := NewFileJournal(filepath.Join(t.TempDir(), "events.jsonl"))
	events := []Event{
		{Type: RecoveryParked, RecoveryID: "recovery-1", RuntimeID: "runtime-1", ThreadID: "thread-1", TurnID: "turn-1", AccountID: "acct-a", AuthGeneration: 1, Reason: "runtime parked usage-limit recovery"},
		{Type: RecoveryBound, RecoveryID: "recovery-1", RuntimeID: "runtime-1", ThreadID: "thread-1", TransitionID: "tx-1", TargetAccountID: "acct-b", AuthGeneration: 2, ExpectedGeneration: 2},
		{Type: RecoveryReleaseRequested, RecoveryID: "recovery-1", RuntimeID: "runtime-1", ThreadID: "thread-1", TransitionID: "tx-1", TargetAccountID: "acct-b", AuthGeneration: 2, ExpectedGeneration: 2},
		{Type: RecoveryReleased, RecoveryID: "recovery-1", RuntimeID: "runtime-1", ThreadID: "thread-1", TransitionID: "tx-1", TargetAccountID: "acct-b", AuthGeneration: 2, ExpectedGeneration: 2, Outcome: "released"},
	}
	for _, event := range events {
		if err := j.Append(event); err != nil {
			t.Fatalf("Append(%s) error = %v", event.Type, err)
		}
	}
	got, err := j.ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(events) || got[2].RecoveryID != "recovery-1" || got[2].TransitionID != "tx-1" || got[2].ExpectedGeneration != 2 {
		t.Fatalf("replayed recovery events = %#v", got)
	}
	raw, err := os.ReadFile(j.Path())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "prompt") || strings.Contains(string(raw), "access_token") {
		t.Fatalf("recovery journal contains sensitive/prompt marker: %s", raw)
	}
}
