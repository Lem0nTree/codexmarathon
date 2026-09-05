package app

// This file owns the operational boundary of CodexMarathon.  The controller
// domains remain usable in tests and libraries, while RuntimeSupervisor is
// the one-command product entry point: it starts the bundled runtime, dials a
// user-scoped local IPC endpoint, negotiates the protocol, watches health, and
// reconnects/reconciles after a runtime loss.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"codexmarathon/controller/internal/transitions"
)

var (
	ErrSupervisorAlreadyStarted = errors.New("runtime supervisor is already started")
	ErrSupervisorNotStarted     = errors.New("runtime supervisor is not started")
	ErrBundledRuntimeMissing    = errors.New("bundled CodexMarathon runtime is not installed")
	ErrUnauthenticatedTCP       = errors.New("unauthenticated TCP runtime endpoints are disabled")
	ErrUnsafeRuntimeEndpoint    = errors.New("runtime endpoint is not user-scoped")
)

const (
	defaultHealthInterval    = 5 * time.Second
	defaultConnectTimeout    = 15 * time.Second
	defaultReconnectInterval = 750 * time.Millisecond
	defaultReconnectMax      = 8 * time.Second
	defaultShutdownTimeout   = 10 * time.Second
)

// RuntimeConnector is injectable for integration fixtures.  Production uses
// DialManagedEndpoint, which only permits authenticated-by-OS local IPC.
type RuntimeConnector func(context.Context, string) (io.ReadWriteCloser, error)

// SupervisorConfig controls the bundled runtime process and its local IPC.
// RuntimeCommand is argv-shaped (the first element is the executable), never
// a shell command.  The supervisor injects the stable
// CODEXMARATHON_LISTEN=<endpoint> environment contract into the bundled
// runtime.  Using an environment contract lets the unmodified Codex app
// server continue to own its normal command-line transport while the
// Marathon listener is an additional local control boundary.
type SupervisorConfig struct {
	Endpoint           string
	RuntimeCommand     []string
	RuntimeArgs        []string
	RuntimeDir         string
	RuntimeEnv         []string
	ProtocolVersions   []int
	ConnectTimeout     time.Duration
	HealthInterval     time.Duration
	ReconnectInterval  time.Duration
	ReconnectMax       time.Duration
	ShutdownTimeout    time.Duration
	MaxRestarts        int
	DiagnosticsPath    string
	Connector          RuntimeConnector
	DisableAutoRestart bool
}

func (c SupervisorConfig) withDefaults(controller *Controller) (SupervisorConfig, error) {
	if controller == nil {
		return SupervisorConfig{}, errors.New("controller is nil")
	}
	if strings.TrimSpace(c.Endpoint) == "" {
		endpoint, err := DefaultRuntimeEndpoint(controller.Config().StateDir)
		if err != nil {
			return SupervisorConfig{}, err
		}
		c.Endpoint = endpoint
	}
	if err := ValidateManagedEndpoint(c.Endpoint, controller.Config().StateDir); err != nil {
		return SupervisorConfig{}, err
	}
	if len(c.ProtocolVersions) == 0 {
		c.ProtocolVersions = []int{1}
	}
	versionOne := false
	for _, version := range c.ProtocolVersions {
		if version == 1 {
			versionOne = true
		}
	}
	if !versionOne {
		return SupervisorConfig{}, fmt.Errorf("runtime supervisor supports protocol v1; configured versions=%v", c.ProtocolVersions)
	}
	if c.ConnectTimeout <= 0 {
		c.ConnectTimeout = defaultConnectTimeout
	}
	if c.HealthInterval <= 0 {
		c.HealthInterval = defaultHealthInterval
	}
	if c.ReconnectInterval <= 0 {
		c.ReconnectInterval = defaultReconnectInterval
	}
	if c.ReconnectMax <= 0 {
		c.ReconnectMax = defaultReconnectMax
	}
	if c.ShutdownTimeout <= 0 {
		c.ShutdownTimeout = defaultShutdownTimeout
	}
	externalConnector := c.Connector != nil
	if c.Connector == nil {
		c.Connector = DialManagedEndpoint
	}
	if strings.TrimSpace(c.DiagnosticsPath) == "" {
		c.DiagnosticsPath = filepath.Join(controller.Config().StateDir, "diagnostics.json")
	}
	if len(c.RuntimeCommand) == 0 && (controller.RuntimeConnected() || externalConnector) {
		// A caller may supervise a runtime it owns outside this process while
		// still benefiting from health/reconciliation.  The product CLI always
		// supplies/derives a bundled command below.
		return c, nil
	}
	if len(c.RuntimeCommand) == 0 {
		command, err := ResolveBundledRuntimeCommand()
		if err != nil {
			return SupervisorConfig{}, err
		}
		c.RuntimeCommand = []string{command}
	}
	if strings.TrimSpace(c.RuntimeCommand[0]) == "" {
		return SupervisorConfig{}, errors.New("runtime command executable is empty")
	}
	return c, nil
}

