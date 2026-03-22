// Package heavy contains end-to-end tests that are correct and important but
// take more time to run (reconnection waits, multiple harness instances, etc.).
// They are intended for nightly CI or pre-release validation.
//
// Run the heavy suite:
//
//	go test -v ./test/e2e/heavy -count=1 -timeout 10m
package heavy

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"hooklet/internal/api"
	"hooklet/test/e2e/internal/harness"
)

// ─────────────────────────────────────────────────────────────────────────────
// TestWebhookTokenLifecycle
//
// Verifies that the admin token management commands for webhooks work end to end:
//   - set-token generates a token and the webhook becomes protected
//   - clear-token removes it and the webhook becomes unprotected again
//
// This matters because the token rotation flow is a real operational scenario:
// a compromised producer token needs to be regenerated without downtime.
// ─────────────────────────────────────────────────────────────────────────────
func TestWebhookTokenLifecycle(t *testing.T) {
	h := harness.New(t, harness.WithRabbitPort(sharedRabbitPort))

	// Create a webhook without a token initially
	out, err := h.CLI("webhook", "create", "token-lifecycle-hook")
	if err != nil {
		t.Fatalf("create webhook: %v\noutput: %s", err, out)
	}
	hash := harness.ExtractWebhookHash(t, out)
	id := harness.ExtractWebhookID(t, out)

	// Connect a consumer so RabbitMQ has a queue (avoids 503 masking 401 checks)
	conOut, err := h.CLI("consumer", "create", "token-lifecycle-consumer",
		"--subscriptions", "token-lifecycle-hook")
	if err != nil {
		t.Fatalf("create consumer: %v\noutput: %s", err, conOut)
	}
	consumerToken := harness.ExtractConsumerToken(t, conOut)
	ws := h.ConnectWS("token-lifecycle-hook")
	if ws.Auth(consumerToken)["auth_status"] != "auth_ok" {
		t.Fatal("ws auth failed")
	}

	// ── Before set-token: webhook accepts requests without a producer token ───
	// PostWithRetry: queue binding may not be in place yet right after WS auth.
	resp := h.PostWithRetry(api.RoutePublish+hash, map[string]string{"step": "before"}, nil, 10, 100*time.Millisecond)
	resp.Body.Close()
	harness.AssertStatus(t, resp, http.StatusAccepted)

	// ── set-token: generate a producer token via CLI ──────────────────────────
	setOut, err := h.CLI("webhook", "set-token", id)
	if err != nil {
		t.Fatalf("set-token: %v\noutput: %s", err, setOut)
	}
	// CLI output has a "  Token: <value>" line
	producerToken := harness.ExtractConsumerToken(t, setOut)

	// Without token → 401 now that the webhook is protected
	resp = h.Post(api.RoutePublish+hash, map[string]string{"step": "no-token"}, nil)
	resp.Body.Close()
	harness.AssertStatus(t, resp, http.StatusUnauthorized)

	// With correct token → 202
	resp = h.Post(api.RoutePublish+hash, map[string]string{"step": "with-token"},
		map[string]string{api.HeaderAuthToken: producerToken})
	resp.Body.Close()
	harness.AssertStatus(t, resp, http.StatusAccepted)

	// ── clear-token: remove the token, webhook becomes open again ─────────────
	clearOut, err := h.CLI("webhook", "clear-token", id)
	if err != nil {
		t.Fatalf("clear-token: %v\noutput: %s", err, clearOut)
	}

	// Without token → 202 again after clear
	resp = h.Post(api.RoutePublish+hash, map[string]string{"step": "after-clear"}, nil)
	resp.Body.Close()
	harness.AssertStatus(t, resp, http.StatusAccepted)
}

// ─────────────────────────────────────────────────────────────────────────────
// TestConsumerTokenRegeneration
//
// Verifies that regenerating a consumer token invalidates the old token and
// only the new token grants WebSocket access.
//
// This simulates a compromised consumer token being rotated: new sessions
// must use the new token; the old token must be rejected.
// ─────────────────────────────────────────────────────────────────────────────
func TestConsumerTokenRegeneration(t *testing.T) {
	h := harness.New(t, harness.WithRabbitPort(sharedRabbitPort))

	// Create a webhook + consumer
	_, err := h.CLI("webhook", "create", "regen-hook")
	if err != nil {
		t.Fatalf("create webhook: %v", err)
	}

	conOut, err := h.CLI("consumer", "create", "regen-consumer", "--subscriptions", "regen-hook")
	if err != nil {
		t.Fatalf("create consumer: %v\noutput: %s", err, conOut)
	}
	oldToken := harness.ExtractConsumerToken(t, conOut)
	consumerID := harness.ExtractConsumerID(t, conOut)

	// ── Old token works before regen ──────────────────────────────────────────
	ws := h.ConnectWS("regen-hook")
	authResp := ws.Auth(oldToken)
	if authResp["auth_status"] != "auth_ok" {
		t.Fatalf("old token should work before regen: %v", authResp)
	}
	ws.Conn.CloseNow()

	// ── Regenerate the consumer token via CLI ─────────────────────────────────
	regenOut, err := h.CLI("consumer", "regen-token", consumerID)
	if err != nil {
		t.Fatalf("regen-token: %v\noutput: %s", err, regenOut)
	}
	// CLI outputs "New token: <value>" on its own line
	var newToken string
	for _, line := range strings.Split(regenOut, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "New token:") {
			newToken = strings.TrimSpace(strings.TrimPrefix(line, "New token:"))
			break
		}
	}
	if newToken == "" {
		t.Fatalf("could not extract new token from regen output:\n%s", regenOut)
	}

	// ── Old token no longer works ─────────────────────────────────────────────
	ws = h.ConnectWS("regen-hook")
	authResp = ws.Auth(oldToken)
	if authResp["auth_status"] == "auth_ok" {
		t.Fatal("old token should NOT work after regen")
	}
	ws.Conn.CloseNow()

	// ── New token works ───────────────────────────────────────────────────────
	ws = h.ConnectWS("regen-hook")
	authResp = ws.Auth(newToken)
	if authResp["auth_status"] != "auth_ok" {
		t.Fatalf("new token should work after regen: %v", authResp)
	}
}
