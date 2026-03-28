// Package smoke contains fast end-to-end tests that cover the core contracts
// of the Hooklet service. They run against real binaries and a real RabbitMQ
// container, but are designed to complete in seconds so they can run on every
// pull request.
//
// Run the smoke suite:
//
//	go test -v ./test/e2e/smoke -count=1
package smoke

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/coder/websocket"

	"hooklet/internal/api"
	"hooklet/test/e2e/internal/harness"
)

// ─────────────────────────────────────────────────────────────────────────────
// TestWebhookDeliveryHappyPath
//
// The central end-to-end test: publish an HTTP webhook and verify that the
// message travels through RabbitMQ and arrives at the WebSocket consumer
// with the exact same payload.
//
// Flow:
//
//	webhook create → consumer create → WS connect → auth → POST /webhook → read WS
//
// ─────────────────────────────────────────────────────────────────────────────
func TestWebhookDeliveryHappyPath(t *testing.T) {
	h := harness.New(t, harness.WithRabbitPort(sharedRabbitPort))

	// Register a webhook (creates the topic in the DB)
	whOut, err := h.CLI("webhook", "create", "delivery-hook")
	if err != nil {
		t.Fatalf("create webhook: %v\noutput: %s", err, whOut)
	}
	hash := harness.ExtractWebhookHash(t, whOut)

	// Create a consumer subscribed to that topic
	conOut, err := h.CLI("consumer", "create", "delivery-consumer", "--subscriptions", "delivery-hook")
	if err != nil {
		t.Fatalf("create consumer: %v\noutput: %s", err, conOut)
	}
	consumerToken := harness.ExtractConsumerToken(t, conOut)

	// Open WebSocket and authenticate
	ws := h.ConnectWS("delivery-hook")
	authResp := ws.Auth(consumerToken)
	if authResp["auth_status"] != "auth_ok" {
		t.Fatalf("ws auth failed: %v", authResp)
	}

	// Publish the webhook — retry on 503 in case the RabbitMQ queue binding
	// is not yet in place right after the WebSocket auth completes.
	payload := map[string]string{"event": "order.created", "id": "42"}
	resp := h.PostWithRetry(api.RoutePublish+hash, payload, nil, 10, 100*time.Millisecond)
	defer resp.Body.Close()
	harness.AssertStatus(t, resp, http.StatusAccepted)

	// Read from WebSocket and verify the payload matches exactly
	raw := ws.ReadMessage()
	var received struct {
		Type       string `json:"type"`
		Topic      string `json:"topic"`
		ReceivedAt string `json:"received_at"`
		Data       struct {
			Event string `json:"event"`
			ID    string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &received); err != nil {
		t.Fatalf("unmarshal WS message: %v\nraw: %s", err, raw)
	}
	if received.Type != "webhook" {
		t.Fatalf("unexpected message type: %q", received.Type)
	}
	if received.Topic != "delivery-hook" {
		t.Fatalf("unexpected topic: %q", received.Topic)
	}
	if received.ReceivedAt == "" {
		t.Fatal("expected received_at in webhook envelope")
	}
	if received.Data.Event != "order.created" || received.Data.ID != "42" {
		t.Fatalf("payload mismatch: got %v", received)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// TestWebSocketAuthRejections
//
// Verifies that the WebSocket endpoint rejects connections that fail
// authentication, covering the most important negative cases:
//   - invalid token in auth message → auth_failed
//   - malformed auth message         → auth_failed
//
// Each sub-test connects a fresh WebSocket so failures are independent.
// ─────────────────────────────────────────────────────────────────────────────
func TestWebSocketAuthRejections(t *testing.T) {
	h := harness.New(t, harness.WithRabbitPort(sharedRabbitPort))

	// We need at least one registered topic so the WS endpoint accepts the path.
	// Without a topic the server returns 400 before the WS upgrade.
	whOut, err := h.CLI("webhook", "create", "auth-rejection-hook")
	if err != nil {
		t.Fatalf("create webhook: %v\noutput: %s", err, whOut)
	}

	// writeWS is a small helper to send a raw text frame.
	writeWS := func(t *testing.T, conn *websocket.Conn, msg []byte) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		conn.Write(ctx, websocket.MessageText, msg) //nolint:errcheck
	}

	// ── Invalid token ─────────────────────────────────────────────────────────
	// Connect a WebSocket and send an auth message with a token that does not
	// exist in the DB. Expect auth_failed + connection close.
	t.Run("invalid_token", func(t *testing.T) {
		ws := h.ConnectWS("auth-rejection-hook")

		authMsg, _ := json.Marshal(map[string]string{
			"type":  "auth",
			"token": "totally-made-up-token",
		})
		writeWS(t, ws.Conn, authMsg)

		// Server should respond with auth_failed and close the connection.
		// A read error (close frame) is also acceptable.
		raw, err := ws.ReadMessageRaw()
		if err != nil {
			return // connection closed by server — that's correct
		}
		var resp map[string]interface{}
		json.Unmarshal(raw, &resp) //nolint:errcheck
		if resp["auth_status"] != "auth_failed" {
			t.Fatalf("expected auth_failed, got: %v", resp)
		}
	})

	// ── Malformed auth message ────────────────────────────────────────────────
	// Send syntactically valid JSON but missing the required "type" field.
	t.Run("malformed_auth_message", func(t *testing.T) {
		ws := h.ConnectWS("auth-rejection-hook")

		writeWS(t, ws.Conn, []byte(`{"not_the_right_field": "value"}`))

		raw, err := ws.ReadMessageRaw()
		if err != nil {
			return // close frame is acceptable
		}
		var resp map[string]interface{}
		json.Unmarshal(raw, &resp) //nolint:errcheck
		if resp["auth_status"] != "auth_failed" {
			t.Fatalf("expected auth_failed, got: %v", resp)
		}
	})
}