// LifecycleState is intentionally small so status output is stable and easy
// for scripts to consume.
type LifecycleState string

const (
	LifecycleStopped  LifecycleState = "stopped"
	LifecycleStarting LifecycleState = "starting"
	LifecycleReady    LifecycleState = "ready"
	LifecycleDegraded LifecycleState = "degraded"
	LifecycleStopping LifecycleState = "stopping"
	LifecycleFailed   LifecycleState = "failed"
)

// LifecycleStatus is safe to serialize: it contains no auth material, token,
// or environment value.
type LifecycleStatus struct {
	State             LifecycleState `json:"state"`
	Endpoint          string         `json:"endpoint"`
	RuntimeCommand    string         `json:"runtime_command,omitempty"`
	PID               int            `json:"pid,omitempty"`
	RuntimeID         string         `json:"runtime_id,omitempty"`
	ProtocolVersion   int            `json:"protocol_version,omitempty"`
	RuntimeConnected  bool           `json:"runtime_connected"`
	RestartCount      int            `json:"restart_count"`
	StartedAt         time.Time      `json:"started_at,omitempty"`
	ReadyAt           time.Time      `json:"ready_at,omitempty"`
	LastHealthAt      time.Time      `json:"last_health_at,omitempty"`
	LastDisconnectAt  time.Time      `json:"last_disconnect_at,omitempty"`
	LastReconcileAt   time.Time      `json:"last_reconcile_at,omitempty"`
	LastError         string         `json:"last_error,omitempty"`
	LastExit          string         `json:"last_exit,omitempty"`
	ReconciliationErr string         `json:"reconciliation_error,omitempty"`
}

// RuntimeSupervisor owns the runtime process lifecycle.  It is safe to call
// Status and WriteDiagnostics while Start/monitor/reconnect are in progress.
type RuntimeSupervisor struct {
	controller *Controller
	config     SupervisorConfig

	mu            sync.RWMutex
	status        LifecycleStatus
	started       bool
	stopping      bool
	cancel        context.CancelFunc
	monitorDone   chan struct{}
	process       *exec.Cmd
	processDone   chan error
	processWaited bool
}

// NewRuntimeSupervisor validates the secure endpoint and composes a process
// supervisor.  It performs no process or socket side effects.
func NewRuntimeSupervisor(controller *Controller, config SupervisorConfig) (*RuntimeSupervisor, error) {
	if controller == nil {
		return nil, errors.New("controller is nil")
	}
	normalized, err := config.withDefaults(controller)
	if err != nil {
		return nil, err
	}
	return &RuntimeSupervisor{
		controller: controller,
		config:     normalized,
		status: LifecycleStatus{
			State:    LifecycleStopped,
			Endpoint: normalized.Endpoint,
		},
	}, nil
}

