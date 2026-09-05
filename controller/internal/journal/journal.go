// Package journal provides the controller's append-only recovery journal.
//
// Journal records contain transition/runtime metadata only. Credential bytes,
// auth.json contents, authorization headers, and recovery prompt text are not
// part of Event and therefore cannot be serialized accidentally through the
// normal API.
package journal

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

var (
	ErrInvalidEvent   = errors.New("invalid journal event")
	ErrCorruptJournal = errors.New("corrupt journal")
	ErrSensitiveEvent = errors.New("journal event contains sensitive material")
)

// EventType is intentionally open to forward-compatible runtime events while
// the constants below document the MVP recovery lifecycle.
type EventType string

const (
	TransitionCreated    EventType = "TransitionCreated"
	TransitionPrepared   EventType = "TransitionPrepared"
	AuthDeployed         EventType = "AuthDeployed"
	CommitSent           EventType = "CommitSent"
	IdentityConfirmed    EventType = "IdentityConfirmed"
	TransitionCommitted  EventType = "TransitionCommitted"
	TransitionUncertain  EventType = "TransitionUncertain"
	TransitionReconciled EventType = "TransitionReconciled"
	RuntimeConnected     EventType = "RuntimeConnected"
	RuntimeDisconnected  EventType = "RuntimeDisconnected"
	// Recovery records describe the controller's durable authorization of a
	// runtime-owned UsageLimitExceeded continuation.  They contain only
	// correlation metadata; the prompt and credentials remain in Codext.
	RecoveryParked           EventType = "RecoveryParked"
	RecoveryWaiting          EventType = "RecoveryWaiting"
	RecoveryBound            EventType = "RecoveryBound"
	RecoveryReleaseRequested EventType = "RecoveryReleaseRequested"
	RecoveryReleased         EventType = "RecoveryReleased"
	RecoveryStarted          EventType = "RecoveryStarted"
	RecoveryCompleted        EventType = "RecoveryCompleted"
	RecoveryUncertain        EventType = "RecoveryUncertain"

	// Event-prefixed aliases make call sites self-documenting when journal and
	// runtime event constants are imported together.
	EventTransitionCreated        = TransitionCreated
	EventTransitionPrepared       = TransitionPrepared
	EventAuthDeployed             = AuthDeployed
	EventCommitSent               = CommitSent
	EventIdentityConfirmed        = IdentityConfirmed
	EventTransitionCommitted      = TransitionCommitted
	EventTransitionUncertain      = TransitionUncertain
	EventTransitionReconciled     = TransitionReconciled
	EventRuntimeConnected         = RuntimeConnected
	EventRuntimeDisconnected      = RuntimeDisconnected
	EventRecoveryParked           = RecoveryParked
	EventRecoveryWaiting          = RecoveryWaiting
	EventRecoveryBound            = RecoveryBound
	EventRecoveryReleaseRequested = RecoveryReleaseRequested
	EventRecoveryReleased         = RecoveryReleased
	EventRecoveryStarted          = RecoveryStarted
	EventRecoveryCompleted        = RecoveryCompleted
	EventRecoveryUncertain        = RecoveryUncertain
)

// Event is the complete journal wire record. The fields are deliberately
// scalar and non-secret; use Reason for a short operator-safe explanation, not
// raw provider responses or token material.
type Event struct {
	At           time.Time `json:"at"`
	Type         EventType `json:"type"`
	TransitionID string    `json:"transition_id,omitempty"`
	RecoveryID   string    `json:"recovery_id,omitempty"`
	ThreadID     string    `json:"thread_id,omitempty"`
	TurnID       string    `json:"turn_id,omitempty"`
	// Generation zero is a valid initial runtime generation. Keep the field on
	// the JSON record even when it is zero so every transition record carries
	// the correlation field required by the protocol contract.
	AuthGeneration     uint64 `json:"auth_generation"`
	ExpectedGeneration uint64 `json:"expected_generation,omitempty"`
	RuntimeID          string `json:"runtime_id,omitempty"`
	AccountID          string `json:"account_id,omitempty"`
	FromAccountID      string `json:"from_account_id,omitempty"`
	TargetAccountID    string `json:"target_account_id,omitempty"`
	Outcome            string `json:"outcome,omitempty"`
	Reason             string `json:"reason,omitempty"`
}

