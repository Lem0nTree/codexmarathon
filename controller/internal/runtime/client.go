package runtime

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

const (
	defaultEventBuffer   = 32
	defaultErrorBuffer   = 4
	defaultMaxLineBytes  = 4 << 20
)

var (
	// ErrClientClosed is returned when a call cannot be sent because the
	// transport has already been closed.
	ErrClientClosed = errors.New("runtime client is closed")
	// ErrInvalidMessage is reported for malformed or unsupported wire messages.
	ErrInvalidMessage = errors.New("invalid runtime protocol message")
)

// RPCError is the JSON-RPC error object returned by the runtime.
type RPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *RPCError) Error() string {
	if e == nil {
		return "runtime RPC error"
	}
	if e.Message == "" {
		return fmt.Sprintf("runtime RPC error (code %d)", e.Code)
	}
	return fmt.Sprintf("runtime RPC error (code %d): %s", e.Code, e.Message)
}

// ClientOption configures a Client.
type ClientOption func(*clientOptions)

type clientOptions struct {
	eventBuffer  int
	errorBuffer  int
	maxLineBytes int
}

// WithEventBuffer controls the number of decoded events buffered before the
// reader waits for the controller to consume them.
func WithEventBuffer(size int) ClientOption {
	return func(options *clientOptions) {
		if size > 0 {
			options.eventBuffer = size
		}
	}
}

// WithMaxLineBytes bounds one newline-delimited JSON message.
func WithMaxLineBytes(size int) ClientOption {
	return func(options *clientOptions) {
		if size > 0 {
			options.maxLineBytes = size
		}
	}
}

// WithErrorBuffer controls the number of asynchronous protocol errors kept
// for callers that inspect Errors.
func WithErrorBuffer(size int) ClientOption {
	return func(options *clientOptions) {
		if size > 0 {
			options.errorBuffer = size
		}
	}
}

// RequestID is the client-generated JSON-RPC identifier. It is a string on
// the wire, while responses with integer IDs are also accepted for peers that
// use the broader JSON-RPC identifier shape.
type RequestID string

type requestEnvelope struct {
	JSONRPC string `json:"jsonrpc"`
	ID      RequestID `json:"id"`
	Method  Method `json:"method"`
	Params  any `json:"params,omitempty"`
}

type notificationEnvelope struct {
	JSONRPC string `json:"jsonrpc"`
	Method  Method `json:"method"`
	Params  any `json:"params"`
}

type rawEnvelope struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
	Result  json.RawMessage `json:"result"`
	Error   *RPCError       `json:"error"`
}

type callResponse struct {
	result json.RawMessage
	err    error
}

// Client is a concurrent, newline-delimited JSON-RPC client for a Marathon
// runtime adapter. The transport must support both reads and writes (for
// example a Unix socket, named pipe, or net.Pipe in tests).
type Client struct {
	conn io.ReadWriteCloser

	writeMu sync.Mutex
	stateMu sync.Mutex
	channelMu sync.Mutex
	closed  bool
	pending map[RequestID]chan callResponse

	sequence atomic.Uint64
	done     chan struct{}
	closeOnce sync.Once
	events   chan Event
	errors   chan error

	maxLineBytes int
}

// NewClient starts a reader for conn. Ownership of conn transfers to Client;
// Close must be called by the owner when the session ends.
func NewClient(conn io.ReadWriteCloser, options ...ClientOption) *Client {
	if conn == nil {
		panic("runtime.NewClient: nil transport")
	}
	config := clientOptions{
		eventBuffer:  defaultEventBuffer,
		errorBuffer:  defaultErrorBuffer,
		maxLineBytes: defaultMaxLineBytes,
	}
	for _, option := range options {
		if option != nil {
			option(&config)
		}
	}
	client := &Client{
		conn:         conn,
		pending:      make(map[RequestID]chan callResponse),
		done:         make(chan struct{}),
		events:       make(chan Event, config.eventBuffer),
		errors:       make(chan error, config.errorBuffer),
		maxLineBytes: config.maxLineBytes,
	}
	go client.readLoop()
	return client
}

