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
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"hooklet/internal/api"
	"hooklet/test/e2e/internal/harness"
)

// ─────────────────────────────────────────────────────────────────────────────
// TestWebhookStrictRegistration
//
// Verifies the "secure by default" rule: publishing to any hash that was not
// explicitly registered must return 404. This is the most fundamental security
// contract of the service.
// ─────────────────────────────────────────────────────────────────────────────
func TestWebhookStrictRegistration(t *testing.T) {
	h := harness.New(t, harness.WithRabbitPort(sharedRabbitPort))

	resp := h.Post(api.RoutePublish+"not-a-real-hash", map[string]string{"event": "test"}, nil)
	defer resp.Body.Close()

	harness.AssertStatus(t, resp, http.StatusNotFound)
	harness.AssertBodyContains(t, resp, "not found")
}

// ─────────────────────────────────────────────────────────────────────────────
// TestWebhookWithoutConsumerReturns503
//
// Verifies that a registered webhook that has no WebSocket consumer currently
// connected (and therefore no queue bound in RabbitMQ) returns 503.
// This is the expected behaviour when a producer publishes before any
// consumer has connected.
// ─────────────────────────────────────────────────────────────────────────────
func TestWebhookWithoutConsumerReturns503(t *testing.T) {
	h := harness.New(t, harness.WithRabbitPort(sharedRabbitPort))

	// Register a webhook — this creates the topic but no queue yet
	out, err := h.CLI("webhook", "create", "no-consumer-hook")
	if err != nil {
		t.Fatalf("create webhook: %v\noutput: %s", err, out)
	}
	hash := harness.ExtractWebhookHash(t, out)

	// Publish with no consumer listening — expect 503
	resp := h.Post(api.RoutePublish+hash, map[string]string{"event": "ping"}, nil)
	defer resp.Body.Close()

	harness.AssertStatus(t, resp, http.StatusServiceUnavailable)
	harness.AssertBodyContains(t, resp, "consumer")
}

// ─────────────────────────────────────────────────────────────────────────────
// TestProtectedWebhookProducerAuth
//
// Verifies producer authentication for webhooks created with --with-token.
// The test covers all three cases in sequence:
//   - no token       → 401
//   - wrong token    → 401
//   - correct token  → 202
//
// A WebSocket consumer is connected before any publish attempt so that the
// route actually exists in RabbitMQ (otherwise 503 would mask the 401/202).
// ─────────────────────────────────────────────────────────────────────────────
func TestProtectedWebhookProducerAuth(t *testing.T) {
	h := harness.New(t, harness.WithRabbitPort(sharedRabbitPort))

	// Create a webhook that requires a producer token
	out, err := h.CLI("webhook", "create", "protected-hook", "--with-token")
	if err != nil {
		t.Fatalf("create webhook: %v\noutput: %s", err, out)
	}
	hash := harness.ExtractWebhookHash(t, out)
	producerToken := harness.ExtractConsumerToken(t, out) // same "Token:" pattern

	// Create a consumer subscribed to the webhook topic so RabbitMQ has a queue
	consumerOut, err := h.CLI("consumer", "create", "auth-consumer", "--subscriptions", "protected-hook")
	if err != nil {
		t.Fatalf("create consumer: %v\noutput: %s", err, consumerOut)
	}
	consumerToken := harness.ExtractConsumerToken(t, consumerOut)

	// Connect + authenticate the WebSocket consumer
	ws := h.ConnectWS("protected-hook")
	authResp := ws.Auth(consumerToken)
	if authResp["auth_status"] != "auth_ok" {
		t.Fatalf("ws auth failed: %v", authResp)
	}

	payload := map[string]string{"event": "secret"}

	// ── No token → 401 ────────────────────────────────────────────────────────
	resp := h.Post(api.RoutePublish+hash, payload, nil)
	resp.Body.Close()
	harness.AssertStatus(t, resp, http.StatusUnauthorized)

	// ── Wrong token → 401 ─────────────────────────────────────────────────────
	resp = h.Post(api.RoutePublish+hash, payload, map[string]string{
		api.HeaderAuthToken: "completely-wrong-token",
	})
	resp.Body.Close()
	harness.AssertStatus(t, resp, http.StatusUnauthorized)

	// ── Correct token → 202 + message delivered ───────────────────────────────
	// Use PostWithRetry: the queue binding may not be in place yet right after
	// WS auth, so the first attempt could return 503 before the correct 202.
	resp = h.PostWithRetry(api.RoutePublish+hash, payload, map[string]string{
		api.HeaderAuthToken: producerToken,
	}, 10, 100*time.Millisecond)
	resp.Body.Close()
	harness.AssertStatus(t, resp, http.StatusAccepted)

	// Confirm the message was delivered to the WebSocket consumer
	msg := ws.ReadMessage()
	var delivered struct {
		Type string `json:"type"`
		Data struct {
			Event string `json:"event"`
		} `json:"data"`
	}
	if err := json.Unmarshal(msg, &delivered); err != nil {
		t.Fatalf("failed to unmarshal delivered webhook: %v\nraw: %s", err, msg)
	}
	if delivered.Type != "webhook" || delivered.Data.Event != "secret" {
		t.Fatalf("unexpected delivered message: %s", msg)
	}
	if string(msg) == "" {
		t.Fatal("expected a message on the WebSocket, got nothing")
	}
}
