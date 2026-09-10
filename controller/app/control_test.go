package app

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"codexmarathon/controller/internal/accounts"
)

func TestControllerControlServerNegotiatesStatusAndResolvesAlias(t *testing.T) {
	stateDir := t.TempDir()
	controller, err := New(Config{
		StateDir: stateDir,
		AuthPath: filepath.Join(stateDir, "auth.json"),
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if err := controller.EnsureLayout(); err != nil {
		t.Fatalf("EnsureLayout() error = %v", err)
	}
	for _, account := range []struct {
		id    string
		alias string
		token string
	}{
		{id: "acct-a", alias: "personal", token: "token-a"},
		{id: "acct-b", alias: "work", token: "token-b"},
	} {
		_, err := controller.AccountManager().Import(context.Background(), accounts.ImportRequest{
			AccountID: account.id,
			Alias:     account.alias,
			AuthJSON:  []byte(fmt.Sprintf(`{"tokens":{"account_id":%q,"access_token":%q}}`, account.id, account.token)),
		})
		if err != nil {
			t.Fatalf("Import(%s) error = %v", account.id, err)
		}
	}

	var switchedTo string
	server, err := NewControllerControlServer(controller, ControllerControlServerConfig{
		Switch: func(_ context.Context, accountID string) (ControlSwitchResult, error) {
			switchedTo = accountID
			return ControlSwitchResult{AccountID: accountID, Outcome: "committed", Mode: "test", IdentityVerified: true}, nil
		},
	})
	if err != nil {
		t.Fatalf("NewControllerControlServer() error = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := server.Start(ctx); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	defer func() { _ = server.Close() }()

	path := strings.TrimPrefix(server.Endpoint(), "unix://")
	conn, err := net.Dial("unix", path)
	if err != nil {
		t.Fatalf("dial controller endpoint: %v", err)
	}
	defer conn.Close()
	reader := bufio.NewReader(conn)

	writeControlRequest(t, conn, `{"jsonrpc":"2.0","id":"1","method":"protocol/negotiate","params":{"supported_versions":[1]}}`)
	var negotiated struct {
		Result controlNegotiateResult `json:"result"`
	}
	readControlResponse(t, conn, reader, &negotiated)
	if negotiated.Result.ProtocolVersion != ControllerProtocolVersion {
		t.Fatalf("protocol version = %d, want %d", negotiated.Result.ProtocolVersion, ControllerProtocolVersion)
	}

	writeControlRequest(t, conn, `{"jsonrpc":"2.0","id":"2","method":"marathon/status","params":{}}`)
	var status struct {
		Result ControlStatus `json:"result"`
	}
	readControlResponse(t, conn, reader, &status)
	if status.Result.ActiveAccountID != "acct-a" {
		t.Fatalf("active account = %q, want acct-a", status.Result.ActiveAccountID)
	}
	if len(status.Result.Accounts) != 2 || status.Result.Accounts[1].Alias != "work" {
		t.Fatalf("status accounts = %#v", status.Result.Accounts)
	}

	writeControlRequest(t, conn, `{"jsonrpc":"2.0","id":"3","method":"marathon/switch","params":{"target":"work","timeout_ms":1000}}`)
	var switched struct {
		Result ControlSwitchResult `json:"result"`
	}
	readControlResponse(t, conn, reader, &switched)
	if switchedTo != "acct-b" {
		t.Fatalf("switch target = %q, want acct-b", switchedTo)
	}
	if switched.Result.AccountID != "acct-b" || switched.Result.Alias != "work" || switched.Result.Outcome != "committed" {
		t.Fatalf("switch result = %#v", switched.Result)
	}
}

func TestControllerControlServerRequiresNegotiation(t *testing.T) {
	controller, err := New(Config{StateDir: t.TempDir(), AuthPath: filepath.Join(t.TempDir(), "auth.json")})
	if err != nil {
		t.Fatal(err)
	}
	server, err := NewControllerControlServer(controller, ControllerControlServerConfig{})
	if err != nil {
		t.Fatal(err)
	}
	response := server.handleLine(context.Background(), []byte(`{"jsonrpc":"2.0","id":"1","method":"marathon/status","params":{}}`), new(bool))
	var decoded struct {
		Error *controlError `json:"error"`
	}
	if err := json.Unmarshal(response, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Error == nil || decoded.Error.Code != -32001 {
		t.Fatalf("response error = %#v, want not-negotiated", decoded.Error)
	}
}

func TestControllerControlEndpointRejectsNetworkAndOutsidePaths(t *testing.T) {
	stateDir := t.TempDir()
	if err := ValidateControllerControlEndpoint("tcp://127.0.0.1:1", stateDir); err == nil {
		t.Fatal("TCP controller endpoint was accepted")
	}
	outside := filepath.Join(t.TempDir(), "controller.sock")
	if err := ValidateControllerControlEndpoint("unix://"+outside, stateDir); err == nil {
		t.Fatal("controller endpoint outside state directory was accepted")
	}
	if _, err := DefaultControllerControlEndpoint(stateDir); err != nil {
		t.Fatalf("DefaultControllerControlEndpoint() error = %v", err)
	}
}

func writeControlRequest(t *testing.T, conn net.Conn, request string) {
	t.Helper()
	if _, err := fmt.Fprintf(conn, "%s\n", request); err != nil {
		t.Fatalf("write control request: %v", err)
	}
}

func readControlResponse(t *testing.T, conn net.Conn, reader *bufio.Reader, out any) {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	line, err := reader.ReadBytes('\n')
	if err != nil {
		t.Fatalf("read control response: %v", err)
	}
	if err := json.Unmarshal(line, out); err != nil {
		t.Fatalf("decode control response: %v", err)
	}
}
