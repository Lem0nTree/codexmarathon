package runtime

// This file contains the companion-runtime boundary.  CodexMarathon can be
// installed next to an existing Codex CLI; it must not start a second copy of
// the Codex runtime just to change authentication.  Recent Codex releases
// expose the app-server control socket, which is a private WebSocket carrying
// the normal app-server JSON protocol.  The small client below speaks that
// protocol directly so the controller does not need a Rust runtime or a
// third-party WebSocket dependency.

import (
	"context"
	"crypto/rand"
	"crypto/sha1" // #nosec G505 -- SHA-1 is required by RFC 6455's accept key.
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	defaultInstalledProbeTimeout = 3 * time.Second
	defaultInstalledMessageLimit = 64 << 20
	websocketGUID                = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"
)

var (
	// ErrInstalledRuntimeNotFound means no usable Codex executable was found.
	ErrInstalledRuntimeNotFound = errors.New("installed Codex CLI was not found")
	// ErrAppServerUnavailable means the CLI is installed but its app-server
	// control socket is not currently serving requests.
	ErrAppServerUnavailable = errors.New("installed Codex app-server is unavailable")
	// ErrCapabilityUnsupported is returned when an installed runtime does not
	// implement a requested app-server method.
	ErrCapabilityUnsupported = errors.New("installed Codex runtime capability is unsupported")
	// ErrActiveTurn prevents a caller from treating a no-op auth reload as a
	// successful identity transition while a model turn is still running.
	ErrActiveTurn = errors.New("Codex has an active turn")
	// ErrInstalledClientClosed reports use of a disconnected app-server client.
	ErrInstalledClientClosed = errors.New("installed Codex app-server client is closed")
)

// InstalledCapability names a feature discovered on the installed Codex
// executable or confirmed by an app-server request.
type InstalledCapability string

const (
	CapabilityAppServer       InstalledCapability = "app-server"
	CapabilityAppServerDaemon InstalledCapability = "app-server-daemon"
	CapabilityAppServerProxy  InstalledCapability = "app-server-proxy"
	CapabilityControlSocket   InstalledCapability = "control-socket"
	CapabilityAuthReload      InstalledCapability = "auth-reload"
	CapabilityThreadResume    InstalledCapability = "thread-resume"
	CapabilityRateLimits      InstalledCapability = "rate-limits"
	CapabilityTurnEvents      InstalledCapability = "turn-events"
)

// Installation describes an existing Codex CLI without starting it.  Paths
// and version metadata are safe to display; no credential or environment
// value is retained in this type.
type Installation struct {
	Executable    string                `json:"executable"`
	Version       string                `json:"version,omitempty"`
	CodexHome     string                `json:"codex_home"`
	ControlSocket string                `json:"control_socket"`
	Capabilities  []InstalledCapability `json:"capabilities,omitempty"`
}

// HasCapability reports whether discovery or a live probe confirmed name.
func (i Installation) HasCapability(name InstalledCapability) bool {
	for _, capability := range i.Capabilities {
		if capability == name {
			return true
		}
	}
	return false
}

// ControlEndpoint returns the local WebSocket endpoint accepted by
// ConnectInstalledEndpoint. It is useful for diagnostics and avoids callers
// duplicating the CodexHome socket layout.
func (i Installation) ControlEndpoint() string {
	if strings.TrimSpace(i.ControlSocket) == "" {
		return ""
	}
	return "unix://" + filepath.ToSlash(i.ControlSocket)
}

// DiscoveryOptions customizes installed CLI discovery.  Empty fields follow
// the user's normal environment (CODEX_BIN, PATH, CODEX_HOME, and HOME).
type DiscoveryOptions struct {
	Executable   string
	CodexHome    string
	Env          []string
	ProbeTimeout time.Duration
}