// Start starts the bundled runtime (when not already attached), waits for
// protocol readiness, and then starts health/reconnect monitoring.
func (s *RuntimeSupervisor) Start(ctx context.Context) error {
	if s == nil || s.controller == nil {
		return errors.New("runtime supervisor is nil")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	s.mu.Lock()
	if s.started {
		s.mu.Unlock()
		return ErrSupervisorAlreadyStarted
	}
	s.started = true
	s.stopping = false
	s.status.State = LifecycleStarting
	s.status.StartedAt = time.Now().UTC()
	s.status.LastError = ""
	loopCtx, cancel := context.WithCancel(ctx)
	s.cancel = cancel
	s.monitorDone = make(chan struct{})
	s.mu.Unlock()

	if err := s.controller.EnsureLayout(); err != nil {
		return s.failStart(err)
	}
	if err := s.startManagedProcess(loopCtx); err != nil {
		return s.failStart(err)
	}
	if err := s.connectWithRetry(loopCtx, s.config.ConnectTimeout); err != nil {
		return s.failStart(err)
	}
	s.markReady()
	go s.monitor(loopCtx)
	_ = s.WriteDiagnostics("")
	return nil
}

// Run is the convenient one-command blocking form used by the CLI.
func (s *RuntimeSupervisor) Run(ctx context.Context) error {
	if err := s.Start(ctx); err != nil {
		return err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	s.mu.RLock()
	monitorDone := s.monitorDone
	s.mu.RUnlock()
	select {
	case <-ctx.Done():
		return s.Stop()
	case <-monitorDone:
		status := s.Status()
		if status.State == LifecycleFailed {
			return fmt.Errorf("runtime supervisor stopped: %s", status.LastError)
		}
		return nil
	}
}

// Stop performs an orderly disconnect and then asks the managed process to
// exit.  A stubborn process is force-terminated after ShutdownTimeout.
func (s *RuntimeSupervisor) Stop() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	if !s.started {
		s.mu.Unlock()
		return nil
	}
	s.stopping = true
	s.status.State = LifecycleStopping
	cancel := s.cancel
	monitorDone := s.monitorDone
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if s.controller != nil {
		_ = s.controller.DisconnectRuntime("runtime supervisor stopping")
	}
	if monitorDone != nil {
		<-monitorDone
	}
	// The monitor may have consumed processDone while Stop was waiting for it.
	// Re-read ownership after the monitor exits so we never wait on a channel
	// whose process-exit value was already consumed by another goroutine.
	s.mu.RLock()
	process := s.process
	processDone := s.processDone
	processWaited := s.processWaited
	s.mu.RUnlock()
	var err error
	if !processWaited {
		err = s.stopProcess(process, processDone)
	}
	s.mu.Lock()
	s.started = false
	s.cancel = nil
	s.monitorDone = nil
	s.process = nil
	s.processDone = nil
	s.status.State = LifecycleStopped
	s.status.RuntimeConnected = false
	s.status.PID = 0
	if err != nil {
		s.status.LastError = err.Error()
	}
	s.mu.Unlock()
	_ = s.WriteDiagnostics("")
	return err
}

// Wait blocks until the monitor stops.  It is useful to embed the supervisor
// in a host that owns signal handling itself.
func (s *RuntimeSupervisor) Wait() error {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	done := s.monitorDone
	s.mu.RUnlock()
	if done == nil {
		return ErrSupervisorNotStarted
	}
	<-done
	return nil
}

// Status returns a race-free operational snapshot.
func (s *RuntimeSupervisor) Status() LifecycleStatus {
	if s == nil {
		return LifecycleStatus{State: LifecycleFailed, LastError: "runtime supervisor is nil"}
	}
	s.mu.RLock()
	status := s.status
	status.RuntimeConnected = s.controller != nil && s.controller.RuntimeConnected()
	if s.controller != nil {
		status.ProtocolVersion = s.controller.RuntimeProtocolVersion()
	}
	s.mu.RUnlock()
	return status
}

// WriteDiagnostics writes a redacted atomic status document.  An empty path
// uses the configured state-dir/diagnostics.json location.
func (s *RuntimeSupervisor) WriteDiagnostics(path string) error {
	if s == nil {
		return errors.New("runtime supervisor is nil")
	}
	if strings.TrimSpace(path) == "" {
		path = s.config.DiagnosticsPath
	}
	if strings.TrimSpace(path) == "" {
		return errors.New("diagnostics path is empty")
	}
	status := s.Status()
	payload, err := json.MarshalIndent(status, "", "  ")
	if err != nil {
		return fmt.Errorf("encode runtime diagnostics: %w", err)
	}
	path, err = filepath.Abs(path)
	if err != nil {
		return fmt.Errorf("resolve diagnostics path: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create diagnostics directory: %w", err)
	}
	temp, err := os.CreateTemp(filepath.Dir(path), ".diagnostics-*.tmp")
	if err != nil {
		return fmt.Errorf("create diagnostics file: %w", err)
	}
	tempName := temp.Name()
	defer func() { _ = os.Remove(tempName) }()
	if err := temp.Chmod(0o600); err != nil && runtime.GOOS != "windows" {
		_ = temp.Close()
		return fmt.Errorf("protect diagnostics file: %w", err)
	}
	if _, err := temp.Write(append(payload, '\n')); err != nil {
		_ = temp.Close()
		return fmt.Errorf("write runtime diagnostics: %w", err)
	}
	if err := temp.Sync(); err != nil {
		_ = temp.Close()
		return fmt.Errorf("flush runtime diagnostics: %w", err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("close runtime diagnostics: %w", err)
	}
	if err := os.Rename(tempName, path); err != nil {
		if runtime.GOOS != "windows" {
			return fmt.Errorf("publish runtime diagnostics: %w", err)
		}
		// Windows does not replace an existing destination on every supported
		// filesystem. Remove only the exact diagnostics target, then publish
		// the already-fsynced temporary file; no broad cleanup is attempted.
		if removeErr := os.Remove(path); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			return fmt.Errorf("publish runtime diagnostics: %w", err)
		}
		if renameErr := os.Rename(tempName, path); renameErr != nil {
			return fmt.Errorf("publish runtime diagnostics: %w", renameErr)
		}
	}
	return nil
}

func (s *RuntimeSupervisor) failStart(err error) error {
	if err == nil {
		err = errors.New("runtime supervisor failed to start")
	}
	s.mu.Lock()
	s.status.State = LifecycleFailed
	s.status.LastError = err.Error()
	s.started = false
	cancel := s.cancel
	process := s.process
	processDone := s.processDone
	s.cancel = nil
	s.process = nil
	s.processDone = nil
	s.monitorDone = nil
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if s.controller != nil {
		_ = s.controller.DisconnectRuntime("runtime supervisor startup failed")
	}
	_ = s.stopProcess(process, processDone)
	_ = s.WriteDiagnostics("")
	return err
}

func (s *RuntimeSupervisor) markReady() {
	s.mu.Lock()
	s.status.State = LifecycleReady
	s.status.ReadyAt = time.Now().UTC()
	s.status.LastError = ""
	s.status.RuntimeConnected = s.controller.RuntimeConnected()
	s.status.ProtocolVersion = s.controller.RuntimeProtocolVersion()
	s.mu.Unlock()
}

func (s *RuntimeSupervisor) setError(err error, state LifecycleState) {
	if err == nil {
		return
	}
	s.mu.Lock()
	s.status.State = state
	s.status.LastError = err.Error()
	s.mu.Unlock()
}

func (s *RuntimeSupervisor) startManagedProcess(_ context.Context) error {
	s.mu.RLock()
	alreadyConnected := s.controller.RuntimeConnected()
	command := append([]string(nil), s.config.RuntimeCommand...)
	args := append([]string(nil), s.config.RuntimeArgs...)
	runtimeDir := s.config.RuntimeDir
	env := append([]string(nil), s.config.RuntimeEnv...)
	endpoint := s.config.Endpoint
	s.mu.RUnlock()
	if alreadyConnected || len(command) == 0 {
		return nil
	}
	if runtimeDir == "" {
		runtimeDir = filepath.Dir(command[0])
	}
	// The embedded app-server reads this variable after it has constructed its
	// native AuthManager/ThreadManager.  Do not pass a Marathon-specific CLI
	// flag to a Codex binary that does not know that flag; the supervisor owns
	// the integration boundary and the runtime remains a normal Codex binary.
	env = append(env, "CODEXMARATHON_LISTEN="+endpoint)
	if isCodexAppServer(command[0]) && !hasFlag(args, "--listen") {
		// The supervisor owns the process over local IPC.  Inheriting the
		// app-server's stdio transport would make it exit when the child has no
		// interactive stdin, so explicitly disable that transport.
		args = append(args, "--listen", "off")
	}
	// Do not bind the child directly to loopCtx: cancelling that context is
	// also how the supervisor asks its monitor to stop, and CommandContext
	// would send an unconditional kill before stopProcess can deliver the
	// graceful interrupt/timeout sequence.
	cmd := exec.Command(command[0], append(command[1:], args...)...)
	cmd.Dir = runtimeDir
	if len(env) > 0 {
		cmd.Env = append(os.Environ(), env...)
	}
	// Runtime stderr may contain provider diagnostics.  It is intentionally
	// discarded by the supervisor so tokens cannot leak into CLI logs; callers
	// can inspect the redacted lifecycle status instead.
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start bundled CodexMarathon runtime %q: %w", command[0], err)
	}
	processDone := make(chan error, 1)
	go func() { processDone <- cmd.Wait() }()
	s.mu.Lock()
	s.process = cmd
	s.processDone = processDone
	s.processWaited = false
	s.status.PID = cmd.Process.Pid
	s.status.RuntimeCommand = command[0]
	s.mu.Unlock()
	return nil
}

