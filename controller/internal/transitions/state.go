// Package transitions coordinates controller-owned credential deployment with
// the runtime's safe-boundary and identity APIs.
//
// The package deliberately keeps the runtime and disk as separate authorities:
// a successful filesystem write is not a successful runtime transition, and a
// runtime acknowledgement that cannot be correlated is not silently treated
// as a failure.  The resulting explicit uncertain state is what lets callers
// reconcile after an IPC or process failure without starting a second,
// competing transition.
package transitions

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"codexmarathon/controller/internal/credentials"
	"codexmarathon/controller/internal/journal"
	"codexmarathon/controller/internal/runtime"
)

// TransitionOutcome is intentionally a string so it can be logged and
// compared with runtime.TransitionOutcome without an ordinal conversion.
// Uncertain is a first-class outcome: it means the controller cannot prove
// whether a state-changing runtime command was applied.
type TransitionOutcome string

const (
	TransitionCommitted TransitionOutcome = "committed"
	TransitionRejected  TransitionOutcome = "rejected"
	TransitionUncertain TransitionOutcome = "uncertain"
)

// Outcome is a descriptive alias for integrations that use the shorter name.
type Outcome = TransitionOutcome

// Result is the coordinator's durable, controller-facing result.  RuntimeID,
// DiskAccountID, and Reason are diagnostic evidence; no credential material is
// representable here.
type TransitionResult struct {
	TransitionID       string            `json:"transition_id"`
	TargetAccountID    string            `json:"target_account_id"`
	AdoptedAccountID   string            `json:"adopted_account_id,omitempty"`
	ExpectedGeneration uint64            `json:"expected_generation"`
	FinalGeneration    uint64            `json:"final_generation"`
	Outcome            TransitionOutcome `json:"outcome"`
	CompletedAt        time.Time         `json:"completed_at,omitempty"`
	RuntimeID          string            `json:"runtime_id,omitempty"`
	DiskAccountID      string            `json:"disk_account_id,omitempty"`
	Reason             string            `json:"reason,omitempty"`
}

// Result is retained as a convenient compatibility spelling.
type Result = TransitionResult

// TransitionPhase is the controller's local lifecycle state.  It is not a
// substitute for runtime state; it records what the controller has sent or
// durably observed so reconciliation can resume after a restart.
type TransitionPhase string

const (
	PhaseIdle              TransitionPhase = "idle"
	PhasePrepared          TransitionPhase = "prepared"
	PhaseWaitingForBoundary TransitionPhase = "waiting_for_boundary"
	PhaseDeploying         TransitionPhase = "deploying"
	PhaseCommitSent        TransitionPhase = "commit_sent"
	PhaseUncertain         TransitionPhase = "uncertain"
	PhaseCommitted         TransitionPhase = "committed"
	PhaseRejected          TransitionPhase = "rejected"
)

// State is a snapshot of one controller-owned transition.  Copies returned by
// the coordinator are independent and can be retained by callers.
type State struct {
	TransitionID       string          `json:"transition_id"`
	RuntimeID          string          `json:"runtime_id"`
	CurrentAccountID   string          `json:"current_account_id,omitempty"`
	TargetAccountID    string          `json:"target_account_id"`
	DiskAccountID      string          `json:"disk_account_id,omitempty"`
	AdoptedAccountID   string          `json:"adopted_account_id,omitempty"`
	ExpectedGeneration uint64          `json:"expected_generation"`
	FinalGeneration    uint64          `json:"final_generation,omitempty"`
	Phase              TransitionPhase `json:"phase"`
	PrepareSent        bool            `json:"prepare_sent"`
	CommitSent         bool            `json:"commit_sent"`
	CommitAttempts     uint32          `json:"commit_attempts"`
	ActiveTurnCount    int             `json:"active_turn_count"`
	CreatedAt          time.Time       `json:"created_at"`
	UpdatedAt          time.Time       `json:"updated_at"`
	CompletedAt        time.Time       `json:"completed_at,omitempty"`
	Reason             string          `json:"reason,omitempty"`
}

// TransitionState is a descriptive alias for State.
type TransitionState = State