// DiscoverInstalled locates the user's Codex CLI and determines whether it
// has the standard app-server and local-control commands.  Discovery is
// side-effect free: it runs only --version/--help and never edits auth.json.
func DiscoverInstalled(ctx context.Context, options DiscoveryOptions) (Installation, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if options.ProbeTimeout <= 0 {
		options.ProbeTimeout = defaultInstalledProbeTimeout
	}

	executable, err := resolveCodexExecutable(options.Executable, options.Env)
	if err != nil {
		return Installation{}, err
	}
	codexHome, err := resolveCodexHome(options.CodexHome, options.Env)
	if err != nil {
		return Installation{}, err
	}

	versionOutput, err := runInstalledCommand(ctx, options.ProbeTimeout, executable, options.Env, "--version")
	if err != nil {
		return Installation{}, fmt.Errorf("%w: run %q --version: %v", ErrInstalledRuntimeNotFound, executable, err)
	}
	version := parseCodexVersion(string(versionOutput))

	capabilities := make([]InstalledCapability, 0, 5)
	appServerHelp, appServerOK := runInstalledHelp(ctx, options, executable, "app-server", "--help")
	if appServerOK || looksLikeCodexAppServerHelp(appServerHelp) {
		capabilities = append(capabilities, CapabilityAppServer)
	}
	if daemonHelp, ok := runInstalledHelp(ctx, options, executable, "app-server", "daemon", "--help"); ok || looksLikeSubcommandHelp(daemonHelp, "daemon") {
		capabilities = append(capabilities, CapabilityAppServerDaemon)
	}
	if proxyHelp, ok := runInstalledHelp(ctx, options, executable, "app-server", "proxy", "--help"); ok || looksLikeSubcommandHelp(proxyHelp, "proxy") {
		capabilities = append(capabilities, CapabilityAppServerProxy)
	}

	socket := appServerControlSocketPath(codexHome)
	if fileInfo, statErr := os.Stat(socket); statErr == nil && !fileInfo.IsDir() && (fileInfo.Mode()&os.ModeSocket != 0 || os.PathSeparator == '\\') {
		capabilities = append(capabilities, CapabilityControlSocket)
	}
	if hasCapability(capabilities, CapabilityAppServer) {
		// These methods are part of the standard app-server protocol.  A live
		// connection downgrades them if a very old server returns method-not-found.
		capabilities = append(capabilities, CapabilityAuthReload, CapabilityThreadResume, CapabilityRateLimits, CapabilityTurnEvents)
	}

	return Installation{
		Executable:    executable,
		Version:       version,
		CodexHome:     codexHome,
		ControlSocket: socket,
		Capabilities:  uniqueCapabilities(capabilities),
	}, nil
}

func resolveCodexExecutable(explicit string, envs ...[]string) (string, error) {
	var env []string
	if len(envs) > 0 {
		env = envs[0]
	}
	if strings.TrimSpace(explicit) == "" {
		explicit = strings.TrimSpace(environmentValue(env, "CODEX_BIN"))
		if explicit == "" {
			explicit = strings.TrimSpace(os.Getenv("CODEX_BIN"))
		}
	}
	if explicit == "" {
		path, err := exec.LookPath("codex")
		if err != nil {
			return "", fmt.Errorf("%w: %v", ErrInstalledRuntimeNotFound, err)
		}
		return path, nil
	}
	path, err := exec.LookPath(explicit)
	if err != nil {
		// LookPath does not resolve a relative path containing a separator on
		// every platform; try an absolute path after the normal lookup.
		absolute, absErr := filepath.Abs(explicit)
		if absErr != nil {
			return "", fmt.Errorf("%w: %v", ErrInstalledRuntimeNotFound, err)
		}
		info, statErr := os.Stat(absolute)
		if statErr != nil {
			return "", fmt.Errorf("%w: %v", ErrInstalledRuntimeNotFound, err)
		}
		if info.IsDir() {
			return "", fmt.Errorf("%w: %q is a directory", ErrInstalledRuntimeNotFound, absolute)
		}
		path = absolute
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("%w: resolve %q: %v", ErrInstalledRuntimeNotFound, path, err)
	}
	return filepath.Clean(absolute), nil
}

func resolveCodexHome(explicit string, envs ...[]string) (string, error) {
	var env []string
	if len(envs) > 0 {
		env = envs[0]
	}
	if strings.TrimSpace(explicit) == "" {
		explicit = strings.TrimSpace(environmentValue(env, "CODEX_HOME"))
		if explicit == "" {
			explicit = strings.TrimSpace(os.Getenv("CODEX_HOME"))
		}
	}
	if explicit == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve Codex home: %w", err)
		}
		explicit = filepath.Join(home, ".codex")
	}
	abs, err := filepath.Abs(explicit)
	if err != nil {
		return "", fmt.Errorf("resolve Codex home: %w", err)
	}
	return filepath.Clean(abs), nil
}

func environmentValue(env []string, key string) string {
	for index := len(env) - 1; index >= 0; index-- {
		name, value, ok := strings.Cut(env[index], "=")
		if ok && name == key {
			return value
		}
	}
	return ""
}

func appServerControlSocketPath(codexHome string) string {
	return filepath.Join(codexHome, "app-server-control", "app-server-control.sock")
}

func runInstalledHelp(ctx context.Context, options DiscoveryOptions, executable string, args ...string) ([]byte, bool) {
	output, err := runInstalledCommand(ctx, options.ProbeTimeout, executable, options.Env, args...)
	return output, err == nil
}

func runInstalledCommand(parent context.Context, timeout time.Duration, executable string, env []string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	command := exec.CommandContext(ctx, executable, args...)
	if len(env) > 0 {
		command.Env = append(os.Environ(), env...)
	}
	output, err := command.CombinedOutput()
	if err != nil {
		// Some wrappers print usage to stderr before returning a non-zero
		// status. Keep that output for capability detection while still
		// reporting the failed command to callers.
		return output, err
	}
	return output, nil
}

