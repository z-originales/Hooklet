// Package heavy contains end-to-end tests that are correct and important but
// take more time to run (reconnection waits, multiple harness instances, etc.).
// They are intended for nightly CI or pre-release validation.
//
// Run the heavy suite:
//
//	go test -v ./test/e2e/heavy -count=1 -timeout 10m
package heavy

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"hooklet/internal/api"
	"hooklet/test/e2e/internal/harness"
)

// ─────────────────────────────────────────────────────────────────────────────
// TestOfflineQueueRetention
//
// Verifies that RabbitMQ holds messages in a durable queue while the consumer
// is disconnected, and delivers them upon reconnection.
//
// This relies on the durable queue that the service creates when a consumer
// first connects. The queue persists after the WebSocket closes, so any
// messages published while offline are retained until the consumer reconnects.
//
// Flow:
//
//	connect → auth → disconnect → publish → reconnect → auth → read retained msg
//
// ─────────────────────────────────────────────────────────────────────────────
func TestOfflineQueueRetention(t *testing.T) {
	h := harness.New(t, harness.WithRabbitPort(sharedRabbitPort))

	// Set up webhook + subscribed consumer
	whOut, err := h.CLI("webhook", "create", "retention-hook")
	if err != nil {
		t.Fatalf("create webhook: %v\noutput: %s", err, whOut)
	}
	hash := harness.ExtractWebhookHash(t, whOut)

	conOut, err := h.CLI("consumer", "create", "retention-consumer", "--subscriptions", "retention-hook")
	if err != nil {
		t.Fatalf("create consumer: %v\noutput: %s", err, conOut)
	}
	consumerToken := harness.ExtractConsumerToken(t, conOut)

	// First connection: authenticate so RabbitMQ creates and binds the durable queue.
	ws1 := h.ConnectWS("retention-hook")
	auth1 := ws1.Auth(consumerToken)
	if auth1["auth_status"] != "auth_ok" {
		t.Fatalf("first auth failed: %v", auth1)
	}

	// Disconnect the consumer — the queue remains in RabbitMQ.
	ws1.Conn.CloseNow()
	time.Sleep(200 * time.Millisecond) // let the server process the close

	// Publish while consumer is offline
	payload := map[string]string{"event": "offline.event"}
	resp := h.Post(api.RoutePublish+hash, payload, nil)
	defer resp.Body.Close()
	harness.AssertStatus(t, resp, http.StatusAccepted)

	// Reconnect — the retained message should be delivered immediately after auth.
	ws2 := h.ConnectWS("retention-hook")
	auth2 := ws2.Auth(consumerToken)
	if auth2["auth_status"] != "auth_ok" {
		t.Fatalf("reconnect auth failed: %v", auth2)
	}

	raw := ws2.ReadMessage()
	var received map[string]string
	if err := json.Unmarshal(raw, &received); err != nil {
		t.Fatalf("unmarshal retained message: %v\nraw: %s", err, raw)
	}
	if received["event"] != "offline.event" {
		t.Fatalf("expected retained event, got: %v", received)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// TestSingleActiveConnectionPerConsumer
//
// Verifies that the server enforces a single active WebSocket connection per
// consumer. When the same consumer reconnects, the previous connection must
// receive a "kicked" notification and then be closed by the server.
//
// This is a deliberate product decision: consumers use a stable named queue,
// and having two connections simultaneously would cause duplicate deliveries.
// ─────────────────────────────────────────────────────────────────────────────
func TestSingleActiveConnectionPerConsumer(t *testing.T) {
	h := harness.New(t, harness.WithRabbitPort(sharedRabbitPort))

	// Set up webhook + consumer
	whOut, err := h.CLI("webhook", "create", "kick-hook")
	if err != nil {
		t.Fatalf("create webhook: %v\noutput: %s", err, whOut)
	}

	conOut, err := h.CLI("consumer", "create", "kick-consumer", "--subscriptions", "kick-hook")
	if err != nil {
		t.Fatalf("create consumer: %v\noutput: %s", err, conOut)
	}
	consumerToken := harness.ExtractConsumerToken(t, conOut)

	// First connection
	ws1 := h.ConnectWS("kick-hook")
	auth1 := ws1.Auth(consumerToken)
	if auth1["auth_status"] != "auth_ok" {
		t.Fatalf("first auth failed: %v", auth1)
	}

	// Second connection with the same consumer token
	ws2 := h.ConnectWS("kick-hook")
	auth2 := ws2.Auth(consumerToken)
	if auth2["auth_status"] != "auth_ok" {
		t.Fatalf("second auth failed: %v", auth2)
	}

	// The first connection should now receive a "kicked" message or a close frame.
	raw, err := ws1.ReadMessageRaw()
	if err != nil {
		// Close frame received — server closed the old connection
		return
	}

	var kicked map[string]interface{}
	json.Unmarshal(raw, &kicked) //nolint:errcheck
	if kicked["type"] != "kicked" {
		t.Fatalf("expected kicked message on old connection, got: %v", kicked)
	}
}
