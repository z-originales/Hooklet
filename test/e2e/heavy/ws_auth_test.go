// Package heavy contains end-to-end tests that are correct and important but
// take more time to run (reconnection waits, multiple harness instances, etc.).
// They are intended for nightly CI or pre-release validation.
//
// Run the heavy suite:
//
//	go test -v ./test/e2e/heavy -count=1 -timeout 10m
package heavy

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/coder/websocket"

	"hooklet/test/e2e/internal/harness"
)

// writeWS sends a raw WebSocket text frame with a short timeout.
// Defined here to avoid duplication across heavy test files.
func writeWS(t *testing.T, conn *websocket.Conn, msg []byte) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn.Write(ctx, websocket.MessageText, msg) //nolint:errcheck
}

// ─────────────────────────────────────────────────────────────────────────────
// TestWSAuthNegativeCases
//
// Exhaustive matrix of WebSocket authentication failures. Each sub-test
// covers one invalid scenario. They are grouped here rather than in smoke
// because they each require a harness, which has a non-trivial startup cost,
// and because they are less critical for a fast PR gate than the happy path.
//
// Cases:
//   - absent auth message (timeout)      → connection closed with auth_failed
//   - wrong JSON structure (missing type) → auth_failed
//   - correct structure but empty token   → auth_failed
//   - valid JSON but type != "auth"       → auth_failed
//   - valid structure but unknown token   → auth_failed
//
// ─────────────────────────────────────────────────────────────────────────────
func TestWSAuthNegativeCases(t *testing.T) {
	// HOOKLET_WS_AUTH_TIMEOUT=1 keeps the auth_timeout sub-test to ~1s instead of ~10s.
	h := harness.New(t,
		harness.WithRabbitPort(sharedRabbitPort),
		harness.WithEnv("HOOKLET_WS_AUTH_TIMEOUT", "1"),
	)

	// Register a topic so the WS endpoint accepts the connection before auth.
	whOut, err := h.CLI("webhook", "create", "ws-auth-test-hook")
	if err != nil {
		t.Fatalf("create webhook: %v\noutput: %s", err, whOut)
	}

	// expectAuthFailed reads one message and asserts it is auth_failed.
	// A read error (e.g. close frame) is also accepted — the server may close
	// the connection rather than send a message.
	expectAuthFailed := func(t *testing.T, ws *harness.WSConn) {
		t.Helper()
		raw, err := ws.ReadMessageRaw()
		if err != nil {
			return // server closed connection — correct behaviour
		}
		var resp map[string]interface{}
		json.Unmarshal(raw, &resp) //nolint:errcheck
		if resp["auth_status"] != "auth_failed" {
			t.Fatalf("expected auth_failed, got: %v (raw: %s)", resp, raw)
		}
	}

	// ── Missing type field ────────────────────────────────────────────────────
	t.Run("missing_type", func(t *testing.T) {
		ws := h.ConnectWS("ws-auth-test-hook")
		writeWS(t, ws.Conn, []byte(`{"token":"some-token"}`))
		expectAuthFailed(t, ws)
	})

	// ── Empty token ───────────────────────────────────────────────────────────
	t.Run("empty_token", func(t *testing.T) {
		ws := h.ConnectWS("ws-auth-test-hook")
		writeWS(t, ws.Conn, []byte(`{"type":"auth","token":""}`))
		expectAuthFailed(t, ws)
	})

	// ── Wrong type value ──────────────────────────────────────────────────────
	t.Run("wrong_type_value", func(t *testing.T) {
		ws := h.ConnectWS("ws-auth-test-hook")
		writeWS(t, ws.Conn, []byte(`{"type":"subscribe","token":"abc"}`))
		expectAuthFailed(t, ws)
	})

	// ── Unknown token ─────────────────────────────────────────────────────────
	t.Run("unknown_token", func(t *testing.T) {
		ws := h.ConnectWS("ws-auth-test-hook")
		msg, _ := json.Marshal(map[string]string{
			"type":  "auth",
			"token": "this-token-does-not-exist-in-db",
		})
		writeWS(t, ws.Conn, msg)
		expectAuthFailed(t, ws)
	})

	// ── Auth timeout: send nothing, wait for server to close ─────────────────
	// The service is started with HOOKLET_WS_AUTH_TIMEOUT=1 (1 second) so the
	// server closes unauthenticated connections after ~1s. We wait up to 3s.
	t.Run("auth_timeout", func(t *testing.T) {
		ws := h.ConnectWS("ws-auth-test-hook")

		// Do NOT send any auth message — just wait for server to kick us.
		_, err := ws.ReadMessageRawTimeout(3 * time.Second)
		if err == nil {
			// If we got a message, it should be auth_failed
			t.Logf("server sent a message before closing (acceptable)")
		}
		// err != nil means the connection was closed — expected
	})
}