var codexVersionPattern = regexp.MustCompile(`(?i)(?:^|[^0-9])v?([0-9]+\.[0-9]+\.[0-9]+(?:[-+][0-9A-Za-z.-]+)?)`)

func parseCodexVersion(output string) string {
	match := codexVersionPattern.FindStringSubmatch(output)
	if len(match) > 1 {
		return match[1]
	}
	line := strings.TrimSpace(strings.SplitN(output, "\n", 2)[0])
	return line
}

func looksLikeCodexAppServerHelp(output []byte) bool {
	value := strings.ToLower(string(output))
	return strings.Contains(value, "app-server") || strings.Contains(value, "app server")
}

func looksLikeSubcommandHelp(output []byte, command string) bool {
	value := strings.ToLower(string(output))
	return strings.Contains(value, command)
}

func hasCapability(capabilities []InstalledCapability, wanted InstalledCapability) bool {
	for _, capability := range capabilities {
		if capability == wanted {
			return true
		}
	}
	return false
}

func uniqueCapabilities(capabilities []InstalledCapability) []InstalledCapability {
	seen := make(map[InstalledCapability]struct{}, len(capabilities))
	result := make([]InstalledCapability, 0, len(capabilities))
	for _, capability := range capabilities {
		if _, ok := seen[capability]; ok {
			continue
		}
		seen[capability] = struct{}{}
		result = append(result, capability)
	}
	return result
}

// AppServerInitializeResult is the standard Codex app-server handshake
// response.  It intentionally uses strings for paths to keep the controller
// independent of Rust's AbsolutePathBuf representation.
type AppServerInitializeResult struct {
	UserAgent      string `json:"userAgent"`
	CodexHome      string `json:"codexHome"`
	PlatformFamily string `json:"platformFamily"`
	PlatformOS     string `json:"platformOs"`
}

// InstalledNotification is one standard app-server notification.  Params are
// retained as JSON because the companion only needs a few stable fields and
// must tolerate new Codex notification shapes.
type InstalledNotification struct {
	Method string
	Params json.RawMessage
}

// InstalledClient is a serialized request client for one existing Codex
// app-server connection.  Calls are serialized because the control socket is
// primarily an operator channel; notifications are still retained and
// delivered while requests wait for their response.
type InstalledClient struct {
	conn net.Conn

	callMu                sync.Mutex // serializes callers while preserving request order
	writeMu               sync.Mutex // protects WebSocket writes (requests and pong frames)
	pendingMu             sync.Mutex // owns pending responses consumed by the reader
	stateMu               sync.RWMutex
	closed                bool
	readerStarted         bool
	closeOnce             sync.Once
	readerOnce            sync.Once
	notificationCloseOnce sync.Once
	sequence              atomic.Uint64

	maxMessageBytes int
	info            AppServerInitializeResult
	version         string
	capabilities    map[InstalledCapability]bool
	activeTurns     int
	notifications   chan InstalledNotification
	pending         map[uint64]chan installedResponse
}

type installedResponse struct {
	result   json.RawMessage
	rpcError *AppServerRPCError
	err      error
}

// ConnectInstalled attaches to an already-running Codex app-server control
// socket and performs the standard initialize/initialized handshake.
func ConnectInstalled(ctx context.Context, installation Installation) (*InstalledClient, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if strings.TrimSpace(installation.ControlSocket) == "" {
		if installation.CodexHome == "" {
			return nil, fmt.Errorf("%w: control socket path is empty", ErrAppServerUnavailable)
		}
		installation.ControlSocket = appServerControlSocketPath(installation.CodexHome)
	}
	conn, err := dialInstalledEndpoint(ctx, installation.ControlSocket)
	if err != nil {
		return nil, fmt.Errorf("%w at %q: %v", ErrAppServerUnavailable, installation.ControlSocket, err)
	}
	client, err := newInstalledClient(conn)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	if err := client.initialize(ctx); err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("initialize installed Codex app-server: %w", err)
	}
	return client, nil
}

// ConnectInstalledEndpoint is useful for tests and local operators that have
// an explicit Unix or loopback WebSocket endpoint.  Network endpoints are
// restricted to loopback; a companion must never silently create an
// unauthenticated network control channel.
func ConnectInstalledEndpoint(ctx context.Context, endpoint string) (*InstalledClient, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	conn, err := dialInstalledEndpoint(ctx, endpoint)
	if err != nil {
		return nil, err
	}
	client, err := newInstalledClient(conn)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	if err := client.initialize(ctx); err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("initialize installed Codex app-server: %w", err)
	}
	return client, nil
}