// Runtime is the small runtime surface used by the coordinator.  Events are
// optional (see RuntimeEvents); keeping them out of this required interface
// allows deterministic fakes and reconnecting transports to implement only the
// request side.
type Runtime interface {
	GetRuntimeState(context.Context) (runtime.RuntimeState, error)
	PrepareAuthTransition(context.Context, runtime.AuthTransitionParams) (runtime.TransitionResult, error)
	CommitAuthTransition(context.Context, runtime.AuthTransitionParams) (runtime.TransitionResult, error)
	CancelAuthTransition(context.Context, runtime.CancelAuthTransitionParams) (runtime.TransitionResult, error)
	GetIdentity(context.Context) (runtime.Identity, error)
	GetAuthGeneration(context.Context) (runtime.AuthGenerationResult, error)
}

// RuntimeEvents is implemented by runtime.Client and the fake runtime.  The
// coordinator consumes this channel only while it is waiting for a matching
// safe-boundary event; callers may instead pass events to HandleEvent.
type RuntimeEvents interface {
	Events() <-chan runtime.Event
}

// TransitionCoordinator is the public orchestration seam consumed by policy
// code. IDs and generations are intentionally absent from RequestTransition:
// they are coordinator-owned values.
type TransitionCoordinator interface {
	RequestTransition(context.Context, string) (*TransitionResult, error)
	Reconcile(context.Context, string) (*TransitionResult, error)
}

// DiskIdentityReader supplies the identity observed in the deployed auth file.
// It must return an empty string when the file exists but contains no account
// identity; that is evidence of an unauthenticated file, not an inferred
// target account.
type DiskIdentityReader interface {
	ReadIdentity() (string, error)
}

// CredentialDeployer is repeated here as a descriptive seam so callers do
// not need to import credentials merely to construct a coordinator.
type CredentialDeployer interface {
	Deploy(accountID string) (credentials.DeploymentResult, error)
}

// RuntimeAuthSnapshotReader exposes the native runtime's current opaque auth
// snapshot. It is optional for compatibility with small runtimes and fakes,
// but production transitions should implement it so refreshed Account A
// credentials are synchronized before Account B is deployed.
type RuntimeAuthSnapshotReader interface {
	ReadAuthSnapshot(context.Context) (runtime.NativeAuthSnapshotResult, error)
}

// Clock is injectable for deterministic transition and journal tests.
type Clock func() time.Time

// Config constructs a Coordinator.  Runtime and Deployer are required;
// Journal, Disk, and the remaining fields are optional.
type Config struct {
	Runtime  Runtime
	Deployer CredentialDeployer
	// SnapshotWriter receives the active account's opaque native snapshot
	// before target deployment. It must be the same protected vault used by
	// Deployer; no token material enters transition state or the journal.
	SnapshotWriter credentials.SnapshotWriter

	// Disk is the preferred spelling. DiskIdentity is accepted as a
	// compatibility alias when wiring older callers.
	Disk         DiskIdentityReader
	DiskIdentity DiskIdentityReader
	Journal      journal.Journal

	Now                  Clock
	BoundaryPollInterval time.Duration
	TransitionID         func() string
}

// Coordinator is safe for a policy loop and an event/reconciliation loop to
// call concurrently.  Network, runtime, deployer, and journal calls happen
// outside mu; local state remains serialized by mu.
type Coordinator struct {
	runtime  Runtime
	deployer CredentialDeployer
	snapshotWriter credentials.SnapshotWriter
	disk     DiskIdentityReader
	journal  journal.Journal
	now      Clock
	boundaryPollInterval time.Duration
	idGenerator         func() string

	mu          sync.Mutex
	current     *State
	transitions map[string]*transitionRecord
	boundary   map[string]chan struct{}
	sequence   atomic.Uint64
}

type transitionRecord struct {
	state       State
	oldIdentity runtime.Identity
	params      runtime.AuthTransitionParams
}

var (
	ErrNilCoordinator       = errors.New("nil transition coordinator")
	ErrNilRuntime           = errors.New("transition runtime is nil")
	ErrNilDeployer          = errors.New("transition deployer is nil")
	ErrTransitionInProgress = errors.New("transition already in progress")
	ErrTransitionNotFound   = errors.New("transition not found")
	ErrTransitionIDMismatch = errors.New("transition id mismatch")
	ErrRuntimeIDMismatch    = errors.New("runtime id mismatch")
	ErrGenerationMismatch   = errors.New("auth generation mismatch")
	ErrIdentityMismatch     = errors.New("runtime identity mismatch")
	ErrSafeBoundaryPending  = errors.New("safe boundary is pending")
	ErrSplitBrain           = errors.New("disk and runtime identities disagree")
	ErrReconciliationNeeded = errors.New("transition reconciliation is required")
)

