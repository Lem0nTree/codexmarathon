// Package app contains the small CodexMarathon controller composition root.
//
// It wires the durable controller domains (accounts, credentials, journal,
// telemetry, policy, and reset scheduling) to a single runtime client.  The
// package deliberately does not supervise or launch Codext: a runtime is an
// already-running peer connected through an io.ReadWriteCloser.  A future
// process supervisor can own that transport without changing the controller
// domains.
package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"sync"
	"time"

	"codexmarathon/controller/internal/accounts"
	"codexmarathon/controller/internal/credentials"
	"codexmarathon/controller/internal/journal"
	"codexmarathon/controller/internal/policy"
	"codexmarathon/controller/internal/reset"
	"codexmarathon/controller/internal/runtime"
	"codexmarathon/controller/internal/telemetry"
	"codexmarathon/controller/internal/transitions"
)

const (
	// DefaultTelemetryTTL is intentionally short enough that a status display
	// does not present an old provider observation as current indefinitely.
	DefaultTelemetryTTL = 5 * time.Minute
	// DefaultConnectTimeout bounds only the initial protocol handshake. A
	// caller can use a longer context for a transition or reconciliation.
	DefaultConnectTimeout = 10 * time.Second
)

var (
	ErrRuntimeNotConnected = errors.New("runtime is not connected")
	ErrRuntimeAlreadySet   = errors.New("runtime is already connected")
	ErrRuntimeEndpoint     = errors.New("unsupported runtime endpoint")
	// ErrTelemetryRefetchRequired is returned for an ambiguous sparse update.
	// It is an observation requiring a full provider read, not evidence that
	// the account is unavailable.
	ErrTelemetryRefetchRequired = errors.New("telemetry refetch required")
)

// Config selects the controller's durable paths. Empty derived paths are
// filled from StateDir by New. Runtime connections are intentionally not part
// of Config so constructing a controller remains side-effect free; use
// ConnectRuntime for a live peer.
type Config struct {
	StateDir      string
	RegistryPath  string
	VaultDir      string
	AuthPath      string
	JournalPath   string
	TelemetryTTL  time.Duration
}

// DefaultConfig returns paths suitable for the current user. It does not
// create any files or directories. AuthPath follows Codex's conventional
// ~/.codex/auth.json location; operators should pass an explicit path when
// running against a non-default Codex installation.
func DefaultConfig() Config {
	stateDir := ""
	if configDir, err := os.UserConfigDir(); err == nil && configDir != "" {
		stateDir = filepath.Join(configDir, "CodexMarathon")
	}
	if stateDir == "" {
		stateDir = filepath.Join(".", ".codexmarathon")
	}
	authPath := ""
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		authPath = filepath.Join(home, ".codex", "auth.json")
	}
	if authPath == "" {
		authPath = filepath.Join(".", ".codex", "auth.json")
	}
	return Config{
		StateDir:     stateDir,
		AuthPath:     authPath,
		TelemetryTTL: DefaultTelemetryTTL,
	}
}

func (c Config) withDefaults() (Config, error) {
	defaults := DefaultConfig()
	if strings.TrimSpace(c.StateDir) == "" {
		c.StateDir = defaults.StateDir
	}
	if strings.TrimSpace(c.AuthPath) == "" {
		c.AuthPath = defaults.AuthPath
	}
	if strings.TrimSpace(c.RegistryPath) == "" {
		c.RegistryPath = filepath.Join(c.StateDir, "accounts.json")
	}
	if strings.TrimSpace(c.VaultDir) == "" {
		c.VaultDir = filepath.Join(c.StateDir, "credentials")
	}
	if strings.TrimSpace(c.JournalPath) == "" {
		c.JournalPath = filepath.Join(c.StateDir, "events.jsonl")
	}
	if c.TelemetryTTL == 0 {
		c.TelemetryTTL = DefaultTelemetryTTL
	}
	if c.TelemetryTTL < 0 {
		return Config{}, errors.New("telemetry TTL cannot be negative")
	}
	for name, path := range map[string]string{
		"state directory": c.StateDir,
		"registry path":   c.RegistryPath,
		"vault directory": c.VaultDir,
		"auth path":       c.AuthPath,
		"journal path":    c.JournalPath,
	} {
		if strings.TrimSpace(path) == "" {
			return Config{}, fmt.Errorf("%s is empty", name)
		}
	}
	return c, nil
}