func dialInstalledEndpoint(ctx context.Context, endpoint string) (net.Conn, error) {
	endpoint = strings.TrimSpace(endpoint)
	if endpoint == "" {
		return nil, fmt.Errorf("%w: endpoint is empty", ErrAppServerUnavailable)
	}
	if !strings.Contains(endpoint, "://") {
		endpoint = "unix://" + endpoint
	}
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return nil, fmt.Errorf("%w: invalid endpoint: %v", ErrAppServerUnavailable, err)
	}
	scheme := strings.ToLower(parsed.Scheme)
	var network, address string
	switch scheme {
	case "unix":
		if parsed.Host != "" || parsed.Path == "" {
			return nil, fmt.Errorf("%w: unix endpoint must contain an absolute path", ErrAppServerUnavailable)
		}
		address = filepath.Clean(filepath.FromSlash(parsed.Path))
		if !filepath.IsAbs(address) {
			return nil, fmt.Errorf("%w: Unix control socket path must be absolute", ErrAppServerUnavailable)
		}
		network = "unix"
	case "ws", "http":
		if parsed.User != nil || parsed.Host == "" || parsed.Path == "" && parsed.RawPath == "" {
			return nil, fmt.Errorf("%w: invalid WebSocket endpoint", ErrAppServerUnavailable)
		}
		host, _, splitErr := net.SplitHostPort(parsed.Host)
		if splitErr != nil || !isLoopbackHost(host) {
			return nil, fmt.Errorf("%w: WebSocket endpoint must be loopback", ErrAppServerUnavailable)
		}
		if scheme == "http" {
			parsed.Scheme = "ws"
		}
		address = parsed.Host
		network = "tcp"
	default:
		return nil, fmt.Errorf("%w: unsupported endpoint scheme %q", ErrAppServerUnavailable, parsed.Scheme)
	}
	dialer := net.Dialer{}
	conn, err := dialer.DialContext(ctx, network, address)
	if err != nil {
		return nil, err
	}
	if err := websocketClientHandshake(conn, endpoint); err != nil {
		_ = conn.Close()
		return nil, err
	}
	return conn, nil
}

func isLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	parsed := net.ParseIP(host)
	return parsed != nil && parsed.IsLoopback()
}

func newInstalledClient(conn net.Conn) (*InstalledClient, error) {
	if conn == nil {
		return nil, errors.New("installed app-server transport is nil")
	}
	return &InstalledClient{
		conn:            conn,
		maxMessageBytes: defaultInstalledMessageLimit,
		capabilities:    make(map[InstalledCapability]bool),
		notifications:   make(chan InstalledNotification, 64),
		pending:         make(map[uint64]chan installedResponse),
	}, nil
}

func (c *InstalledClient) initialize(ctx context.Context) error {
	// ConnectInstalled calls this only after the HTTP upgrade. Keeping reader
	// startup here also permits tests and local adapters to construct a client,
	// perform their own upgrade, and then use the same production path.
	c.startReader()
	result, err := c.call(ctx, "initialize", map[string]any{
		"clientInfo": map[string]string{
			"name":    "codexmarathon",
			"title":   "CodexMarathon companion",
			"version": "0.1.0",
		},
		"capabilities": map[string]any{
			"experimentalApi": true,
		},
	})
	if err != nil {
		return err
	}
	var info AppServerInitializeResult
	if err := json.Unmarshal(result, &info); err != nil {
		return fmt.Errorf("decode app-server initialize response: %w", err)
	}
	if strings.TrimSpace(info.UserAgent) == "" {
		return errors.New("app-server initialize response has no userAgent")
	}
	c.stateMu.Lock()
	c.info = info
	c.version = parseCodexVersion(info.UserAgent)
	c.capabilities[CapabilityAppServer] = true
	c.capabilities[CapabilityAuthReload] = true
	c.capabilities[CapabilityThreadResume] = true
	c.capabilities[CapabilityRateLimits] = true
	c.capabilities[CapabilityTurnEvents] = true
	c.stateMu.Unlock()
	if err := c.notify(ctx, "initialized", nil); err != nil {
		return fmt.Errorf("send app-server initialized notification: %w", err)
	}
	return nil
}

// Info returns the metadata from the app-server handshake.
func (c *InstalledClient) Info() AppServerInitializeResult {
	if c == nil {
		return AppServerInitializeResult{}
	}
	c.stateMu.RLock()
	defer c.stateMu.RUnlock()
	return c.info
}

// Version returns the server-reported version, falling back to the user-agent
// string when an installation uses a non-semver build label.
func (c *InstalledClient) Version() string {
	if c == nil {
		return ""
	}
	c.stateMu.RLock()
	defer c.stateMu.RUnlock()
	return c.version
}