func (s *RuntimeSupervisor) connectWithRetry(ctx context.Context, timeout time.Duration) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if timeout <= 0 {
		timeout = s.config.ConnectTimeout
	}
	connectCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var lastErr error
	delay := s.config.ReconnectInterval
	for {
		if s.controller.RuntimeConnected() {
			return nil
		}
		conn, err := s.config.Connector(connectCtx, s.config.Endpoint)
		if err == nil && conn == nil {
			err = errors.New("runtime connector returned a nil transport")
		}
		if err == nil {
			attachCtx, attachCancel := context.WithTimeout(connectCtx, s.config.ConnectTimeout)
			err = s.controller.ConnectRuntime(attachCtx, conn)
			attachCancel()
			if err == nil {
				if reconcileErr := s.reconcileAfterReconnect(connectCtx); reconcileErr != nil {
					s.mu.Lock()
					s.status.ReconciliationErr = reconcileErr.Error()
					s.mu.Unlock()
				}
				return nil
			}
			_ = conn.Close()
		}
		lastErr = err
		if lastErr == nil {
			lastErr = errors.New("runtime connection failed")
		}
		select {
		case <-connectCtx.Done():
			return fmt.Errorf("runtime did not become ready at %s: %w (last error: %v)", s.config.Endpoint, connectCtx.Err(), lastErr)
		case <-time.After(delay):
		}
		if delay < s.config.ReconnectMax {
			delay *= 2
			if delay > s.config.ReconnectMax {
				delay = s.config.ReconnectMax
			}
		}
	}
}