// Controller is the composed controller runtime. Its fields are exposed as
// narrow read-only-in-practice accessors rather than global singletons so
// tests and later frontends can supply their own command loop.
type Controller struct {
	config Config

	registry *accounts.FileRegistry
	vault    *credentials.FileVault
	journal  *journal.FileJournal
	store    *telemetry.StateStore
	cache    *telemetry.SnapshotCache
	router   *telemetry.MultiAccountUsageRouter
	scheduler *reset.Scheduler
	policy   *policy.Engine

	mu            sync.RWMutex
	runtime      *runtime.Client
	runtimeFacade *clientRuntime
	coordinator   *transitions.Coordinator
	runtimeID    string
	protocolVersion int

	eventCancel context.CancelFunc
	eventDone   chan struct{}
	eventErrors chan error
	closeOnce   sync.Once
}

// New composes all controller domains without opening files or connecting to
// a runtime. It is safe to use for read-only status and account commands.
func New(config Config) (*Controller, error) {
	config, err := config.withDefaults()
	if err != nil {
		return nil, err
	}
	registry := accounts.NewFileRegistry(config.RegistryPath)
	vault := credentials.NewFileVault(config.VaultDir)
	fileJournal := journal.NewFileJournal(config.JournalPath)
	store := telemetry.NewStateStore()
	cache := telemetry.NewSnapshotCache(config.TelemetryTTL)
	router := telemetry.NewMultiAccountUsageRouter(cache, store)
	scheduler := reset.NewScheduler()
	return &Controller{
		config:      config,
		registry:    registry,
		vault:       vault,
		journal:     fileJournal,
		store:       store,
		cache:       cache,
		router:      router,
		scheduler:   scheduler,
		policy:      policy.NewEngine(scheduler),
		eventErrors: make(chan error, 32),
	}, nil
}

// EnsureLayout creates only the controller-owned state directories. It never
// creates or changes Codex's auth path and never reads credential contents.
func (c *Controller) EnsureLayout() error {
	if c == nil {
		return errors.New("nil controller")
	}
	for _, path := range []string{c.config.StateDir, c.config.VaultDir} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			return fmt.Errorf("create controller directory %q: %w", path, err)
		}
		if err := os.Chmod(path, 0o700); err != nil && !isPermissionMetadataError(err) {
			return fmt.Errorf("tighten controller directory %q: %w", path, err)
		}
	}
	return nil
}

// Config returns the effective paths without exposing any credential data.
func (c *Controller) Config() Config {
	if c == nil {
		return Config{}
	}
	return c.config
}

// Registry returns the durable account registry seam.
func (c *Controller) Registry() *accounts.FileRegistry {
	if c == nil {
		return nil
	}
	return c.registry
}

// Vault returns the credential vault seam. Callers should prefer transition
// methods and must not print the bytes returned by Vault.Load.
func (c *Controller) Vault() *credentials.FileVault {
	if c == nil {
		return nil
	}
	return c.vault
}

// Journal returns the append-only metadata journal.
func (c *Controller) Journal() *journal.FileJournal {
	if c == nil {
		return nil
	}
	return c.journal
}

// Store returns the normalized in-memory telemetry state store.
func (c *Controller) Store() *telemetry.StateStore {
	if c == nil {
		return nil
	}
	return c.store
}

// Cache returns the immutable-by-convention telemetry cache.
func (c *Controller) Cache() *telemetry.SnapshotCache {
	if c == nil {
		return nil
	}
	return c.cache
}

// Router returns the provider router used for active and inactive account
// observations.
func (c *Controller) Router() *telemetry.MultiAccountUsageRouter {
	if c == nil {
		return nil
	}
	return c.router
}

// Scheduler returns the pool-exhaustion/reset scheduler.
func (c *Controller) Scheduler() *reset.Scheduler {
	if c == nil {
		return nil
	}
	return c.scheduler
}

// Policy returns the decision engine composed with Scheduler.
func (c *Controller) Policy() *policy.Engine {
	if c == nil {
		return nil
	}
	return c.policy
}

// Coordinator returns nil until a runtime connection has been attached.
func (c *Controller) Coordinator() *transitions.Coordinator {
	if c == nil {
		return nil
	}
	c.mu.RLock()
	coordinator := c.coordinator
	c.mu.RUnlock()
	return coordinator
}

