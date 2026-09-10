package app

// This file exposes the controller-owned local control boundary used by an
// embedded Codex TUI.  The TUI never receives credential bytes and never
// writes auth.json.  It sends an account target to this server; the
// controller resolves aliases and delegates the complete transition to the
// existing coordinator (or to the installed-Codex session callback).

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"sync"
	"time"

	"codexmarathon/controller/internal/runtime"
	"codexmarathon/controller/internal/transitions"
)

const (
	// ControllerProtocolVersion is the local TUI/controller protocol version.
	// It is deliberately separate from the controller/runtime protocol: the
	// controller remains the only authority allowed to deploy credentials.
	ControllerProtocolVersion = 1

	// ControllerControlEnv names the optional endpoint hint inherited by a
	// Codex process launched by the companion.  A client may also derive the
	// default endpoint from the controller state directory.
	ControllerControlEnv = "CODEXMARATHON_CONTROL"

	ControllerMethodNegotiate = "protocol/negotiate"
	ControllerMethodStatus    = "marathon/status"
	ControllerMethodSwitch    = "marathon/switch"

	defaultControlMaxLineBytes = 4 << 20
	defaultControlSwitchWait   = 2 * time.Minute
)

var (
	ErrControlNotStarted     = errors.New("controller control server is not started")
	ErrControlAlreadyStarted = errors.New("controller control server is already started")
	ErrControlProtocol       = errors.New("invalid controller control protocol message")
	ErrControlMethod         = errors.New("unknown controller control method")
	ErrControlNotNegotiated  = errors.New("controller control protocol is not negotiated")
	ErrControlTargetRequired = errors.New("account ID or alias is required")
	ErrControlAlreadyRunning = errors.New("controller control endpoint is already in use")
	ErrUnsafeControlEndpoint = errors.New("controller control endpoint is not user-scoped")
	ErrControlUnsupportedOS  = errors.New("controller control endpoint is unsupported on this platform")
)

// DefaultControllerControlEndpoint returns the endpoint used by the
// companion-owned control server. It lives beside the runtime socket but has
// a separate filename and authority.
func DefaultControllerControlEndpoint(stateDir string) (string, error) {
	if strings.TrimSpace(stateDir) == "" {
		return "", errors.New("state directory is empty")
	}
	stateDir, err := filepath.Abs(stateDir)
	if err != nil {
		return "", fmt.Errorf("resolve state directory: %w", err)
	}
	if goruntime.GOOS == "windows" {
		return "pipe://./pipe/" + userScopedPipeName() + "-control", nil
	}
	return "unix://" + filepath.Join(stateDir, "controller.sock"), nil
}