func (s *RuntimeSupervisor) reconcileAfterReconnect(ctx context.Context) error {
	if !s.controller.RuntimeConnected() {
		return ErrRuntimeNotConnected
	}
	s.mu.Lock()
	s.status.LastReconcileAt = time.Now().UTC()
	s.mu.Unlock()
	// Reconcile is deliberately best-effort here.  A clean runtime with no
	// pending transition returns ErrTransitionNotFound; an actual uncertain
	// transition remains visible in diagnostics for the operator or the durable
	// recovery coordinator to inspect.
	_, err := s.controller.Reconcile(ctx, "")
	if errors.Is(err, transitions.ErrTransitionNotFound) {
		err = nil
	}
	if err != nil {
		return err
	}
	// Recovery reconciliation is independent of transition lookup. It reads
	// Codext's runtime-owned recovery map and retries only durable, already
	// identity-verified release intents.
	if manager := s.controller.Recovery(); manager != nil {
		if recoveryErr := manager.Reconcile(ctx); recoveryErr != nil {
			return recoveryErr
		}
	}
	return nil
}

func (s *RuntimeSupervisor) monitor(ctx context.Context) {
	defer close(s.monitorDone)
	ticker := time.NewTicker(s.config.HealthInterval)
	defer ticker.Stop()
	var processDone <-chan error
	s.mu.RLock()
	if s.processDone != nil {
		processDone = s.processDone
	}
	s.mu.RUnlock()
	for {
		select {
		case <-ctx.Done():
			return
		case err := <-processDone:
			if processDone == nil {
				continue
			}
			s.handleProcessExit(err)
			processDone = nil
			if s.config.DisableAutoRestart || s.isStopping() {
				s.setError(errors.New("managed runtime exited"), LifecycleFailed)
				return
			}
			if err := s.restart(ctx); err != nil {
				s.setError(err, LifecycleFailed)
				if s.config.MaxRestarts > 0 && s.Status().RestartCount >= s.config.MaxRestarts {
					return
				}
			}
			s.mu.RLock()
			processDone = s.processDone
			s.mu.RUnlock()
		case <-ticker.C:
			s.healthCheck(ctx)
		}
	}
}