// RuntimeClient returns the live JSON-RPC client, or nil when disconnected.
func (c *Controller) RuntimeClient() *runtime.Client {
	if c == nil {
		return nil
	}
	c.mu.RLock()
	client := c.runtime
	c.mu.RUnlock()
	return client
}

// RuntimeConnected reports whether a live runtime client is attached.
func (c *Controller) RuntimeConnected() bool { return c.RuntimeClient() != nil }

// EventErrors exposes non-fatal protocol/telemetry errors from the event
// loop. A consumer may ignore this channel when it only needs request APIs.
func (c *Controller) EventErrors() <-chan error {
	if c == nil {
		return nil
	}
	return c.eventErrors
}

// ConnectRuntime attaches an already-running runtime, negotiates protocol v1,
// verifies its initial state, and starts the one event fan-out loop owned by
// the controller. The transport's ownership transfers to Controller.
func (c *Controller) ConnectRuntime(ctx context.Context, conn io.ReadWriteCloser) error {
	if c == nil {
		return errors.New("nil controller")
	}
	if conn == nil {
		return errors.New("runtime transport is nil")
	}
	if ctx == nil {
		ctx = context.Background()
	}

	c.mu.Lock()
	if c.runtime != nil {
		c.mu.Unlock()
		return ErrRuntimeAlreadySet
	}
	client := runtime.NewClient(conn)
	c.runtime = client
	c.runtimeFacade = &clientRuntime{client: client}
	deployer := credentials.NewAtomicDeployer(c.vault, c.config.AuthPath)
	c.coordinator = transitions.NewCoordinator(transitions.Config{
		Runtime:  c.runtimeFacade,
		Deployer: deployer,
		Disk:     transitions.NewFileIdentityReader(c.config.AuthPath),
		Journal:  c.journal,
	})
	c.mu.Unlock()

	connected := false
	defer func() {
		if !connected {
			_ = c.detachRuntime()
		}
	}()
	negotiated, err := client.NegotiateVersion(ctx, []int{runtime.ProtocolVersion})
	if err != nil {
		return fmt.Errorf("negotiate runtime protocol: %w", err)
	}
	state, err := client.GetRuntimeState(ctx)
	if err != nil {
		return fmt.Errorf("read runtime state after negotiation: %w", err)
	}
	if state.Identity.RuntimeID == "" {
		return errors.New("runtime returned an empty runtime_id")
	}
	c.mu.Lock()
	c.runtimeID = state.Identity.RuntimeID
	c.protocolVersion = negotiated.ProtocolVersion
	c.mu.Unlock()
	if err := c.journal.Append(journal.Event{
		Type:       journal.RuntimeConnected,
		RuntimeID:  state.Identity.RuntimeID,
		AccountID:  optionalAccountID(state.Identity.AccountID),
		Reason:     fmt.Sprintf("protocol v%d negotiated", negotiated.ProtocolVersion),
	}); err != nil {
		return fmt.Errorf("journal runtime connection: %w", err)
	}
	if err := c.StartEventLoop(context.Background()); err != nil {
		return err
	}
	connected = true
	return nil
}

// ConnectRuntimeEndpoint dials a supported local endpoint and calls
// ConnectRuntime. The MVP CLI supports tcp:// (and host:port shorthand) so
// it remains usable with a small external adapter harness on Windows. Named
// pipes/Unix sockets can be supplied directly through ConnectRuntime by a
// host-specific connector without changing the controller composition.
func (c *Controller) ConnectRuntimeEndpoint(ctx context.Context, endpoint string) error {
	conn, err := DialEndpoint(endpoint)
	if err != nil {
		return err
	}
	if err := c.ConnectRuntime(ctx, conn); err != nil {
		_ = conn.Close()
		return err
	}
	return nil
}