// NewCoordinator constructs a coordinator from a Config.  For compatibility
// with small callers it also accepts the equivalent positional form
// NewCoordinator(runtime, deployer, journal...).  It returns a usable object
// even when required dependencies are nil; operations then return a stable
// error rather than panicking.
func NewCoordinator(value any, args ...any) *Coordinator {
	config := Config{}
	switch typed := value.(type) {
	case Config:
		config = typed
	case Runtime:
		config.Runtime = typed
		for _, arg := range args {
			if arg == nil {
				continue
			}
			if config.Deployer == nil {
				if deployer, ok := arg.(CredentialDeployer); ok {
					config.Deployer = deployer
					continue
				}
			}
			if config.Journal == nil {
				if record, ok := arg.(journal.Journal); ok {
					config.Journal = record
					continue
				}
			}
			if config.Disk == nil {
				if diskReader, ok := arg.(DiskIdentityReader); ok {
					config.Disk = diskReader
				}
			}
		}
	default:
		// Leave dependencies nil.  This preserves a deterministic validation
		// error for a malformed constructor call rather than panicking.
	}
	disk := config.Disk
	if disk == nil {
		disk = config.DiskIdentity
	}
	now := config.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	interval := config.BoundaryPollInterval
	if interval <= 0 {
		interval = 100 * time.Millisecond
	}
	c := &Coordinator{
		runtime:              config.Runtime,
		deployer:             config.Deployer,
		snapshotWriter:       config.SnapshotWriter,
		disk:                 disk,
		journal:              config.Journal,
		now:                  now,
		boundaryPollInterval: interval,
		transitions:          make(map[string]*transitionRecord),
		boundary:             make(map[string]chan struct{}),
	}
	if config.TransitionID != nil {
		c.idGenerator = config.TransitionID
	} else {
		c.idGenerator = func() string {
			return fmt.Sprintf("tx-%d", c.sequence.Add(1))
		}
	}
	return c
}

// NewTransitionCoordinator is the explicit constructor spelling.
func NewTransitionCoordinator(config Config) *Coordinator { return NewCoordinator(config) }

// NewCoordinatorWithDeps is a compact constructor for simple wiring.  The
// optional journal keeps this source-compatible with callers that do not need
// durable records in unit tests.
func NewCoordinatorWithDeps(rt Runtime, deployer CredentialDeployer, journals ...journal.Journal) *Coordinator {
	var record journal.Journal
	if len(journals) > 0 {
		record = journals[0]
	}
	return NewCoordinator(Config{Runtime: rt, Deployer: deployer, Journal: record})
}

// NewTransitionCoordinatorWithDeps is an alias for NewCoordinatorWithDeps.
func NewTransitionCoordinatorWithDeps(rt Runtime, deployer CredentialDeployer, journals ...journal.Journal) *Coordinator {
	return NewCoordinatorWithDeps(rt, deployer, journals...)
}