// ValidateControllerControlEndpoint keeps the controller listener inside the
// same user-owned boundary as the runtime listener. TCP is rejected because a
// TUI request can initiate credential deployment.
func ValidateControllerControlEndpoint(endpoint, stateDir string) error {
	if strings.TrimSpace(endpoint) == "" {
		return fmt.Errorf("%w: endpoint is empty", ErrUnsafeControlEndpoint)
	}
	if goruntime.GOOS == "windows" {
		parsed, err := url.Parse(endpoint)
		if err != nil || (strings.ToLower(parsed.Scheme) != "pipe" && strings.ToLower(parsed.Scheme) != "npipe" && strings.ToLower(parsed.Scheme) != "namedpipe") {
			return fmt.Errorf("%w: controller endpoint must be a named pipe", ErrUnsafeControlEndpoint)
		}
		expected := `\\.\pipe\` + userScopedPipeName() + "-control"
		if namedPipePath(parsed) != expected {
			return fmt.Errorf("%w: named pipe must use the current user's controller endpoint", ErrUnsafeControlEndpoint)
		}
		return nil
	}
	if err := ValidateManagedEndpoint(endpoint, stateDir); err != nil {
		return fmt.Errorf("%w: %v", ErrUnsafeControlEndpoint, err)
	}
	return nil
}

// ControlSwitchResult is a secret-free result suitable for a TUI status
// surface.  Mode is "runtime", "live-reload", or "restart-resume".
// TransitionID is present for the Marathon runtime coordinator path and lets
// a caller reconcile an uncertain result later.
type ControlSwitchResult struct {
	AccountID          string `json:"account_id"`
	Alias              string `json:"alias,omitempty"`
	Outcome            string `json:"outcome"`
	Mode               string `json:"mode,omitempty"`
	TransitionID       string `json:"transition_id,omitempty"`
	RuntimeID          string `json:"runtime_id,omitempty"`
	ExpectedGeneration uint64 `json:"expected_generation,omitempty"`
	FinalGeneration    uint64 `json:"final_generation,omitempty"`
	IdentityVerified   bool   `json:"identity_verified"`
	Reason             string `json:"reason,omitempty"`
}

// ControlAccount is the non-secret account view exposed to the TUI.
type ControlAccount struct {
	AccountID         string     `json:"account_id"`
	Alias             string     `json:"alias,omitempty"`
	Active            bool       `json:"active"`
	CredentialPresent bool       `json:"credential_present"`
	CredentialHealth  string     `json:"credential_health,omitempty"`
	LastTelemetryAt   *time.Time `json:"last_telemetry_at,omitempty"`
	TelemetrySource   string     `json:"telemetry_source,omitempty"`
}

// ControlRuntimeStatus reports only runtime identity and transition metadata.
// It intentionally has no auth snapshot or provider token fields.
type ControlRuntimeStatus struct {
	Connected         bool                       `json:"connected"`
	RuntimeID         string                     `json:"runtime_id,omitempty"`
	AccountID         string                     `json:"account_id,omitempty"`
	AuthGeneration    uint64                     `json:"auth_generation,omitempty"`
	ActiveTurnCount   int                        `json:"active_turn_count"`
	PendingTransition *runtime.PendingTransition `json:"pending_transition,omitempty"`
}

// ControlStatus is the status response consumed by a future /marathon UI.
// Runtime is nil when the controller is running in installed-Codex mode and
// no Marathon runtime protocol peer is attached.
type ControlStatus struct {
	ProtocolVersion  int                   `json:"protocol_version"`
	Endpoint         string                `json:"endpoint"`
	RuntimeConnected bool                  `json:"runtime_connected"`
	ActiveAccountID  string                `json:"active_account_id,omitempty"`
	Accounts         []ControlAccount      `json:"accounts"`
	Runtime          *ControlRuntimeStatus `json:"runtime,omitempty"`
	Transition       *transitions.State    `json:"transition,omitempty"`
}

// ControlSwitchHandler lets the installed-Codex companion reuse the same
// local API while retaining its process ownership and restart/resume logic.
// The handler receives a canonical account ID, never an untrusted alias.
type ControlSwitchHandler func(context.Context, string) (ControlSwitchResult, error)

// ControlStatusHandler can fill the runtime portion of status for an
// installed app-server session.  It is optional; the Marathon runtime path
// derives status from Controller.RuntimeState automatically.
type ControlStatusHandler func(context.Context) (*ControlRuntimeStatus, error)

// ControllerControlServerConfig configures the local TUI/controller server.
type ControllerControlServerConfig struct {
	Endpoint     string
	Switch       ControlSwitchHandler
	Status       ControlStatusHandler
	MaxLineBytes int
}

// ControllerControlServer is a user-scoped newline-delimited JSON-RPC
// endpoint.  The listener is intentionally separate from the runtime socket:
// only this server can resolve aliases and initiate a controller transition.
type ControllerControlServer struct {
	controller *Controller
	config     ControllerControlServerConfig
	endpoint   string

	mu        sync.Mutex
	listener  net.Listener
	cancel    context.CancelFunc
	done      chan struct{}
	closeOnce sync.Once
	connWG    sync.WaitGroup
	closeErr  error
}

// NewControllerControlServer validates and composes a server without opening
// a socket.  Start owns the listener side effect.
func NewControllerControlServer(controller *Controller, config ControllerControlServerConfig) (*ControllerControlServer, error) {
	if controller == nil {
		return nil, errors.New("controller is nil")
	}
	if strings.TrimSpace(config.Endpoint) == "" {
		endpoint, err := DefaultControllerControlEndpoint(controller.Config().StateDir)
		if err != nil {
			return nil, err
		}
		config.Endpoint = endpoint
	}
	if err := ValidateControllerControlEndpoint(config.Endpoint, controller.Config().StateDir); err != nil {
		return nil, err
	}
	if config.MaxLineBytes <= 0 {
		config.MaxLineBytes = defaultControlMaxLineBytes
	}
	return &ControllerControlServer{
		controller: controller,
		config:     config,
		endpoint:   config.Endpoint,
	}, nil
}

// Endpoint returns the configured user-scoped endpoint.
func (s *ControllerControlServer) Endpoint() string {
	if s == nil {
		return ""
	}
	return s.endpoint
}

// Start opens the listener and starts accepting TUI requests.  Connections
// are handled independently, while the transition coordinator still rejects
// competing state-changing requests with its durable in-progress guard.
func (s *ControllerControlServer) Start(ctx context.Context) error {
	if s == nil {
		return errors.New("nil controller control server")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	if s.listener != nil {
		s.mu.Unlock()
		return ErrControlAlreadyStarted
	}
	listener, socketPath, err := listenControllerControlEndpoint(s.endpoint, s.controller.Config().StateDir)
	if err != nil {
		s.mu.Unlock()
		return err
	}
	serverCtx, cancel := context.WithCancel(ctx)
	s.listener = listener
	s.cancel = cancel
	s.done = make(chan struct{})
	s.mu.Unlock()

	go s.acceptLoop(serverCtx, socketPath)
	return nil
}

// Close stops the listener and waits for in-flight requests.  It is safe to
// call more than once and removes only the socket created by this server.
func (s *ControllerControlServer) Close() error {
	if s == nil {
		return nil
	}
	s.closeOnce.Do(func() {
		s.mu.Lock()
		listener := s.listener
		cancel := s.cancel
		done := s.done
		s.mu.Unlock()
		if cancel != nil {
			cancel()
		}
		if listener != nil {
			_ = listener.Close()
		}
		if done != nil {
			<-done
		}
		s.connWG.Wait()
		s.mu.Lock()
		s.closeErr = removeControllerControlEndpoint(s.endpoint)
		s.mu.Unlock()
	})
	s.mu.Lock()
	err := s.closeErr
	s.mu.Unlock()
	return err
}

func (s *ControllerControlServer) acceptLoop(ctx context.Context, socketPath string) {
	defer func() {
		s.mu.Lock()
		done := s.done
		s.mu.Unlock()
		if done != nil {
			close(done)
		}
		_ = socketPath // retained for platform-specific listener cleanup.
	}()
	s.mu.Lock()
	listener := s.listener
	s.mu.Unlock()
	for {
		conn, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return
			}
			continue
		}
		s.connWG.Add(1)
		go func() {
			defer s.connWG.Done()
			_ = s.serveConn(ctx, conn)
		}()
	}
}

func (s *ControllerControlServer) serveConn(ctx context.Context, conn net.Conn) error {
	defer conn.Close()
	reader := bufio.NewReader(conn)
	negotiated := false
	for {
		line, err := readControlLine(reader, s.config.MaxLineBytes)
		if len(line) > 0 {
			response := s.handleLine(ctx, line, &negotiated)
			if _, writeErr := conn.Write(response); writeErr != nil {
				return writeErr
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
	}
}

func readControlLine(reader *bufio.Reader, maxBytes int) ([]byte, error) {
	line, err := reader.ReadBytes('\n')
	if len(line) > maxBytes {
		return nil, fmt.Errorf("control frame exceeds %d bytes", maxBytes)
	}
	return bytesTrimSpace(line), err
}

// bytesTrimSpace avoids retaining a bufio.Reader buffer in the request and
// accepts the final JSON object without requiring a trailing newline.
func bytesTrimSpace(value []byte) []byte {
	return []byte(strings.TrimSpace(string(value)))
}

type controlRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type controlResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *controlError   `json:"error,omitempty"`
}

type controlError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

type controlNegotiateParams struct {
	SupportedVersions []int `json:"supported_versions"`
}

type controlNegotiateResult struct {
	ProtocolVersion int   `json:"protocol_version"`
	ServerVersions  []int `json:"server_versions"`
}

type controlSwitchParams struct {
	Target    string `json:"target"`
	TimeoutMS int    `json:"timeout_ms,omitempty"`
}

func (s *ControllerControlServer) handleLine(ctx context.Context, line []byte, negotiated *bool) []byte {
	request := controlRequest{}
	if err := json.Unmarshal(line, &request); err != nil {
		return marshalControlResponse(nil, nil, &controlError{Code: -32700, Message: ErrControlProtocol.Error()})
	}
	if request.JSONRPC != "2.0" || len(request.ID) == 0 || string(request.ID) == "null" || strings.TrimSpace(request.Method) == "" {
		return marshalControlResponse(request.ID, nil, &controlError{Code: -32600, Message: ErrControlProtocol.Error()})
	}
	if request.Method == ControllerMethodNegotiate {
		var params controlNegotiateParams
		if err := json.Unmarshal(request.Params, &params); err != nil || !containsVersion(params.SupportedVersions, ControllerProtocolVersion) {
			return marshalControlResponse(request.ID, nil, &controlError{Code: -32602, Message: "supported_versions must include controller protocol v1"})
		}
		*negotiated = true
		return marshalControlResponse(request.ID, controlNegotiateResult{ProtocolVersion: ControllerProtocolVersion, ServerVersions: []int{ControllerProtocolVersion}}, nil)
	}
	if !*negotiated {
		return marshalControlResponse(request.ID, nil, &controlError{Code: -32001, Message: ErrControlNotNegotiated.Error()})
	}

	switch request.Method {
	case ControllerMethodStatus:
		status, err := s.status(ctx)
		if err != nil {
			return marshalControlResponse(request.ID, nil, controlServerError(err))
		}
		return marshalControlResponse(request.ID, status, nil)
	case ControllerMethodSwitch:
		result, err := s.switchAccount(ctx, request.Params)
		if err != nil {
			return marshalControlResponse(request.ID, nil, controlServerError(err))
		}
		return marshalControlResponse(request.ID, result, nil)
	default:
		return marshalControlResponse(request.ID, nil, &controlError{Code: -32601, Message: ErrControlMethod.Error()})
	}
}

func controlServerError(err error) *controlError {
	if err == nil {
		return nil
	}
	return &controlError{Code: -32000, Message: err.Error()}
}

func marshalControlResponse(id json.RawMessage, result any, err *controlError) []byte {
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	response, marshalErr := json.Marshal(controlResponse{JSONRPC: "2.0", ID: id, Result: result, Error: err})
	if marshalErr != nil {
		response = []byte(`{"jsonrpc":"2.0","id":null,"error":{"code":-32000,"message":"failed to encode controller response"}}`)
	}
	return append(response, '\n')
}

func containsVersion(versions []int, want int) bool {
	for _, version := range versions {
		if version == want {
			return true
		}
	}
	return false
}

func (s *ControllerControlServer) status(ctx context.Context) (ControlStatus, error) {
	if s == nil || s.controller == nil {
		return ControlStatus{}, errors.New("controller is unavailable")
	}
	profiles, err := s.controller.AccountManager().List()
	if err != nil {
		return ControlStatus{}, err
	}
	activeID, err := s.controller.Registry().ActiveID()
	if err != nil {
		return ControlStatus{}, err
	}
	status := ControlStatus{
		ProtocolVersion:  ControllerProtocolVersion,
		Endpoint:         s.endpoint,
		RuntimeConnected: s.controller.RuntimeConnected(),
		ActiveAccountID:  activeID,
		Accounts:         make([]ControlAccount, 0, len(profiles)),
	}
	for _, profile := range profiles {
		status.Accounts = append(status.Accounts, ControlAccount{
			AccountID:         profile.AccountID,
			Alias:             profile.Alias,
			Active:            profile.Active,
			CredentialPresent: profile.CredentialPresent,
			CredentialHealth:  string(profile.CredentialHealth),
			LastTelemetryAt:   profile.LastTelemetryAt,
			TelemetrySource:   profile.TelemetrySource,
		})
	}
	if s.config.Status != nil {
		runtimeStatus, statusErr := s.config.Status(ctx)
		if statusErr != nil {
			return ControlStatus{}, statusErr
		}
		status.Runtime = runtimeStatus
	} else if s.controller.RuntimeConnected() {
		state, stateErr := s.controller.RuntimeState(ctx)
		if stateErr != nil {
			return ControlStatus{}, stateErr
		}
		runtimeStatus := &ControlRuntimeStatus{
			Connected:         true,
			RuntimeID:         state.Identity.RuntimeID,
			AccountID:         optionalAccountID(state.Identity.AccountID),
			AuthGeneration:    state.Identity.AuthGeneration,
			ActiveTurnCount:   state.ActiveTurnCount,
			PendingTransition: state.PendingTransition,
		}
		status.Runtime = runtimeStatus
	}
	if state, ok := s.controller.Coordinator().State(); ok {
		status.Transition = &state
	}
	return status, nil
}

func (s *ControllerControlServer) switchAccount(parent context.Context, raw json.RawMessage) (ControlSwitchResult, error) {
	params := controlSwitchParams{}
	if len(raw) != 0 && string(raw) != "null" {
		if err := json.Unmarshal(raw, &params); err != nil {
			return ControlSwitchResult{}, fmt.Errorf("decode switch params: %w", err)
		}
	}
	target := strings.TrimSpace(params.Target)
	if target == "" {
		return ControlSwitchResult{}, ErrControlTargetRequired
	}
	account, err := s.resolveAccountTarget(target)
	if err != nil {
		return ControlSwitchResult{}, err
	}
	handler := s.config.Switch
	if handler == nil {
		handler = func(ctx context.Context, accountID string) (ControlSwitchResult, error) {
			transition, requestErr := s.controller.RequestTransition(ctx, accountID)
			if transition == nil {
				return ControlSwitchResult{}, requestErr
			}
			result := ControlSwitchResult{
				AccountID:          accountID,
				Outcome:            string(transition.Outcome),
				Mode:               "runtime",
				TransitionID:       transition.TransitionID,
				RuntimeID:          transition.RuntimeID,
				ExpectedGeneration: transition.ExpectedGeneration,
				FinalGeneration:    transition.FinalGeneration,
				IdentityVerified:   transition.Outcome == transitions.TransitionCommitted,
				Reason:             transition.Reason,
			}
			if requestErr != nil && transition.Outcome != transitions.TransitionUncertain && transition.Outcome != transitions.TransitionRejected {
				return result, requestErr
			}
			return result, nil
		}
	}
	requestCtx := parent
	if requestCtx == nil {
		requestCtx = context.Background()
	}
	wait := defaultControlSwitchWait
	if params.TimeoutMS > 0 {
		wait = time.Duration(params.TimeoutMS) * time.Millisecond
	}
	if wait <= 0 {
		return ControlSwitchResult{}, errors.New("switch timeout must be positive")
	}
	requestCtx, cancel := context.WithTimeout(requestCtx, wait)
	defer cancel()
	result, err := handler(requestCtx, account.AccountID)
	if result.AccountID == "" {
		result.AccountID = account.AccountID
	}
	result.Alias = account.Alias
	return result, err
}

func (s *ControllerControlServer) resolveAccountTarget(target string) (ControlAccount, error) {
	profiles, err := s.controller.AccountManager().List()
	if err != nil {
		return ControlAccount{}, err
	}
	var match *ControlAccount
	for _, profile := range profiles {
		candidate := ControlAccount{
			AccountID:         profile.AccountID,
			Alias:             profile.Alias,
			Active:            profile.Active,
			CredentialPresent: profile.CredentialPresent,
			CredentialHealth:  string(profile.CredentialHealth),
			LastTelemetryAt:   profile.LastTelemetryAt,
			TelemetrySource:   profile.TelemetrySource,
		}
		if profile.AccountID == target {
			return candidate, nil
		}
		if profile.Alias == target {
			if match != nil {
				return ControlAccount{}, fmt.Errorf("account alias %q is ambiguous", target)
			}
			copy := candidate
			match = &copy
		}
	}
	if match == nil {
		return ControlAccount{}, fmt.Errorf("account %q was not found", target)
	}
	return *match, nil
}