func (s *RuntimeSupervisor) healthCheck(ctx context.Context) {
	if s.isStopping() {
		return
	}
	if !s.controller.RuntimeConnected() {
		s.setError(errors.New("runtime is disconnected"), LifecycleDegraded)
		if !s.hasLiveProcess() && !s.config.DisableAutoRestart {
			if err := s.restart(ctx); err != nil {
				s.setError(err, LifecycleDegraded)
				return
			}
		}
		if err := s.connectWithRetry(ctx, s.config.ReconnectMax); err != nil {
			s.setError(err, LifecycleDegraded)
			return
		}
		s.markReady()
		_ = s.WriteDiagnostics("")
		return
	}
	healthCtx, cancel := context.WithTimeout(ctx, s.config.ConnectTimeout)
	_, err := s.controller.RuntimeState(healthCtx)
	cancel()
	s.mu.Lock()
	s.status.LastHealthAt = time.Now().UTC()
	s.mu.Unlock()
	if err == nil {
		return
	}
	s.mu.Lock()
	s.status.LastDisconnectAt = time.Now().UTC()
	s.mu.Unlock()
	_ = s.controller.DisconnectRuntime("runtime health check failed: " + err.Error())
	s.setError(fmt.Errorf("runtime health check: %w", err), LifecycleDegraded)
	if reconnectErr := s.connectWithRetry(ctx, s.config.ReconnectMax); reconnectErr != nil {
		s.setError(reconnectErr, LifecycleDegraded)
		return
	}
	s.markReady()
	_ = s.WriteDiagnostics("")
}

func (s *RuntimeSupervisor) hasLiveProcess() bool {
	s.mu.RLock()
	live := s.process != nil && !s.processWaited
	s.mu.RUnlock()
	return live
}

func (s *RuntimeSupervisor) restart(ctx context.Context) error {
	s.mu.Lock()
	s.status.RestartCount++
	s.mu.Unlock()
	if err := s.startManagedProcess(ctx); err != nil {
		return err
	}
	if err := s.connectWithRetry(ctx, s.config.ConnectTimeout); err != nil {
		return err
	}
	s.markReady()
	return nil
}

func (s *RuntimeSupervisor) handleProcessExit(err error) {
	s.mu.Lock()
	s.process = nil
	s.processDone = nil
	s.processWaited = true
	s.status.PID = 0
	if err != nil {
		s.status.LastExit = err.Error()
	} else {
		s.status.LastExit = "runtime exited"
	}
	s.mu.Unlock()
	_ = s.controller.DisconnectRuntime("managed runtime exited")
}