// State returns the active transition snapshot.  The bool is false when there
// is no active or unresolved transition.
func (c *Coordinator) State() (State, bool) {
	if c == nil {
		return State{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.current == nil {
		return State{}, false
	}
	return c.current.state, true
}

// CurrentState is a descriptive alias for State.
func (c *Coordinator) CurrentState() (State, bool) { return c.State() }

// Pending reports whether a transition blocks a new request.  Terminal
// committed/rejected records do not block subsequent requests.
func (c *Coordinator) Pending() (State, bool) {
	state, ok := c.State()
	if !ok || state.Phase == PhaseCommitted || state.Phase == PhaseRejected || state.Phase == PhaseIdle {
		return State{}, false
	}
	return state, true
}

// HasUnresolved reports whether reconciliation is required before a new
// transition may begin.
func (c *Coordinator) HasUnresolved() bool {
	state, ok := c.Pending()
	return ok && state.Phase == PhaseUncertain
}

func (c *Coordinator) validateDependencies() error {
	if c == nil {
		return ErrNilCoordinator
	}
	if c.runtime == nil {
		return ErrNilRuntime
	}
	if c.deployer == nil {
		return ErrNilDeployer
	}
	return nil
}

func (c *Coordinator) nowUTC() time.Time {
	now := c.now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	return now().UTC()
}

func cloneState(state State) State { return state }

func cloneResult(result TransitionResult) *TransitionResult {
	return &result
}

func runtimeOutcome(outcome runtime.TransitionOutcome) TransitionOutcome {
	switch outcome {
	case runtime.TransitionCommitted:
		return TransitionCommitted
	case runtime.TransitionRejected:
		return TransitionRejected
	case runtime.TransitionUncertain:
		return TransitionUncertain
	default:
		return TransitionUncertain
	}
}

func (c *Coordinator) setStateLocked(record *transitionRecord, phase TransitionPhase, reason string) {
	now := c.nowUTC()
	record.state.Phase = phase
	record.state.Reason = reason
	record.state.UpdatedAt = now
	if phase == PhaseCommitted || phase == PhaseRejected {
		record.state.CompletedAt = now
	}
	if c.current == nil || c.current.TransitionID == record.state.TransitionID {
		copy := record.state
		c.current = &copy
	}
}

func (c *Coordinator) signalBoundaryLocked(transitionID string) {
	ch := c.boundary[transitionID]
	if ch == nil {
		return
	}
	select {
	case ch <- struct{}{}:
	default:
	}
}

// HandleEvent delivers a runtime event to the coordinator.  It is safe to
// call this from a runtime event loop.  Events unrelated to an active
// transition are ignored, while malformed/mismatched safe-boundary events are
// rejected so a stale runtime cannot advance a transition.
func (c *Coordinator) HandleEvent(event runtime.Event) error {
	if c == nil {
		return ErrNilCoordinator
	}
	if event.EventType != runtime.EventSafeBoundaryReached {
		return nil
	}
	if event.TransitionID == "" {
		return ErrTransitionIDMismatch
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	record := c.transitions[event.TransitionID]
	if record == nil {
		return fmt.Errorf("%w: %s", ErrTransitionNotFound, event.TransitionID)
	}
	if record.state.RuntimeID != event.RuntimeID {
		return fmt.Errorf("%w: expected %q, got %q", ErrRuntimeIDMismatch, record.state.RuntimeID, event.RuntimeID)
	}
	if event.AuthGeneration != record.oldIdentity.AuthGeneration && event.AuthGeneration != record.state.ExpectedGeneration {
		return fmt.Errorf("%w: safe boundary generation %d, expected %d or %d", ErrGenerationMismatch, event.AuthGeneration, record.oldIdentity.AuthGeneration, record.state.ExpectedGeneration)
	}
	if record.state.Phase == PhaseWaitingForBoundary || record.state.Phase == PhasePrepared {
		c.signalBoundaryLocked(event.TransitionID)
	}
	return nil
}

// OnEvent is a compatibility alias for HandleEvent.
func (c *Coordinator) OnEvent(event runtime.Event) error { return c.HandleEvent(event) }

// consumeBoundaryEvent validates and handles one event while waiting.  The
// returned bool indicates whether the event was the matching safe boundary.
func (c *Coordinator) consumeBoundaryEvent(event runtime.Event, transitionID string) (bool, error) {
	if event.EventType != runtime.EventSafeBoundaryReached {
		return false, nil
	}
	if event.TransitionID != transitionID {
		// A different transition's boundary must not wake this request, but it
		// is otherwise a valid event for a caller-owned event loop.
		return false, nil
	}
	if err := c.HandleEvent(event); err != nil {
		return false, err
	}
	return true, nil
}

// FileIdentityReader reads only the account identity from the deployed
// auth.json. It never returns or stores the raw document. The runtime remains
// authoritative for parsing semantics; this reader is strictly a controller
// reconciliation observation of the account_id field supported by the vault
// contract.
type FileIdentityReader struct {
	Path string
}

// NewFileIdentityReader constructs a disk identity reader for path.
func NewFileIdentityReader(path string) *FileIdentityReader {
	return &FileIdentityReader{Path: path}
}

// ReadIdentity reads the current deployed identity. An auth file without an
// account_id returns an empty identity, not a guessed value.
func (r *FileIdentityReader) ReadIdentity() (string, error) {
	if r == nil {
		return "", errors.New("nil disk identity reader")
	}
	if r.Path == "" {
		return "", errors.New("disk identity path is empty")
	}
	info, err := os.Lstat(r.Path)
	if err != nil {
		return "", err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("disk identity path is a symbolic link")
	}
	if info.IsDir() {
		return "", errors.New("disk identity path is a directory")
	}
	raw, err := os.ReadFile(r.Path)
	if err != nil {
		return "", err
	}
	return credentials.ExtractAccountID(raw)
}

// DiskIdentityFunc adapts a function to DiskIdentityReader.
type DiskIdentityFunc func() (string, error)

func (f DiskIdentityFunc) ReadIdentity() (string, error) {
	if f == nil {
		return "", errors.New("nil disk identity function")
	}
	return f()
}