// Capabilities returns a copy of the live capability view. A caller can
// safely expose it in status output without racing a method probe.
func (c *InstalledClient) Capabilities() []InstalledCapability {
	if c == nil {
		return nil
	}
	c.stateMu.RLock()
	capabilities := make([]InstalledCapability, 0, len(c.capabilities))
	for capability, enabled := range c.capabilities {
		if enabled {
			capabilities = append(capabilities, capability)
		}
	}
	c.stateMu.RUnlock()
	sort.Slice(capabilities, func(left, right int) bool { return capabilities[left] < capabilities[right] })
	return capabilities
}

// HasCapability reports the current live capability view.
func (c *InstalledClient) HasCapability(name InstalledCapability) bool {
	if c == nil {
		return false
	}
	c.stateMu.RLock()
	value := c.capabilities[name]
	c.stateMu.RUnlock()
	return value
}

// ActiveTurnCount is maintained from standard turn lifecycle notifications.
// It is a conservative guard: when no notification stream is available the
// account/read request still asks the server's own turn guard to reload safely.
func (c *InstalledClient) ActiveTurnCount() int {
	if c == nil {
		return 0
	}
	c.stateMu.RLock()
	count := c.activeTurns
	c.stateMu.RUnlock()
	return count
}

// Notifications returns standard app-server notifications observed while
// servicing requests. The channel is closed by Close.
func (c *InstalledClient) Notifications() <-chan InstalledNotification {
	if c == nil {
		return nil
	}
	return c.notifications
}

// Close disconnects from the existing app-server. It does not stop Codex or
// modify any Codex files.
func (c *InstalledClient) Close() error {
	if c == nil {
		return nil
	}
	c.closeOnce.Do(func() {
		c.stateMu.Lock()
		c.closed = true
		readerStarted := c.readerStarted
		c.stateMu.Unlock()
		// Closing the underlying connection is deliberate. A peer may be
		// blocked in an active request and waiting for a close frame, so a
		// graceful frame here could deadlock the companion's shutdown path.
		_ = c.conn.Close()
		if !readerStarted {
			c.notificationCloseOnce.Do(func() { close(c.notifications) })
		}
	})
	return nil
}

// ReloadAuth asks the installed app-server to reload auth.json through its
// supported account/read interface. The app-server performs its own safe-turn
// check and invalidates account-bound transport caches when auth_changed is
// true; the companion never mutates the running process's memory directly.
func (c *InstalledClient) ReloadAuth(ctx context.Context) (AccountReadResult, error) {
	if c == nil {
		return AccountReadResult{}, ErrInstalledClientClosed
	}
	if c.ActiveTurnCount() > 0 {
		return AccountReadResult{}, ErrActiveTurn
	}
	result, err := c.call(ctx, "account/read", AccountReadParams{ReloadAuthFromStorage: true})
	if err != nil {
		if isUnsupportedRPCError(err) {
			c.setCapability(CapabilityAuthReload, false)
			return AccountReadResult{}, fmt.Errorf("%w: account/read reloadAuthFromStorage: %v", ErrCapabilityUnsupported, err)
		}
		return AccountReadResult{}, err
	}
	var account AccountReadResult
	if err := json.Unmarshal(result, &account); err != nil {
		return AccountReadResult{}, fmt.Errorf("decode account/read response: %w", err)
	}
	c.setCapability(CapabilityAuthReload, true)
	return account, nil
}

// ReadAccount reads account metadata without causing an auth reload.
func (c *InstalledClient) ReadAccount(ctx context.Context) (AccountReadResult, error) {
	result, err := c.call(ctx, "account/read", AccountReadParams{})
	if err != nil {
		return AccountReadResult{}, err
	}
	var account AccountReadResult
	if err := json.Unmarshal(result, &account); err != nil {
		return AccountReadResult{}, fmt.Errorf("decode account/read response: %w", err)
	}
	return account, nil
}

// ReadRateLimits obtains the standard active-account quota snapshot. It is
// returned as JSON to remain compatible with fields added by Codex releases.
func (c *InstalledClient) ReadRateLimits(ctx context.Context) (json.RawMessage, error) {
	result, err := c.call(ctx, "account/rateLimits/read", nil)
	if err != nil {
		if isUnsupportedRPCError(err) {
			c.setCapability(CapabilityRateLimits, false)
			return nil, fmt.Errorf("%w: account/rateLimits/read: %v", ErrCapabilityUnsupported, err)
		}
		return nil, err
	}
	c.setCapability(CapabilityRateLimits, true)
	return append(json.RawMessage(nil), result...), nil
}