// Events returns decoded runtime notifications. The channel is closed after
// the transport terminates or Close is called.
func (c *Client) Events() <-chan Event {
	return c.events
}

// Errors returns asynchronous protocol or transport errors. A call-specific
// error is delivered by Call and is not duplicated here.
func (c *Client) Errors() <-chan error {
	return c.errors
}

// Done returns a channel that is closed when the runtime transport terminates
// or the client is explicitly closed.  Supervisors use this as a
// transport-liveness signal without consuming the event or error streams.
func (c *Client) Done() <-chan struct{} {
	if c == nil {
		return nil
	}
	return c.done
}

// Closed reports whether the client has observed a transport termination.
// It is intentionally a snapshot; callers that need notification should
// select on Done instead.
func (c *Client) Closed() bool {
	if c == nil {
		return true
	}
	c.stateMu.Lock()
	closed := c.closed
	c.stateMu.Unlock()
	return closed
}

// Close terminates the transport and completes outstanding calls with
// ErrClientClosed. It is safe to call more than once.
func (c *Client) Close() error {
	c.shutdown(ErrClientClosed)
	return nil
}

// Call sends a typed JSON-RPC request and decodes its result into out. A nil
// out is valid when the caller only needs the success/failure status.
func (c *Client) Call(ctx context.Context, method Method, params any, out any) error {
	if method == "" {
		return errors.New("runtime method is required")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	id := RequestID(strconv.FormatUint(c.sequence.Add(1), 10))
	responseCh := make(chan callResponse, 1)

	c.stateMu.Lock()
	if c.closed {
		c.stateMu.Unlock()
		return ErrClientClosed
	}
	c.pending[id] = responseCh
	c.stateMu.Unlock()

	request := requestEnvelope{
		JSONRPC: "2.0",
		ID:      id,
		Method:  method,
		Params:  params,
	}
	if err := c.write(request); err != nil {
		c.removePending(id)
		return err
	}

	select {
	case <-ctx.Done():
		c.removePending(id)
		return ctx.Err()
	case response := <-responseCh:
		if response.err != nil {
			return response.err
		}
		if out == nil || len(response.result) == 0 || string(response.result) == "null" {
			return nil
		}
		if err := json.Unmarshal(response.result, out); err != nil {
			return fmt.Errorf("decode %s result: %w", method, err)
		}
		return nil
	}
}

// Notify sends a JSON-RPC notification. Notifications are not correlated and
// therefore do not produce a response.
func (c *Client) Notify(ctx context.Context, method Method, params any) error {
	if method == "" {
		return errors.New("runtime method is required")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	return c.write(notificationEnvelope{
		JSONRPC: "2.0",
		Method:  method,
		Params:  params,
	})
}

// NegotiateVersion selects the highest locally supported version shared with
// the runtime. A result outside the offered set is rejected as a protocol
// error instead of being silently interpreted as v1.
func (c *Client) NegotiateVersion(ctx context.Context, supported []int) (VersionNegotiationResult, error) {
	params := VersionNegotiationParams{SupportedVersions: append([]int(nil), supported...)}
	if err := params.Validate(); err != nil {
		return VersionNegotiationResult{}, err
	}
	var result VersionNegotiationResult
	if err := c.Call(ctx, MethodNegotiateVersion, params, &result); err != nil {
		return VersionNegotiationResult{}, err
	}
	for _, version := range supported {
		if version == result.ProtocolVersion {
			return result, nil
		}
	}
	return VersionNegotiationResult{}, fmt.Errorf("runtime selected unsupported protocol version %d", result.ProtocolVersion)
}

// GetRuntimeState reads the state required for reconciliation.
func (c *Client) GetRuntimeState(ctx context.Context) (RuntimeState, error) {
	var result RuntimeState
	if err := c.Call(ctx, MethodGetRuntimeState, struct{}{}, &result); err != nil {
		return RuntimeState{}, err
	}
	return result, nil
}

// PrepareAuthTransition records transition intent in the runtime.
func (c *Client) PrepareAuthTransition(ctx context.Context, params AuthTransitionParams) (TransitionResult, error) {
	if err := params.Validate(); err != nil {
		return TransitionResult{}, err
	}
	var result TransitionResult
	if err := c.Call(ctx, MethodPrepareAuthTransition, params, &result); err != nil {
		return TransitionResult{}, err
	}
	return result, nil
}

// CommitAuthTransition asks the runtime to apply a prepared transition after
// the controller has atomically deployed the target auth snapshot.
func (c *Client) CommitAuthTransition(ctx context.Context, params AuthTransitionParams) (TransitionResult, error) {
	if err := params.Validate(); err != nil {
		return TransitionResult{}, err
	}
	var result TransitionResult
	if err := c.Call(ctx, MethodCommitAuthTransition, params, &result); err != nil {
		return TransitionResult{}, err
	}
	return result, nil
}

// CancelAuthTransition abandons a prepared transition at the expected
// generation.
func (c *Client) CancelAuthTransition(ctx context.Context, params CancelAuthTransitionParams) (TransitionResult, error) {
	if err := params.Validate(); err != nil {
		return TransitionResult{}, err
	}
	var result TransitionResult
	if err := c.Call(ctx, MethodCancelAuthTransition, params, &result); err != nil {
		return TransitionResult{}, err
	}
	return result, nil
}

// GetIdentity returns the identity currently held by Codext's AuthManager.
func (c *Client) GetIdentity(ctx context.Context) (Identity, error) {
	var result Identity
	if err := c.Call(ctx, MethodGetIdentity, struct{}{}, &result); err != nil {
		return Identity{}, err
	}
	return result, nil
}

// GetAuthGeneration returns the runtime's monotonic identity generation.
func (c *Client) GetAuthGeneration(ctx context.Context) (AuthGenerationResult, error) {
	var result AuthGenerationResult
	if err := c.Call(ctx, MethodGetAuthGeneration, struct{}{}, &result); err != nil {
		return AuthGenerationResult{}, err
	}
	return result, nil
}

// ReadAuthSnapshot reads the active opaque snapshot held by Codex's native
// AuthManager. Callers must keep the result in memory only long enough to
// synchronize the protected account vault; it must not be logged or journalled.
func (c *Client) ReadAuthSnapshot(ctx context.Context) (NativeAuthSnapshotResult, error) {
	var result NativeAuthSnapshotResult
	if err := c.Call(ctx, MethodReadAuthSnapshot, struct{}{}, &result); err != nil {
		return NativeAuthSnapshotResult{}, err
	}
	if result.AccountID == "" {
		return NativeAuthSnapshotResult{}, errors.New("native auth snapshot has no account_id")
	}
	if err := validateRuntimeAccountID(result.AccountID); err != nil {
		return NativeAuthSnapshotResult{}, err
	}
	if len(result.AuthJSON) == 0 || string(result.AuthJSON) == "null" || string(result.AuthJSON) == "{}" {
		return NativeAuthSnapshotResult{}, errors.New("native auth snapshot has no auth_json")
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(result.AuthJSON, &object); err != nil || object == nil {
		return NativeAuthSnapshotResult{}, errors.New("native auth snapshot auth_json is not an object")
	}
	return result, nil
}

// ReleaseRecovery authorizes Codex's already-parked UsageLimitExceeded
// recovery turn after the controller has verified an identity-changing
// transition. The runtime remains the sole owner of the queued prompt,
// thread, and actual submission. The request is idempotent by recovery ID on
// a compliant runtime, so a lost response can be retried during reconciliation
// without duplicating the continuation.
func (c *Client) ReleaseRecovery(ctx context.Context, params RecoveryReleaseParams) (RecoveryReleaseResult, error) {
	if err := params.Validate(); err != nil {
		return RecoveryReleaseResult{}, err
	}
	var result RecoveryReleaseResult
	if err := c.Call(ctx, MethodReleaseRecovery, params, &result); err != nil {
		return RecoveryReleaseResult{}, err
	}
	if result.RecoveryID != params.RecoveryID {
		return RecoveryReleaseResult{}, fmt.Errorf("recovery release response id %q does not match %q", result.RecoveryID, params.RecoveryID)
	}
	if result.Outcome == "" {
		return RecoveryReleaseResult{}, errors.New("recovery release response has no outcome")
	}
	if result.ThreadID != "" && params.ThreadID != "" && result.ThreadID != params.ThreadID {
		return RecoveryReleaseResult{}, fmt.Errorf("recovery release response thread %q does not match %q", result.ThreadID, params.ThreadID)
	}
	return result, nil
}

// LoginAccount delegates browser/device/API login to the embedded Codex
// runtime. The returned auth snapshot is an opaque in-memory handoff; callers
// must persist it only in a protected vault and must not log it.
func (c *Client) LoginAccount(ctx context.Context, params NativeLoginParams) (NativeLoginResult, error) {
	if params.AccountID != "" {
		if err := validateRuntimeAccountID(params.AccountID); err != nil {
			return NativeLoginResult{}, err
		}
	}
	var result NativeLoginResult
	if err := c.Call(ctx, MethodLoginAccount, params, &result); err != nil {
		return NativeLoginResult{}, err
	}
	if result.AccountID == "" {
		return NativeLoginResult{}, errors.New("native login response has no account_id")
	}
	if len(result.AuthJSON) == 0 || string(result.AuthJSON) == "null" {
		return NativeLoginResult{}, errors.New("native login response has no auth_json")
	}
	return result, nil
}

// RefreshAccount delegates token refresh to the embedded Codex login runtime.
// The opaque snapshot and returned tokens stay in memory until the controller
// performs validated write-back.
func (c *Client) RefreshAccount(ctx context.Context, params NativeRefreshParams) (NativeRefreshResult, error) {
	if params.AccountID == "" {
		return NativeRefreshResult{}, errors.New("account_id is required")
	}
	if len(params.AuthJSON) == 0 || string(params.AuthJSON) == "null" {
		return NativeRefreshResult{}, errors.New("auth_json is required")
	}
	if err := validateRuntimeAccountID(params.AccountID); err != nil {
		return NativeRefreshResult{}, err
	}
	var result NativeRefreshResult
	if err := c.Call(ctx, MethodRefreshAccount, params, &result); err != nil {
		return NativeRefreshResult{}, err
	}
	if result.AccessToken == "" && result.IDToken == "" && result.RefreshToken == "" {
		return NativeRefreshResult{}, errors.New("native refresh response has no updated token fields")
	}
	if result.AccountID != "" && result.AccountID != params.AccountID {
		return NativeRefreshResult{}, fmt.Errorf("native refresh response account_id %q does not match %q", result.AccountID, params.AccountID)
	}
	return result, nil
}

func validateRuntimeAccountID(accountID string) error {
	if accountID == "" || accountID == "." || accountID == ".." {
		return errors.New("account_id is invalid")
	}
	for _, r := range accountID {
		if r < 0x20 || r == 0x7f {
			return errors.New("account_id contains control characters")
		}
	}
	if strings.ContainsAny(accountID, `/\\<>:\"|?*`) {
		return errors.New("account_id contains path characters")
	}
	return nil
}

func (c *Client) write(message any) error {
	payload, err := json.Marshal(message)
	if err != nil {
		return fmt.Errorf("encode runtime message: %w", err)
	}
	payload = append(payload, '\n')

	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	c.stateMu.Lock()
	closed := c.closed
	c.stateMu.Unlock()
	if closed {
		return ErrClientClosed
	}
	if _, err := c.conn.Write(payload); err != nil {
		wrapped := fmt.Errorf("write runtime message: %w", err)
		c.shutdown(wrapped)
		return wrapped
	}
	return nil
}

func (c *Client) readLoop() {
	scanner := bufio.NewScanner(c.conn)
	scanner.Buffer(make([]byte, 4096), c.maxLineBytes)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		if err := c.handleMessage(line); err != nil {
			c.reportError(err)
		}
	}
	if err := scanner.Err(); err != nil {
		c.shutdown(fmt.Errorf("read runtime message: %w", err))
		return
	}
	c.shutdown(io.EOF)
}

func (c *Client) handleMessage(line []byte) error {
	var envelope rawEnvelope
	if err := json.Unmarshal(line, &envelope); err != nil {
		return fmt.Errorf("%w: decode JSON: %v", ErrInvalidMessage, err)
	}
	if envelope.JSONRPC != "2.0" {
		return fmt.Errorf("%w: jsonrpc must be 2.0", ErrInvalidMessage)
	}
	if envelope.Method != "" {
		if len(envelope.ID) != 0 {
			return fmt.Errorf("%w: server requests are not supported", ErrInvalidMessage)
		}
		if Method(envelope.Method) != EventNotificationMethod {
			return fmt.Errorf("%w: unsupported notification method %q", ErrInvalidMessage, envelope.Method)
		}
		event, err := DecodeEvent(envelope.Params)
		if err != nil {
			return err
		}
		c.channelMu.Lock()
		defer c.channelMu.Unlock()
		select {
		case c.events <- event:
			return nil
		case <-c.done:
			return ErrClientClosed
		}
	}
	if len(envelope.ID) == 0 {
		return fmt.Errorf("%w: response id is missing", ErrInvalidMessage)
	}
	id, err := decodeRequestID(envelope.ID)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidMessage, err)
	}
	if envelope.Error != nil && len(envelope.Result) > 0 {
		return fmt.Errorf("%w: response has both result and error", ErrInvalidMessage)
	}
	if envelope.Error != nil && envelope.Error.Message == "" {
		return fmt.Errorf("%w: response error message is missing", ErrInvalidMessage)
	}
	if envelope.Error == nil && len(envelope.Result) == 0 {
		return fmt.Errorf("%w: response has neither result nor error", ErrInvalidMessage)
	}
	c.stateMu.Lock()
	responseCh := c.pending[id]
	if responseCh != nil {
		delete(c.pending, id)
	}
	c.stateMu.Unlock()
	if responseCh == nil {
		return fmt.Errorf("%w: response id %q is not pending", ErrInvalidMessage, id)
	}
	select {
	case responseCh <- callResponse{result: envelope.Result, err: envelope.Error}:
	case <-c.done:
	}
	return nil
}