// DialEndpoint opens the transport-neutral endpoint forms supported by the
// standard library. tcp://host:port and host:port are supported everywhere;
// unix:// is available only on platforms whose net package supports Unix
// sockets. The CLI rejects named-pipe endpoints explicitly instead of
// pretending to provide Windows IPC it cannot verify.
func DialEndpoint(endpoint string) (io.ReadWriteCloser, error) {
	endpoint = strings.TrimSpace(endpoint)
	if endpoint == "" {
		return nil, fmt.Errorf("%w: endpoint is empty", ErrRuntimeEndpoint)
	}
	if !strings.Contains(endpoint, "://") {
		endpoint = "tcp://" + endpoint
	}
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrRuntimeEndpoint, err)
	}
	switch strings.ToLower(parsed.Scheme) {
	case "tcp":
		address := parsed.Host
		if address == "" {
			return nil, fmt.Errorf("%w: tcp endpoint has no host:port", ErrRuntimeEndpoint)
		}
		conn, err := net.DialTimeout("tcp", address, DefaultConnectTimeout)
		if err != nil {
			return nil, fmt.Errorf("dial runtime %q: %w", address, err)
		}
		return conn, nil
	case "unix":
		if parsed.Path == "" {
			return nil, fmt.Errorf("%w: unix endpoint has no path", ErrRuntimeEndpoint)
		}
		conn, err := net.DialTimeout("unix", parsed.Path, DefaultConnectTimeout)
		if err != nil {
			return nil, fmt.Errorf("dial runtime %q: %w", parsed.Path, err)
		}
		return conn, nil
	case "pipe", "npipe", "namedpipe":
		return nil, fmt.Errorf("%w: named pipes require a host-specific connector", ErrRuntimeEndpoint)
	default:
		return nil, fmt.Errorf("%w: scheme %q", ErrRuntimeEndpoint, parsed.Scheme)
	}
}