// ResumeThread asks the standard app-server to load a persisted conversation
// after a controlled Codex process restart. It never creates a new prompt.
func (c *InstalledClient) ResumeThread(ctx context.Context, threadID string) (json.RawMessage, error) {
	if strings.TrimSpace(threadID) == "" {
		return nil, errors.New("thread id is required")
	}
	result, err := c.call(ctx, "thread/resume", map[string]string{"threadId": threadID})
	if err != nil {
		if isUnsupportedRPCError(err) {
			c.setCapability(CapabilityThreadResume, false)
			return nil, fmt.Errorf("%w: thread/resume: %v", ErrCapabilityUnsupported, err)
		}
		return nil, err
	}
	c.setCapability(CapabilityThreadResume, true)
	return append(json.RawMessage(nil), result...), nil
}

// AccountReadParams is the stable subset of standard account/read parameters
// needed by a companion CLI.
type AccountReadParams struct {
	RefreshToken          bool `json:"refreshToken,omitempty"`
	ReloadAuthFromStorage bool `json:"reloadAuthFromStorage,omitempty"`
}

// AccountReadResult is the stable account/read response. Account is opaque
// JSON because providers may add account fields over time.
type AccountReadResult struct {
	Account            json.RawMessage `json:"account"`
	RequiresOpenAIAuth bool            `json:"requiresOpenaiAuth"`
	AuthChanged        bool            `json:"authChanged"`
}

func (c *InstalledClient) setCapability(name InstalledCapability, value bool) {
	c.stateMu.Lock()
	c.capabilities[name] = value
	c.stateMu.Unlock()
}

func (c *InstalledClient) notify(ctx context.Context, method string, params any) error {
	message := map[string]any{"method": method}
	if params != nil {
		message["params"] = params
	}
	return c.writeMessage(ctx, message)
}

func (c *InstalledClient) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	if strings.TrimSpace(method) == "" {
		return nil, errors.New("app-server method is required")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	c.callMu.Lock()
	defer c.callMu.Unlock()

	c.stateMu.RLock()
	closed := c.closed
	c.stateMu.RUnlock()
	if closed {
		return nil, ErrInstalledClientClosed
	}
	id := c.sequence.Add(1)
	responseCh := make(chan installedResponse, 1)
	c.pendingMu.Lock()
	c.pending[id] = responseCh
	c.pendingMu.Unlock()
	if err := c.writeMessage(ctx, map[string]any{"id": id, "method": method, "params": params}); err != nil {
		c.removePending(id)
		return nil, err
	}
	select {
	case response := <-responseCh:
		if response.err != nil {
			return nil, response.err
		}
		if response.rpcError != nil {
			return nil, response.rpcError
		}
		if len(response.result) == 0 {
			return nil, errors.New("app-server response has no result")
		}
		return response.result, nil
	case <-ctx.Done():
		c.removePending(id)
		return nil, ctx.Err()
	}
}

func (c *InstalledClient) removePending(id uint64) {
	c.pendingMu.Lock()
	delete(c.pending, id)
	c.pendingMu.Unlock()
}

func (c *InstalledClient) startReader() {
	if c == nil {
		return
	}
	c.readerOnce.Do(func() {
		c.stateMu.Lock()
		c.readerStarted = true
		c.stateMu.Unlock()
		go c.readLoop()
	})
}

func (c *InstalledClient) readLoop() {
	var terminalErr error
	for {
		frame, err := readWebsocketFrame(c.conn, c.maxMessageBytes)
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) {
				terminalErr = fmt.Errorf("%w: %v", ErrAppServerUnavailable, err)
			} else {
				terminalErr = err
			}
			break
		}
		switch frame.opcode {
		case websocketPing:
			if err := c.writeControlFrame(websocketPong, frame.payload); err != nil {
				terminalErr = fmt.Errorf("write app-server pong: %w", err)
				break
			}
			continue
		case websocketPong:
			continue
		case websocketClose:
			terminalErr = fmt.Errorf("%w: app-server closed the control socket", ErrAppServerUnavailable)
		default:
			if frame.opcode != websocketText {
				continue
			}
			if err := c.routeMessage(frame.payload); err != nil {
				terminalErr = err
			}
		}
		if terminalErr != nil {
			break
		}
	}
	c.finishReader(terminalErr)
}

func (c *InstalledClient) routeMessage(payload []byte) error {
	var envelope struct {
		ID     json.RawMessage    `json:"id"`
		Method string             `json:"method"`
		Result json.RawMessage    `json:"result"`
		Error  *AppServerRPCError `json:"error"`
		Params json.RawMessage    `json:"params"`
	}
	if err := json.Unmarshal(payload, &envelope); err != nil {
		return fmt.Errorf("decode app-server message: %w", err)
	}
	if envelope.Method != "" {
		c.observeNotification(InstalledNotification{Method: envelope.Method, Params: append(json.RawMessage(nil), envelope.Params...)})
		return nil
	}
	id, ok := jsonIDValue(envelope.ID)
	if !ok {
		return nil
	}
	c.pendingMu.Lock()
	responseCh := c.pending[id]
	delete(c.pending, id)
	c.pendingMu.Unlock()
	if responseCh == nil {
		return nil
	}
	responseCh <- installedResponse{
		result:   append(json.RawMessage(nil), envelope.Result...),
		rpcError: envelope.Error,
	}
	return nil
}