// Journal is the persistence seam consumed by transition orchestration.
type Journal interface {
	Append(event Event) error
	ReadAll() ([]Event, error)
}

// FileJournal is a process-safe JSONL journal. O_APPEND plus a process mutex
// keeps each record whole for concurrent callers in this process. Every append
// is synced before returning so a successful call is durable to the OS.
type FileJournal struct {
	path string
	mu   sync.Mutex
	now  func() time.Time
}

// NewFileJournal constructs a journal without creating its file.
func NewFileJournal(path string) *FileJournal {
	return &FileJournal{path: path, now: time.Now}
}

// NewJournal is the concise constructor used by controller wiring.
func NewJournal(path string) *FileJournal {
	return NewFileJournal(path)
}

// Path returns the journal path without exposing its records.
func (j *FileJournal) Path() string {
	if j == nil {
		return ""
	}
	return j.path
}

// SetClock injects a clock for deterministic tests. Event.At values supplied
// by callers remain authoritative; the clock fills only zero timestamps.
func (j *FileJournal) SetClock(now func() time.Time) {
	if j == nil {
		return
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if now == nil {
		j.now = time.Now
		return
	}
	j.now = now
}

// Append validates and durably appends one event. It never rewrites or prunes
// existing records.
func (j *FileJournal) Append(event Event) error {
	if j == nil {
		return errors.New("nil journal")
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if event.At.IsZero() {
		now := j.now
		if now == nil {
			now = time.Now
		}
		event.At = now()
	}
	event.At = event.At.UTC()
	if err := ValidateEvent(event); err != nil {
		return err
	}
	if j.path == "" {
		return errors.New("journal path is empty")
	}
	if err := rejectJournalSymlink(j.path); err != nil {
		return err
	}
	dir := filepath.Dir(j.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(dir, 0o700); err != nil && !isPermissionMetadataError(err) {
		return err
	}
	raw, err := json.Marshal(event)
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	file, err := os.OpenFile(j.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer file.Close()
	if err := file.Chmod(0o600); err != nil && !isPermissionMetadataError(err) {
		return err
	}
	if _, err := file.Write(raw); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	return nil
}

// AppendEvent is a compatibility spelling for Append.
func (j *FileJournal) AppendEvent(event Event) error {
	return j.Append(event)
}

// ReadAll reads and validates every JSONL record. A malformed line is an
// integrity error; silently skipping it would make recovery state ambiguous.
func (j *FileJournal) ReadAll() ([]Event, error) {
	if j == nil {
		return nil, errors.New("nil journal")
	}
	if j.path == "" {
		return nil, errors.New("journal path is empty")
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if err := rejectJournalSymlink(j.path); err != nil {
		return nil, err
	}
	file, err := os.Open(j.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return []Event{}, nil
		}
		return nil, err
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	// A normal event is small, but permit a bounded diagnostic reason without
	// allowing an unbounded scanner allocation.
	scanner.Buffer(make([]byte, 4096), 1024*1024)
	events := make([]Event, 0)
	line := 0
	for scanner.Scan() {
		line++
		data := bytes.TrimSpace(scanner.Bytes())
		if len(data) == 0 {
			continue
		}
		var event Event
		if err := json.Unmarshal(data, &event); err != nil {
			return nil, fmt.Errorf("%w: line %d: %v", ErrCorruptJournal, line, err)
		}
		if err := ValidateEvent(event); err != nil {
			return nil, fmt.Errorf("%w: line %d: %v", ErrCorruptJournal, line, err)
		}
		events = append(events, event)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("%w: line %d: %v", ErrCorruptJournal, line+1, err)
	}
	return events, nil
}

// Events is an alias for ReadAll.
func (j *FileJournal) Events() ([]Event, error) {
	return j.ReadAll()
}

// ValidateEvent enforces the scalar event contract and rejects obvious secret
// field patterns in operator-provided text. Raw credential values cannot be
// represented as structured fields in Event.
func ValidateEvent(event Event) error {
	if strings.TrimSpace(string(event.Type)) == "" {
		return ErrInvalidEvent
	}
	if len(event.Type) > 256 {
		return fmt.Errorf("%w: event type is too long", ErrInvalidEvent)
	}
	if event.At.IsZero() {
		return fmt.Errorf("%w: timestamp is required", ErrInvalidEvent)
	}
	if isTransitionEvent(event.Type) {
		if event.TransitionID == "" {
			return fmt.Errorf("%w: transition_id is required for %s", ErrInvalidEvent, event.Type)
		}
		if event.RuntimeID == "" {
			return fmt.Errorf("%w: runtime_id is required for %s", ErrInvalidEvent, event.Type)
		}
		if (event.Type == TransitionCommitted || event.Type == TransitionReconciled) && event.AuthGeneration == 0 {
			return fmt.Errorf("%w: auth_generation is required for %s", ErrInvalidEvent, event.Type)
		}
	}
	if isRecoveryEvent(event.Type) {
		if event.RecoveryID == "" {
			return fmt.Errorf("%w: recovery_id is required for %s", ErrInvalidEvent, event.Type)
		}
		if event.RuntimeID == "" {
			return fmt.Errorf("%w: runtime_id is required for %s", ErrInvalidEvent, event.Type)
		}
	}
	for name, value := range map[string]string{
		"transition_id":     event.TransitionID,
		"recovery_id":       event.RecoveryID,
		"thread_id":         event.ThreadID,
		"turn_id":           event.TurnID,
		"runtime_id":        event.RuntimeID,
		"account_id":        event.AccountID,
		"from_account_id":   event.FromAccountID,
		"target_account_id": event.TargetAccountID,
		"outcome":           event.Outcome,
		"reason":            event.Reason,
	} {
		if err := validateText(name, value); err != nil {
			return err
		}
	}
	return nil
}

func isTransitionEvent(eventType EventType) bool {
	switch eventType {
	case TransitionCreated, TransitionPrepared, AuthDeployed, CommitSent,
		IdentityConfirmed, TransitionCommitted, TransitionUncertain,
		TransitionReconciled:
		return true
	default:
		return false
	}
}

func isRecoveryEvent(eventType EventType) bool {
	switch eventType {
	case RecoveryParked, RecoveryWaiting, RecoveryBound,
		RecoveryReleaseRequested, RecoveryReleased, RecoveryStarted,
		RecoveryCompleted, RecoveryUncertain:
		return true
	default:
		return false
	}
}

func validateText(field, value string) error {
	if strings.ContainsRune(value, 0) || strings.ContainsAny(value, "\r\n") {
		return fmt.Errorf("%w: %s contains control characters", ErrInvalidEvent, field)
	}
	if len(value) > 4096 {
		return fmt.Errorf("%w: %s is too long", ErrInvalidEvent, field)
	}
	if field == "reason" && looksSensitive(value) {
		return fmt.Errorf("%w: %s", ErrSensitiveEvent, field)
	}
	return nil
}

func looksSensitive(value string) bool {
	lower := strings.ToLower(value)
	for _, marker := range []string{
		"access_token", "id_token", "refresh_token", "authorization:",
		"bearer ", "api_key", "openai_api_key", "password=", "secret=",
		"auth.json", "\"tokens\"",
	} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

func isPermissionMetadataError(error) bool {
	return false
}

func rejectJournalSymlink(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%w: journal path is a symbolic link", ErrInvalidEvent)
	}
	if info.IsDir() {
		return fmt.Errorf("%w: journal path is a directory", ErrInvalidEvent)
	}
	return nil
}