// StartEventLoop consumes the client's event stream exactly once. The
// transition coordinator intentionally does not read the client's channel
// directly; this avoids races between safe-boundary delivery and telemetry
// ingestion while preserving the coordinator's state-poll fallback.
func (c *Controller) StartEventLoop(ctx context.Context) error {
	if c == nil {
		return errors.New("nil controller")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	c.mu.Lock()
	client := c.runtime
	if client == nil {
		c.mu.Unlock()
		return ErrRuntimeNotConnected
	}
	if c.eventDone != nil {
		c.mu.Unlock()
		return errors.New("runtime event loop is already running")
	}
	loopCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	c.eventCancel = cancel
	c.eventDone = done
	c.mu.Unlock()
	go c.runEventLoop(loopCtx, client, done)
	return nil
}

// StopEventLoop stops the fan-out loop but leaves the runtime transport
// attached. Close or detachRuntime closes the transport itself.
func (c *Controller) StopEventLoop() {
	if c == nil {
		return
	}
	c.mu.Lock()
	cancel, done := c.eventCancel, c.eventDone
	c.eventCancel = nil
	c.eventDone = nil
	c.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if done != nil {
		<-done
	}
}

func (c *Controller) runEventLoop(ctx context.Context, client *runtime.Client, done chan struct{}) {
	defer close(done)
	errorEvents := client.Errors()
	for {
		select {
		case <-ctx.Done():
			return
		case event, ok := <-client.Events():
			if !ok {
				return
			}
			if err := c.HandleRuntimeEvent(event); err != nil {
				c.reportEventError(err)
			}
		case err, ok := <-errorEvents:
			if !ok {
				// Errors are auxiliary; continue draining events until the
				// transport closes its event channel.
				errorEvents = nil
				continue
			}
			c.reportEventError(err)
		}
	}
}

func (c *Controller) reportEventError(err error) {
	if c == nil || err == nil {
		return
	}
	select {
	case c.eventErrors <- err:
	default:
		// A full diagnostic channel must not stop transition or telemetry
		// processing. The durable journal remains the source of transition
		// evidence.
	}
}

// HandleRuntimeEvent routes one decoded event to transition correlation and
// controller telemetry. Unknown event types remain forward-compatible and are
// ignored after their common fields have been validated by runtime.Client.
func (c *Controller) HandleRuntimeEvent(event runtime.Event) error {
	if c == nil {
		return errors.New("nil controller")
	}
	if event.RuntimeID == "" {
		return errors.New("runtime event has no runtime_id")
	}
	c.mu.RLock()
	expectedRuntimeID := c.runtimeID
	coordinator := c.coordinator
	c.mu.RUnlock()
	if expectedRuntimeID != "" && event.RuntimeID != expectedRuntimeID {
		return fmt.Errorf("runtime event belongs to %q, expected %q", event.RuntimeID, expectedRuntimeID)
	}
	if coordinator != nil {
		if err := coordinator.HandleEvent(event); err != nil && !errors.Is(err, transitions.ErrTransitionNotFound) {
			return err
		}
	}

	switch event.EventType {
	case runtime.EventRateLimitsSnapshot:
		decoded, err := event.DecodePayload()
		if err != nil {
			return err
		}
		payload, ok := decoded.(*runtime.RateLimitsSnapshotEvent)
		if !ok {
			return errors.New("rate_limits_snapshot payload type mismatch")
		}
		accountID := optionalAccountID(payload.AccountID)
		if accountID == "" {
			// An unauthenticated runtime cannot produce a usable account
			// snapshot. Keep this as an observation, not a zero-quota claim.
			return nil
		}
		observedAt := eventTime(event.OccurredAt)
		snapshot := telemetry.NormalizeSnapshot(accountID, payload.RateLimits, payload.RateLimitsByID, observedAt)
		c.router.IngestFull(accountID, snapshot)
		_ = c.router.Register(accountID, snapshotProvider{store: c.store})
		return c.markTelemetry(accountID, observedAt)

	case runtime.EventRateLimitsUpdated:
		decoded, err := event.DecodePayload()
		if err != nil {
			return err
		}
		payload, ok := decoded.(*runtime.RateLimitsUpdatedEvent)
		if !ok {
			return errors.New("rate_limits_updated payload type mismatch")
		}
		accountID := optionalAccountID(payload.AccountID)
		if accountID == "" {
			return nil
		}
		result := c.router.IngestSparse(accountID, telemetry.AccountRateLimitsUpdatedWire{
			RateLimits: payload.RateLimits,
		})
		if result.RefetchRequired {
			return fmt.Errorf("%w for account %q: %s", ErrTelemetryRefetchRequired, accountID, result.Reason)
		}
		return c.markTelemetry(accountID, eventTime(event.OccurredAt))
	default:
		return nil
	}
}

func (c *Controller) markTelemetry(accountID string, observedAt time.Time) error {
	if c.registry == nil {
		return nil
	}
	if err := c.registry.SetTelemetry(accountID, observedAt, "runtime"); err != nil {
		if errors.Is(err, accounts.ErrAccountNotFound) {
			// Runtime telemetry may arrive before an operator registers the
			// account. StateStore remains useful and registration can happen
			// later without replaying the protocol event.
			return nil
		}
		return fmt.Errorf("mark telemetry for %q: %w", accountID, err)
	}
	return nil
}

func eventTime(seconds int64) time.Time {
	if seconds < 0 {
		return time.Unix(0, 0).UTC()
	}
	return time.Unix(seconds, 0).UTC()
}

func optionalAccountID(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

// AccountsTelemetry returns one normalized observation per registered
// account. Missing observations are explicitly unusable; they are not
// represented as zero usage.
func (c *Controller) AccountsTelemetry() ([]telemetry.AccountTelemetry, error) {
	if c == nil {
		return nil, errors.New("nil controller")
	}
	registered, err := c.registry.List()
	if err != nil {
		return nil, err
	}
	observations := make([]telemetry.AccountTelemetry, 0, len(registered))
	for _, account := range registered {
		if snapshot, ok := c.store.Get(account.ID); ok {
			observations = append(observations, snapshot)
			continue
		}
		observations = append(observations, telemetry.AccountTelemetry{
			AccountID: account.ID,
			IsUsable:  false,
			Source:    "missing",
		})
	}
	return observations, nil
}

// EvaluatePolicy applies the composed policy and reset scheduler to all
// registered accounts. It never starts a transition; callers decide whether
// to invoke RequestTransition after inspecting the returned decision.
func (c *Controller) EvaluatePolicy(now time.Time) (policy.PolicyDecision, error) {
	if c == nil {
		return policy.PolicyDecision{}, errors.New("nil controller")
	}
	observations, err := c.AccountsTelemetry()
	if err != nil {
		return policy.PolicyDecision{}, err
	}
	return c.policy.Evaluate(observations, now), nil
}

// RequestTransition delegates one decision-approved transition to the
// coordinator. It refuses to operate until a runtime peer is connected.
func (c *Controller) RequestTransition(ctx context.Context, accountID string) (*transitions.TransitionResult, error) {
	if c == nil {
		return nil, errors.New("nil controller")
	}
	coordinator := c.Coordinator()
	if coordinator == nil {
		return nil, ErrRuntimeNotConnected
	}
	return coordinator.RequestTransition(ctx, accountID)
}

// Reconcile delegates an uncertain transition to the coordinator.
func (c *Controller) Reconcile(ctx context.Context, transitionID string) (*transitions.TransitionResult, error) {
	if c == nil {
		return nil, errors.New("nil controller")
	}
	coordinator := c.Coordinator()
	if coordinator == nil {
		return nil, ErrRuntimeNotConnected
	}
	return coordinator.Reconcile(ctx, transitionID)
}

// RuntimeState performs one explicit runtime read for a status or operator
// command. It does not infer identity from the deployed auth file.
func (c *Controller) RuntimeState(ctx context.Context) (runtime.RuntimeState, error) {
	if c == nil {
		return runtime.RuntimeState{}, errors.New("nil controller")
	}
	client := c.RuntimeClient()
	if client == nil {
		return runtime.RuntimeState{}, ErrRuntimeNotConnected
	}
	return client.GetRuntimeState(ctx)
}

// Close stops event processing and closes the attached transport. It is
// idempotent and does not delete any controller state or donor checkout.
func (c *Controller) Close() error {
	if c == nil {
		return nil
	}
	var closeErr error
	c.closeOnce.Do(func() {
		c.StopEventLoop()
		closeErr = c.detachRuntime()
	})
	return closeErr
}

func (c *Controller) detachRuntime() error {
	c.mu.Lock()
	client := c.runtime
	runtimeID := c.runtimeID
	c.runtime = nil
	c.runtimeFacade = nil
	c.coordinator = nil
	c.runtimeID = ""
	c.protocolVersion = 0
	c.mu.Unlock()
	if client == nil {
		return nil
	}
	if runtimeID != "" && c.journal != nil {
		if err := c.journal.Append(journal.Event{Type: journal.RuntimeDisconnected, RuntimeID: runtimeID, Reason: "controller closed runtime connection"}); err != nil {
			closeErr := client.Close()
			return errors.Join(closeErr, err)
		}
	}
	return client.Close()
}

// clientRuntime delegates the transition surface while intentionally not
// implementing RuntimeEvents. Controller's single event loop calls
// Coordinator.HandleEvent, and the coordinator retains state polling as a
// fallback for transports with delayed notifications.
type clientRuntime struct{ client *runtime.Client }

func (r *clientRuntime) GetRuntimeState(ctx context.Context) (runtime.RuntimeState, error) {
	return r.client.GetRuntimeState(ctx)
}
func (r *clientRuntime) PrepareAuthTransition(ctx context.Context, params runtime.AuthTransitionParams) (runtime.TransitionResult, error) {
	return r.client.PrepareAuthTransition(ctx, params)
}
func (r *clientRuntime) CommitAuthTransition(ctx context.Context, params runtime.AuthTransitionParams) (runtime.TransitionResult, error) {
	return r.client.CommitAuthTransition(ctx, params)
}
func (r *clientRuntime) CancelAuthTransition(ctx context.Context, params runtime.CancelAuthTransitionParams) (runtime.TransitionResult, error) {
	return r.client.CancelAuthTransition(ctx, params)
}
func (r *clientRuntime) GetIdentity(ctx context.Context) (runtime.Identity, error) {
	return r.client.GetIdentity(ctx)
}
func (r *clientRuntime) GetAuthGeneration(ctx context.Context) (runtime.AuthGenerationResult, error) {
	return r.client.GetAuthGeneration(ctx)
}

// snapshotProvider exposes already-ingested runtime state through the common
// provider router. It never invents capacity and never performs a second
// active-account network probe.
type snapshotProvider struct{ store *telemetry.StateStore }

func (p snapshotProvider) Snapshot(_ context.Context, accountID string) (telemetry.SnapshotResult, error) {
	if p.store == nil {
		err := errors.New("telemetry state store is nil")
		return telemetry.UnusableSnapshot(err), err
	}
	snapshot, ok := p.store.Get(accountID)
	if !ok {
		err := fmt.Errorf("no runtime telemetry snapshot for %q", accountID)
		return telemetry.UnusableSnapshot(err), err
	}
	return telemetry.UsableSnapshot(snapshot), nil
}

func isPermissionMetadataError(err error) bool {
	if err == nil {
		return false
	}
	// chmod is advisory on Windows; unlike the domain packages we do not need
	// platform-specific syscall handling in the composition root.
	return os.IsPermission(err) && goruntime.GOOS == "windows"
}