func (c *InstalledClient) finishReader(err error) {
	if err == nil {
		err = ErrAppServerUnavailable
	}
	c.stateMu.Lock()
	c.closed = true
	c.stateMu.Unlock()
	c.pendingMu.Lock()
	pending := c.pending
	c.pending = make(map[uint64]chan installedResponse)
	c.pendingMu.Unlock()
	for _, responseCh := range pending {
		responseCh <- installedResponse{err: err}
	}
	c.notificationCloseOnce.Do(func() { close(c.notifications) })
}

func (c *InstalledClient) observeNotification(notification InstalledNotification) {
	switch notification.Method {
	case "turn/started":
		c.stateMu.Lock()
		c.activeTurns++
		c.stateMu.Unlock()
	case "turn/completed", "turn/aborted", "turn/failed":
		c.stateMu.Lock()
		if c.activeTurns > 0 {
			c.activeTurns--
		}
		c.stateMu.Unlock()
	}
	select {
	case c.notifications <- notification:
	default:
		// Notifications are advisory. Never block a response behind an unread
		// stream; the active-turn counter has already been updated above.
	}
}

func (c *InstalledClient) writeMessage(ctx context.Context, value any) error {
	payload, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("encode app-server message: %w", err)
	}
	if len(payload) > c.maxMessageBytes {
		return fmt.Errorf("app-server message exceeds %d bytes", c.maxMessageBytes)
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if err := c.setWriteDeadline(ctx); err != nil {
		return err
	}
	if err := c.writeFrame(websocketText, payload); err != nil {
		return fmt.Errorf("write app-server message: %w", err)
	}
	_ = c.conn.SetWriteDeadline(time.Time{})
	return nil
}

func (c *InstalledClient) setWriteDeadline(ctx context.Context) error {
	if ctx == nil {
		return c.conn.SetWriteDeadline(time.Time{})
	}
	if deadline, ok := ctx.Deadline(); ok {
		return c.conn.SetWriteDeadline(deadline)
	}
	return c.conn.SetWriteDeadline(time.Time{})
}

func (c *InstalledClient) writeControlFrame(opcode byte, payload []byte) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return c.writeFrame(opcode, payload)
}