func (c *Client) reportError(err error) {
	if err == nil {
		return
	}
	c.channelMu.Lock()
	defer c.channelMu.Unlock()
	c.stateMu.Lock()
	closed := c.closed
	c.stateMu.Unlock()
	if closed {
		return
	}
	select {
	case c.errors <- err:
	default:
	}
}

func (c *Client) removePending(id RequestID) {
	c.stateMu.Lock()
	delete(c.pending, id)
	c.stateMu.Unlock()
}

func (c *Client) shutdown(cause error) {
	if cause == nil {
		cause = ErrClientClosed
	}
	c.closeOnce.Do(func() {
		c.stateMu.Lock()
		c.closed = true
		pending := c.pending
		c.pending = make(map[RequestID]chan callResponse)
		c.stateMu.Unlock()
		close(c.done)
		_ = c.conn.Close()
		for _, responseCh := range pending {
			responseCh <- callResponse{err: cause}
		}
		c.channelMu.Lock()
		close(c.events)
		close(c.errors)
		c.channelMu.Unlock()
	})
}

func decodeRequestID(raw json.RawMessage) (RequestID, error) {
	trimmed := raw
	if len(trimmed) == 0 {
		return "", errors.New("response id is empty")
	}
	if trimmed[0] == '"' {
		var value string
		if err := json.Unmarshal(trimmed, &value); err != nil || value == "" {
			return "", errors.New("response id string is invalid")
		}
		return RequestID(value), nil
	}
	value, err := strconv.ParseInt(string(trimmed), 10, 64)
	if err != nil {
		return "", fmt.Errorf("response id must be a string or integer: %w", err)
	}
	return RequestID(strconv.FormatInt(value, 10)), nil
}