func (s *RuntimeSupervisor) isStopping() bool {
	s.mu.RLock()
	stopping := s.stopping
	s.mu.RUnlock()
	return stopping
}

func (s *RuntimeSupervisor) stopProcess(process *exec.Cmd, processDone <-chan error) error {
	if process == nil {
		return nil
	}
	if processDone == nil {
		return nil
	}
	select {
	case err := <-processDone:
		return normalizeProcessExit(err)
	default:
	}
	if process.Process != nil {
		if runtime.GOOS == "windows" {
			_ = process.Process.Kill()
		} else {
			_ = process.Process.Signal(os.Interrupt)
		}
	}
	timer := time.NewTimer(s.config.ShutdownTimeout)
	defer timer.Stop()
	select {
	case err := <-processDone:
		return normalizeProcessExit(err)
	case <-timer.C:
		if process.Process != nil {
			_ = process.Process.Kill()
		}
		return fmt.Errorf("managed runtime did not exit within %s", s.config.ShutdownTimeout)
	}
}

func normalizeProcessExit(err error) error {
	if err == nil {
		return nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 0 {
		return nil
	}
	return err
}

func hasFlag(args []string, name string) bool {
	for _, arg := range args {
		if arg == name || strings.HasPrefix(arg, name+"=") {
			return true
		}
	}
	return false
}

func isCodexAppServer(command string) bool {
	name := strings.ToLower(filepath.Base(command))
	return name == "codex-app-server" || name == "codex-app-server.exe"
}

// DefaultRuntimeEndpoint returns the secure, user-scoped local endpoint.  No
// product default is TCP.  The state directory is also the trust boundary for
// Unix sockets and is created with mode 0700 by Controller.EnsureLayout.
func DefaultRuntimeEndpoint(stateDir string) (string, error) {
	if strings.TrimSpace(stateDir) == "" {
		return "", errors.New("state directory is empty")
	}
	stateDir, err := filepath.Abs(stateDir)
	if err != nil {
		return "", fmt.Errorf("resolve state directory: %w", err)
	}
	if runtime.GOOS == "windows" {
		return "pipe://./pipe/" + userScopedPipeName(), nil
	}
	return "unix://" + filepath.Join(stateDir, "runtime.sock"), nil
}

func userScopedPipeName() string {
	identity := os.Getenv("USERNAME")
	if identity == "" {
		identity = os.Getenv("USER")
	}
	if identity == "" {
		if current, err := user.Current(); err == nil {
			identity = current.Username
		}
	}
	if identity == "" {
		identity = "unknown"
	}
	hash := sha256.Sum256([]byte(identity))
	return "codexmarathon-" + hex.EncodeToString(hash[:8])
}

// ValidateManagedEndpoint rejects network transports and paths outside the
// controller's user-owned state directory.  This function is called before a
// managed process starts, so an accidental `tcp://0.0.0.0` can never become a
// product listener.
func ValidateManagedEndpoint(endpoint, stateDir string) error {
	endpoint = strings.TrimSpace(endpoint)
	if endpoint == "" {
		return fmt.Errorf("%w: endpoint is empty", ErrUnsafeRuntimeEndpoint)
	}
	if !strings.Contains(endpoint, "://") {
		return fmt.Errorf("%w: use unix:// or pipe://; bare host:port is not allowed", ErrUnsafeRuntimeEndpoint)
	}
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrUnsafeRuntimeEndpoint, err)
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return fmt.Errorf("%w: runtime endpoints must not include user-info, query, or fragment data", ErrUnsafeRuntimeEndpoint)
	}
	scheme := strings.ToLower(parsed.Scheme)
	switch scheme {
	case "tcp":
		return fmt.Errorf("%w: %s", ErrUnauthenticatedTCP, parsed.Host)
	case "unix":
		if runtime.GOOS == "windows" {
			return fmt.Errorf("%w: Unix sockets are not supported on Windows", ErrUnsafeRuntimeEndpoint)
		}
		if parsed.Host != "" {
			return fmt.Errorf("%w: Unix socket endpoint must not include a host", ErrUnsafeRuntimeEndpoint)
		}
		path := filepath.Clean(filepath.FromSlash(parsed.Path))
		if !filepath.IsAbs(path) {
			return fmt.Errorf("%w: Unix socket path must be absolute", ErrUnsafeRuntimeEndpoint)
		}
		stateAbs, err := filepath.Abs(stateDir)
		if err != nil {
			return fmt.Errorf("resolve state directory: %w", err)
		}
		rel, err := filepath.Rel(filepath.Clean(stateAbs), path)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return fmt.Errorf("%w: Unix socket must live below state directory", ErrUnsafeRuntimeEndpoint)
		}
		if path == filepath.Clean(stateAbs) {
			return fmt.Errorf("%w: endpoint cannot be the state directory", ErrUnsafeRuntimeEndpoint)
		}
		return nil
	case "pipe", "npipe", "namedpipe":
		if runtime.GOOS != "windows" {
			return fmt.Errorf("%w: named pipes are Windows-only", ErrUnsafeRuntimeEndpoint)
		}
		if parsed.Host == "" && parsed.Path == "" {
			return fmt.Errorf("%w: named-pipe name is empty", ErrUnsafeRuntimeEndpoint)
		}
		expected := `\\.\pipe\` + userScopedPipeName()
		if namedPipePath(parsed) != expected {
			return fmt.Errorf("%w: named pipe must use the current user's CodexMarathon endpoint", ErrUnsafeRuntimeEndpoint)
		}
		return nil
	default:
		return fmt.Errorf("%w: unsupported scheme %q", ErrUnsafeRuntimeEndpoint, parsed.Scheme)
	}
}

// DialManagedEndpoint is the only connector used by RuntimeSupervisor.
// Explicit TCP remains available through the legacy diagnostic DialEndpoint,
// but cannot be selected through this product path.
func DialManagedEndpoint(ctx context.Context, endpoint string) (io.ReadWriteCloser, error) {
	endpoint = strings.TrimSpace(endpoint)
	if endpoint == "" {
		return nil, fmt.Errorf("%w: endpoint is empty", ErrUnsafeRuntimeEndpoint)
	}
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnsafeRuntimeEndpoint, err)
	}
	switch strings.ToLower(parsed.Scheme) {
	case "unix":
		return dialUserScopedUnix(ctx, filepath.Clean(filepath.FromSlash(parsed.Path)))
	case "pipe", "npipe", "namedpipe":
		path := namedPipePath(parsed)
		return dialUserNamedPipe(ctx, path)
	case "tcp":
		return nil, fmt.Errorf("%w: %s", ErrUnauthenticatedTCP, parsed.Host)
	default:
		return nil, fmt.Errorf("%w: unsupported scheme %q", ErrUnsafeRuntimeEndpoint, parsed.Scheme)
	}
}

func namedPipePath(parsed *url.URL) string {
	name := strings.Trim(parsed.Host+parsed.Path, "/")
	name = strings.TrimPrefix(name, "./")
	name = strings.TrimPrefix(name, ".\\")
	name = strings.ReplaceAll(name, "/", `\`)
	name = strings.TrimPrefix(name, `pipe\`)
	if strings.HasPrefix(name, `\\.\pipe\`) {
		return name
	}
	return `\\.\pipe\` + name
}

// ResolveBundledRuntimeCommand finds the runtime shipped next to the
// CodexMarathon launcher.  It never searches PATH for an unrelated external
// donor executable.
func ResolveBundledRuntimeCommand() (string, error) {
	current, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("resolve codexmarathon executable: %w", err)
	}
	dir := filepath.Dir(current)
	names := []string{"codexmarathon-runtime", "codexmarathon-runtime.exe", "codex-app-server", "codex-app-server.exe"}
	for _, name := range names {
		candidate := filepath.Join(dir, name)
		if info, statErr := os.Stat(candidate); statErr == nil && !info.IsDir() {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("%w: expected codexmarathon-runtime beside %q; build the bundled runtime package first", ErrBundledRuntimeMissing, current)
}
