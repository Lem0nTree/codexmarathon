package runtime

import (
	"bufio"
	"context"
	"crypto/sha1" // #nosec G505 -- test verifies RFC 6455 handshake behavior.
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestParseCodexVersion(t *testing.T) {
	for _, test := range []struct {
		name   string
		output string
		want   string
	}{
		{name: "cli", output: "codex-cli 0.48.0\n", want: "0.48.0"},
		{name: "user agent", output: "codex_app_server/1.2.3 (Linux aarch64)", want: "1.2.3"},
		{name: "development label", output: "local-build\n", want: "local-build"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := parseCodexVersion(test.output); got != test.want {
				t.Fatalf("parseCodexVersion(%q) = %q, want %q", test.output, got, test.want)
			}
		})
	}
}

func TestResolveCodexHomeUsesExplicitPath(t *testing.T) {
	path, err := resolveCodexHome("./codex-home")
	if err != nil {
		t.Fatal(err)
	}
	want, err := filepath.Abs("./codex-home")
	if err != nil {
		t.Fatal(err)
	}
	if path != filepath.Clean(want) {
		t.Fatalf("resolveCodexHome() = %q, want %q", path, want)
	}
}

func TestDiscoverInstalledRunsOnlyReadOnlyCommands(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture uses a POSIX executable")
	}
	dir := t.TempDir()
	logPath := filepath.Join(dir, "commands.log")
	bin := filepath.Join(dir, "codex")
	script := fmt.Sprintf(`#!/bin/sh
printf '%%s\n' "$*" >> %q
case "$*" in
  --version) printf 'codex 9.8.7\n' ;;
  'app-server --help') printf 'app-server\n' ;;
  'app-server daemon --help') printf 'daemon\n' ;;
  'app-server proxy --help') printf 'proxy\n' ;;
  *) exit 2 ;;
esac
`, logPath)
	if err := os.WriteFile(bin, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	installation, err := DiscoverInstalled(context.Background(), DiscoveryOptions{
		Executable:   bin,
		CodexHome:    filepath.Join(dir, ".codex"),
		ProbeTimeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	if installation.Version != "9.8.7" {
		t.Fatalf("installation version = %q", installation.Version)
	}
	for _, capability := range []InstalledCapability{
		CapabilityAppServer,
		CapabilityAppServerDaemon,
		CapabilityAppServerProxy,
		CapabilityAuthReload,
		CapabilityThreadResume,
	} {
		if !installation.HasCapability(capability) {
			t.Fatalf("discovery omitted capability %q: %#v", capability, installation.Capabilities)
		}
	}
	log, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(log), "auth") || strings.Contains(string(log), "login") {
		t.Fatalf("discovery ran a mutating command: %s", log)
	}
}

func TestInstalledClientReloadsAuthThroughStandardAccountRead(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	client, err := newInstalledClient(clientConn)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	defer serverConn.Close()

	go func() {
		_ = serveFakeAppServer(serverConn, func(request map[string]json.RawMessage) (map[string]any, bool) {
			var method string
			if raw := request["method"]; len(raw) > 0 {
				_ = json.Unmarshal(raw, &method)
			}
			id := request["id"]
			switch method {
			case "initialize":
				return map[string]any{"id": json.RawMessage(id), "result": map[string]any{
					"userAgent":      "codex_app_server/4.5.6 (Linux aarch64)",
					"codexHome":      "/tmp/codex",
					"platformFamily": "unix",
					"platformOs":     "linux",
				}}, true
			case "account/read":
				var params AccountReadParams
				if err := json.Unmarshal(request["params"], &params); err != nil {
					return map[string]any{"id": json.RawMessage(id), "error": map[string]any{"code": -32602, "message": err.Error()}}, true
				}
				if !params.ReloadAuthFromStorage {
					return map[string]any{"id": json.RawMessage(id), "result": map[string]any{
						"account": nil, "requiresOpenaiAuth": true, "authChanged": false,
					}}, true
				}
				return map[string]any{"id": json.RawMessage(id), "result": map[string]any{
					"account":            map[string]any{"type": "chatgpt", "email": "operator@example.test"},
					"requiresOpenaiAuth": true,
					"authChanged":        true,
				}}, true
			default:
				return map[string]any{"id": json.RawMessage(id), "result": map[string]any{}}, true
			}
		})
	}()
	if err := websocketClientHandshake(clientConn, ""); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := client.initialize(ctx); err != nil {
		t.Fatal(err)
	}
	if client.Version() != "4.5.6" || !client.HasCapability(CapabilityAuthReload) {
		t.Fatalf("handshake metadata missing: version=%q authReload=%t", client.Version(), client.HasCapability(CapabilityAuthReload))
	}
	result, err := client.ReloadAuth(ctx)
	if err != nil {
		t.Fatalf("ReloadAuth() error = %v", err)
	}
	if !result.AuthChanged || !json.Valid(result.Account) {
		t.Fatalf("ReloadAuth() = %#v", result)
	}
}

func TestInstalledClientRejectsReloadDuringObservedTurn(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	client, err := newInstalledClient(clientConn)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	defer serverConn.Close()

	go func() {
		_ = serveFakeAppServer(serverConn, func(request map[string]json.RawMessage) (map[string]any, bool) {
			var method string
			_ = json.Unmarshal(request["method"], &method)
			id := request["id"]
			if method == "initialize" {
				return map[string]any{"id": json.RawMessage(id), "result": map[string]any{"userAgent": "codex/1.0.0"}}, true
			}
			return map[string]any{"id": json.RawMessage(id), "result": map[string]any{}}, true
		})
	}()
	if err := websocketClientHandshake(clientConn, ""); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := client.initialize(ctx); err != nil {
		t.Fatal(err)
	}
	// Injecting the standard notification through the same decoder path is
	// equivalent to an app-server turn/started notification and avoids a
	// second test server just for this guard.
	client.observeNotification(InstalledNotification{Method: "turn/started"})
	if client.ActiveTurnCount() != 1 {
		t.Fatalf("ActiveTurnCount() = %d, want 1", client.ActiveTurnCount())
	}
	if _, err := client.ReloadAuth(ctx); !errors.Is(err, ErrActiveTurn) {
		t.Fatalf("ReloadAuth() error = %v, want ErrActiveTurn", err)
	}
}

func TestInstalledClientMarksUnsupportedMethods(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	client, err := newInstalledClient(clientConn)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	defer serverConn.Close()

	go func() {
		_ = serveFakeAppServer(serverConn, func(request map[string]json.RawMessage) (map[string]any, bool) {
			var method string
			_ = json.Unmarshal(request["method"], &method)
			id := request["id"]
			if method == "initialize" {
				return map[string]any{"id": json.RawMessage(id), "result": map[string]any{"userAgent": "codex/1.0.0"}}, true
			}
			return map[string]any{"id": json.RawMessage(id), "error": map[string]any{"code": -32601, "message": "method not found"}}, true
		})
	}()
	if err := websocketClientHandshake(clientConn, ""); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := client.initialize(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := client.ResumeThread(ctx, "thread-1"); !errors.Is(err, ErrCapabilityUnsupported) {
		t.Fatalf("ResumeThread() error = %v, want ErrCapabilityUnsupported", err)
	}
	if client.HasCapability(CapabilityThreadResume) {
		t.Fatal("thread resume capability remained enabled after method-not-found")
	}
}

func TestInstalledClientReceivesAsynchronousTurnNotifications(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	client, err := newInstalledClient(clientConn)
	if err != nil {
		t.Fatal(err)
	}
	serverDone := make(chan struct{})
	defer func() {
		// Close the peer first so the fake server cannot remain blocked in its
		// read loop while the test returns.
		_ = serverConn.Close()
		_ = client.Close()
		<-serverDone
	}()

	go func() {
		defer close(serverDone)
		_ = serveFakeAppServer(serverConn, func(request map[string]json.RawMessage) (map[string]any, bool) {
			var method string
			_ = json.Unmarshal(request["method"], &method)
			id := request["id"]
			if method == "initialize" {
				return map[string]any{"id": json.RawMessage(id), "result": map[string]any{"userAgent": "codex/1.0.0"}}, true
			}
			return map[string]any{"id": json.RawMessage(id), "result": map[string]any{}}, true
		})
	}()
	if err := websocketClientHandshake(clientConn, ""); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := client.initialize(ctx); err != nil {
		t.Fatal(err)
	}

	started := []byte(`{"method":"turn/started","params":{"threadId":"thread-1","turn":{"id":"turn-1","status":"inProgress"}}}`)
	if err := writeServerFrame(serverConn, websocketText, started); err != nil {
		t.Fatal(err)
	}
	select {
	case notification := <-client.Notifications():
		if notification.Method != "turn/started" {
			t.Fatalf("first notification method = %q", notification.Method)
		}
	case <-ctx.Done():
		t.Fatalf("waiting for turn/started notification: %v", ctx.Err())
	}
	deadline := time.Now().Add(time.Second)
	for client.ActiveTurnCount() != 1 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := client.ActiveTurnCount(); got != 1 {
		t.Fatalf("ActiveTurnCount() after turn/started = %d, want 1", got)
	}
	completed := []byte(`{"method":"turn/completed","params":{"threadId":"thread-1","turn":{"id":"turn-1","status":"completed"}}}`)
	if err := writeServerFrame(serverConn, websocketText, completed); err != nil {
		t.Fatal(err)
	}
	select {
	case notification := <-client.Notifications():
		if notification.Method != "turn/completed" {
			t.Fatalf("second notification method = %q", notification.Method)
		}
	case <-ctx.Done():
		t.Fatalf("waiting for turn/completed notification: %v", ctx.Err())
	}
	deadline = time.Now().Add(time.Second)
	for client.ActiveTurnCount() != 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := client.ActiveTurnCount(); got != 0 {
		t.Fatalf("ActiveTurnCount() after turn/completed = %d, want 0", got)
	}
}

// serveFakeAppServer implements enough RFC 6455 to exercise the client over
// net.Pipe. It deliberately validates that requests are masked by the client.
func serveFakeAppServer(conn net.Conn, respond func(map[string]json.RawMessage) (map[string]any, bool)) error {
	requestHeaders, err := readHTTPHeaders(bufio.NewReader(conn), 64<<10)
	if err != nil {
		return err
	}
	key := ""
	for _, line := range strings.Split(requestHeaders, "\r\n") {
		name, value, ok := strings.Cut(line, ":")
		if ok && strings.EqualFold(strings.TrimSpace(name), "Sec-WebSocket-Key") {
			key = strings.TrimSpace(value)
		}
	}
	if key == "" {
		return errors.New("fake server received no WebSocket key")
	}
	hash := sha1.Sum([]byte(key + websocketGUID)) // #nosec G401 -- mandated by RFC 6455.
	response := "HTTP/1.1 101 Switching Protocols\r\n" +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n" +
		"Sec-WebSocket-Accept: " + base64.StdEncoding.EncodeToString(hash[:]) + "\r\n\r\n"
	if _, err := io.WriteString(conn, response); err != nil {
		return err
	}
	for {
		frame, err := readWebsocketFrame(conn, defaultInstalledMessageLimit)
		if err != nil {
			return err
		}
		if frame.opcode == websocketClose {
			return nil
		}
		if frame.opcode != websocketText {
			continue
		}
		var request map[string]json.RawMessage
		if err := json.Unmarshal(frame.payload, &request); err != nil {
			return err
		}
		// Notifications, including the required initialized handshake
		// message, do not receive a response.
		if len(request["id"]) == 0 || string(request["id"]) == "null" {
			continue
		}
		if response, ok := respond(request); ok {
			payload, err := json.Marshal(response)
			if err != nil {
				return err
			}
			if err := writeServerFrame(conn, websocketText, payload); err != nil {
				return err
			}
		}
	}
}

func writeServerFrame(conn io.Writer, opcode byte, payload []byte) error {
	header := []byte{0x80 | opcode}
	switch {
	case len(payload) < 126:
		header = append(header, byte(len(payload)))
	case len(payload) <= 0xffff:
		header = append(header, 126, byte(len(payload)>>8), byte(len(payload)))
	default:
		header = append(header, 127)
		var length [8]byte
		binary.BigEndian.PutUint64(length[:], uint64(len(payload)))
		header = append(header, length[:]...)
	}
	_, err := conn.Write(append(header, payload...))
	return err
}