type AppServerRPCError struct {
	Code    int64           `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *AppServerRPCError) Error() string {
	if e == nil {
		return "app-server RPC error"
	}
	return fmt.Sprintf("app-server RPC error (code %d): %s", e.Code, e.Message)
}

func isUnsupportedRPCError(err error) bool {
	var rpcError *AppServerRPCError
	if errors.As(err, &rpcError) {
		return rpcError.Code == -32601 || rpcError.Code == -32602
	}
	value := strings.ToLower(err.Error())
	return strings.Contains(value, "method not found") || strings.Contains(value, "unsupported method")
}

func jsonIDMatches(raw json.RawMessage, wanted uint64) bool {
	value, ok := jsonIDValue(raw)
	return ok && value == wanted
}

func jsonIDValue(raw json.RawMessage) (uint64, bool) {
	if len(raw) == 0 {
		return 0, false
	}
	if value, err := strconv.ParseUint(string(raw), 10, 64); err == nil {
		return value, true
	}
	var stringID string
	if json.Unmarshal(raw, &stringID) == nil {
		value, err := strconv.ParseUint(stringID, 10, 64)
		if err == nil {
			return value, true
		}
	}
	return 0, false
}

const (
	websocketContinuation byte = 0x0
	websocketText         byte = 0x1
	websocketClose        byte = 0x8
	websocketPing         byte = 0x9
	websocketPong         byte = 0xA
)

type websocketFrame struct {
	opcode  byte
	payload []byte
}

func websocketClientHandshake(conn net.Conn, endpoint string) error {
	keyBytes := make([]byte, 16)
	if _, err := rand.Read(keyBytes); err != nil {
		return fmt.Errorf("create WebSocket handshake key: %w", err)
	}
	key := base64.StdEncoding.EncodeToString(keyBytes)
	host := "localhost"
	if parsed, err := url.Parse(endpoint); err == nil && parsed.Host != "" {
		host = parsed.Host
	}
	request := "GET / HTTP/1.1\r\n" +
		"Host: " + host + "\r\n" +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n" +
		"Sec-WebSocket-Key: " + key + "\r\n" +
		"Sec-WebSocket-Version: 13\r\n\r\n"
	if _, err := io.WriteString(conn, request); err != nil {
		return fmt.Errorf("write WebSocket handshake: %w", err)
	}
	response, err := readHTTPHeaders(conn, 64<<10)
	if err != nil {
		return fmt.Errorf("read WebSocket handshake: %w", err)
	}
	lines := strings.Split(response, "\r\n")
	if len(lines) == 0 || !strings.Contains(lines[0], " 101 ") {
		return fmt.Errorf("WebSocket handshake rejected: %s", strings.TrimSpace(firstLine(response)))
	}
	accept := ""
	for _, line := range lines[1:] {
		name, value, ok := strings.Cut(line, ":")
		if ok && strings.EqualFold(strings.TrimSpace(name), "Sec-WebSocket-Accept") {
			accept = strings.TrimSpace(value)
			break
		}
	}
	hash := sha1.Sum([]byte(key + websocketGUID)) // #nosec G401 -- mandated by RFC 6455.
	want := base64.StdEncoding.EncodeToString(hash[:])
	if accept == "" || accept != want {
		return errors.New("WebSocket handshake has an invalid Sec-WebSocket-Accept")
	}
	return nil
}

func readHTTPHeaders(reader io.Reader, limit int) (string, error) {
	buffer := make([]byte, 0, 1024)
	chunk := []byte{0}
	for len(buffer) < limit {
		if _, err := io.ReadFull(reader, chunk); err != nil {
			return "", err
		}
		buffer = append(buffer, chunk[0])
		if len(buffer) >= 4 && string(buffer[len(buffer)-4:]) == "\r\n\r\n" {
			return string(buffer), nil
		}
	}
	return "", errors.New("WebSocket handshake headers exceed limit")
}

func firstLine(value string) string {
	if index := strings.Index(value, "\r\n"); index >= 0 {
		return value[:index]
	}
	return value
}

func (c *InstalledClient) writeFrame(opcode byte, payload []byte) error {
	if len(payload) > c.maxMessageBytes {
		return fmt.Errorf("WebSocket frame exceeds %d bytes", c.maxMessageBytes)
	}
	maskKey := make([]byte, 4)
	if _, err := rand.Read(maskKey); err != nil {
		return err
	}
	header := make([]byte, 0, 14)
	header = append(header, 0x80|opcode)
	switch {
	case len(payload) < 126:
		header = append(header, byte(0x80|len(payload)))
	case len(payload) <= 0xffff:
		header = append(header, 0x80|126, byte(len(payload)>>8), byte(len(payload)))
	default:
		header = append(header, 0x80|127)
		var length [8]byte
		binary.BigEndian.PutUint64(length[:], uint64(len(payload)))
		header = append(header, length[:]...)
	}
	header = append(header, maskKey...)
	masked := make([]byte, len(payload))
	for index, value := range payload {
		masked[index] = value ^ maskKey[index%4]
	}
	if _, err := c.conn.Write(append(header, masked...)); err != nil {
		return err
	}
	return nil
}

func readWebsocketFrame(reader io.Reader, maxBytes int) (websocketFrame, error) {
	var header [2]byte
	if _, err := io.ReadFull(reader, header[:]); err != nil {
		return websocketFrame{}, err
	}
	fin := header[0]&0x80 != 0
	opcode := header[0] & 0x0f
	if !fin && opcode != websocketContinuation {
		return websocketFrame{}, errors.New("fragmented WebSocket control/data frame is unsupported")
	}
	masked := header[1]&0x80 != 0
	length := int64(header[1] & 0x7f)
	if length == 126 {
		var extended [2]byte
		if _, err := io.ReadFull(reader, extended[:]); err != nil {
			return websocketFrame{}, err
		}
		length = int64(binary.BigEndian.Uint16(extended[:]))
	} else if length == 127 {
		var extended [8]byte
		if _, err := io.ReadFull(reader, extended[:]); err != nil {
			return websocketFrame{}, err
		}
		value := binary.BigEndian.Uint64(extended[:])
		if value > uint64(maxBytes) {
			return websocketFrame{}, fmt.Errorf("WebSocket frame exceeds %d bytes", maxBytes)
		}
		length = int64(value)
	}
	if length < 0 || length > int64(maxBytes) {
		return websocketFrame{}, fmt.Errorf("WebSocket frame exceeds %d bytes", maxBytes)
	}
	var mask [4]byte
	if masked {
		if _, err := io.ReadFull(reader, mask[:]); err != nil {
			return websocketFrame{}, err
		}
	}
	payload := make([]byte, int(length))
	if _, err := io.ReadFull(reader, payload); err != nil {
		return websocketFrame{}, err
	}
	if masked {
		for index := range payload {
			payload[index] ^= mask[index%4]
		}
	}
	if opcode == websocketContinuation {
		return websocketFrame{}, errors.New("fragmented WebSocket messages are unsupported")
	}
	return websocketFrame{opcode: opcode, payload: payload}, nil
}
