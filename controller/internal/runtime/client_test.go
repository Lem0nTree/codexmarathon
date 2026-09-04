package runtime

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestClientCallAndDecodeEvent(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	client := NewClient(clientConn)
	defer client.Close()
	defer serverConn.Close()

	serverErr := make(chan error, 1)
	go func() {
		reader := bufio.NewReader(serverConn)
		line, err := reader.ReadBytes('\n')
		if err != nil {
			serverErr <- err
			return
		}
		var request map[string]any
		if err := json.Unmarshal(line, &request); err != nil {
			serverErr <- err
			return
		}
		if request["method"] != string(MethodGetRuntimeState) {
			serverErr <- errors.New("unexpected method")
			return
		}
		id, ok := request["id"].(string)
		if !ok || id == "" {
			serverErr <- errors.New("request id was not a string")
			return
		}
		response := map[string]any{
			"jsonrpc": "2.0",
			"id":      id,
			"result": RuntimeState{
				Identity: Identity{
					RuntimeID:      "runtime-test",
					AccountID:      stringPointer("account-a"),
					AuthGeneration: 7,
				},
				ActiveTurnCount:   0,
				PendingTransition: nil,
			},
		}
		if err := writeJSONLine(serverConn, response); err != nil {
			serverErr <- err
			return
		}
		event := map[string]any{
			"jsonrpc": "2.0",
			"method":  string(EventNotificationMethod),
			"params": map[string]any{
				"event_type":      string(EventIdentityChanged),
				"occurred_at":     1725451200,
				"runtime_id":      "runtime-test",
				"auth_generation": 8,
				"transition_id":   "tx-8",
				"previous_account_id": "account-a",
				"account_id":          "account-b",
			},
		}
		serverErr <- writeJSONLine(serverConn, event)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	state, err := client.GetRuntimeState(ctx)
	if err != nil {
		t.Fatalf("GetRuntimeState() error = %v", err)
	}
	if state.Identity.RuntimeID != "runtime-test" || state.Identity.AuthGeneration != 7 {
		t.Fatalf("unexpected runtime state: %+v", state)
	}
	select {
	case event := <-client.Events():
		if event.EventType != EventIdentityChanged || event.TransitionID != "tx-8" {
			t.Fatalf("unexpected event: %+v", event)
		}
		decoded, err := event.DecodePayload()
		if err != nil {
			t.Fatalf("DecodePayload() error = %v", err)
		}
		identity, ok := decoded.(*IdentityChangedEvent)
		if !ok || identity.AccountID == nil || *identity.AccountID != "account-b" {
			t.Fatalf("unexpected decoded identity event: %#v", decoded)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for identity event")
	}
	if err := <-serverErr; err != nil {
		t.Fatalf("server error = %v", err)
	}
}

func TestClientRejectsUnsupportedNegotiatedVersion(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	client := NewClient(clientConn)
	defer client.Close()
	defer serverConn.Close()

	go func() {
		reader := bufio.NewReader(serverConn)
		line, err := reader.ReadBytes('\n')
		if err != nil {
			return
		}
		var request map[string]any
		if json.Unmarshal(line, &request) != nil {
			return
		}
		_ = writeJSONLine(serverConn, map[string]any{
			"jsonrpc": "2.0",
			"id":      request["id"],
			"result": map[string]any{
				"protocol_version": 2,
				"server_versions":   []int{2},
			},
		})
	}()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := client.NegotiateVersion(ctx, []int{ProtocolVersion}); err == nil {
		t.Fatal("NegotiateVersion() error = nil, want unsupported version error")
	}
}

func TestTransitionParamsValidation(t *testing.T) {
	if err := (AuthTransitionParams{}).Validate(); err == nil {
		t.Fatal("empty transition params unexpectedly validated")
	}
	if err := (AuthTransitionParams{
		TransitionID:       "tx-1",
		TargetAccountID:    "account-b",
		ExpectedGeneration: 2,
	}).Validate(); err != nil {
		t.Fatalf("valid transition params rejected: %v", err)
	}
	if err := (VersionNegotiationParams{SupportedVersions: []int{1, 1}}).Validate(); err == nil {
		t.Fatal("duplicate protocol versions unexpectedly validated")
	}
}

func TestDecodeEventPreservesUnknownFieldsAndNarrowRateShape(t *testing.T) {
	raw := []byte(`{
		"event_type":"rate_limits_updated",
		"occurred_at":1725451200,
		"runtime_id":"runtime-test",
		"auth_generation":3,
		"account_id":"account-a",
		"rateLimits":{"limitId":"codex","limitName":"Codex","planType":"pro","rateLimitReachedType":null,"primary":{"usedPercent":91.5,"windowDurationMins":300,"resetsAt":1725454800,"futureField":"ignored"},"secondary":null},
		"futureEventField":{"kept":true}
	}`)
	event, err := DecodeEvent(raw)
	if err != nil {
		t.Fatalf("DecodeEvent() error = %v", err)
	}
	if event.EventType != EventRateLimitsUpdated || len(event.Payload) == 0 {
		t.Fatalf("unexpected event: %+v", event)
	}
	decoded, err := event.DecodePayload()
	if err != nil {
		t.Fatalf("DecodePayload() error = %v", err)
	}
	update := decoded.(*RateLimitsUpdatedEvent)
	if update.RateLimits.Primary == nil || update.RateLimits.Primary.UsedPercent != 91.5 {
		t.Fatalf("unexpected rate-limit update: %+v", update)
	}
	var payload map[string]any
	if err := json.Unmarshal(event.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if _, ok := payload["futureEventField"]; !ok {
		t.Fatal("unknown event field was not preserved")
	}
}

func TestDecodeEventRequiresCommonAndTransitionCorrelation(t *testing.T) {
	if _, err := DecodeEvent([]byte(`{"event_type":"runtime_ready","runtime_id":"runtime-test","auth_generation":1}`)); err == nil {
		t.Fatal("DecodeEvent() accepted an event without occurred_at")
	}
	if _, err := DecodeEvent([]byte(`{"event_type":"identity_changed","occurred_at":1,"runtime_id":"runtime-test","auth_generation":2}`)); err == nil {
		t.Fatal("DecodeEvent() accepted a transition event without transition_id")
	}
}

func TestClientRejectsResponseWithBothResultAndError(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	client := NewClient(clientConn)
	defer client.Close()
	defer serverConn.Close()

	err := client.handleMessage([]byte(`{"jsonrpc":"2.0","id":"1","result":{},"error":{"code":-1,"message":"failed"}}`))
	if !errors.Is(err, ErrInvalidMessage) {
		t.Fatalf("handleMessage() error = %v, want ErrInvalidMessage", err)
	}
	err = client.handleMessage([]byte(`{"jsonrpc":"2.0","id":"1","error":{"code":-1}}`))
	if !errors.Is(err, ErrInvalidMessage) {
		t.Fatalf("handleMessage(empty error message) error = %v, want ErrInvalidMessage", err)
	}
}

func TestProtocolSchemaFilesAreValidJSON(t *testing.T) {
	protocolDir := filepath.Join("..", "..", "..", "protocol")
	for _, name := range []string{"protocol.json", "commands.json", "events.json"} {
		raw, err := os.ReadFile(filepath.Join(protocolDir, name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if !json.Valid(raw) {
			t.Fatalf("%s is not valid JSON", name)
		}
	}
}

func writeJSONLine(writer interface{ Write([]byte) (int, error) }, value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	_, err = writer.Write(raw)
	return err
}

func stringPointer(value string) *string {
	return &value
}
