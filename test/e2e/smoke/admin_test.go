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
	"net/http"
	"testing"

	"hooklet/internal/api"
	"hooklet/test/e2e/internal/harness"
)

// ─────────────────────────────────────────────────────────────────────────────
// TestAdminTransports
//
// Verifies the three admin access paths in a single harness:
//  1. Unix socket  — implicit trust, no token needed
//  2. TCP + token  — standard remote administration
//  3. TCP no token — must be rejected (server-side enforcement)
//  4. Wrong token  — must be rejected
//  5. Direct HTTP  — raw GET /admin/webhooks without Authorization header
//
// Keeping all admin transport checks in one test avoids spinning up five
// separate RabbitMQ containers for what is fundamentally the same infra.
// ─────────────────────────────────────────────────────────────────────────────
func TestAdminTransports(t *testing.T) {
	h := harness.New(t, harness.WithRabbitPort(sharedRabbitPort))

	// ── 1. Unix socket: create + list webhook without any token ──────────────
	// The server grants implicit admin trust to connections arriving on the
	// Unix socket, so no HOOKLET_ADMIN_TOKEN is required here.
	t.Run("socket_create_and_list", func(t *testing.T) {
		out, err := h.CLISocket("webhook", "create", "socket-webhook")
		if err != nil {
			t.Fatalf("socket create: %v\noutput: %s", err, out)
		}
		harness.AssertOutputContains(t, out, "Webhook created!")

		out, err = h.CLISocket("webhook", "list")
		if err != nil {
			t.Fatalf("socket list: %v\noutput: %s", err, out)
		}
		harness.AssertOutputContains(t, out, "socket-webhook")
	})

	// ── 2. TCP + valid token: standard remote administration ─────────────────
	t.Run("tcp_with_token", func(t *testing.T) {
		out, err := h.CLI("webhook", "create", "tcp-webhook")
		if err != nil {
			t.Fatalf("tcp create: %v\noutput: %s", err, out)
		}
		harness.AssertOutputContains(t, out, "Webhook created!")

		out, err = h.CLI("consumer", "create", "tcp-consumer")
		if err != nil {
			t.Fatalf("tcp consumer create: %v\noutput: %s", err, out)
		}
		harness.AssertOutputContains(t, out, "Consumer created:")
		harness.AssertOutputContains(t, out, "Token:")
	})

	// ── 3. TCP without token: must fail ──────────────────────────────────────
	// The service always requires a bearer token on the TCP admin surface
	// when HOOKLET_ADMIN_TOKEN is configured (which the harness always does).
	t.Run("tcp_no_token_rejected", func(t *testing.T) {
		out, err := h.CLITCPNoToken("webhook", "list")
		if err == nil {
			t.Fatalf("expected non-zero exit; got success\noutput: %s", out)
		}
		// CLI prints "failed:" prefix wrapping the server error message
		if out == "" {
			t.Fatalf("expected error output, got nothing")
		}
	})

	// ── 4. TCP with wrong token: must fail ────────────────────────────────────
	t.Run("tcp_wrong_token_rejected", func(t *testing.T) {
		out, err := h.CLITCPWrongToken("webhook", "list")
		if err == nil {
			t.Fatalf("expected non-zero exit; got success\noutput: %s", out)
		}
	})

	// ── 5. Direct HTTP without Authorization header: server must return 401 ──
	// This bypasses the CLI entirely to confirm server-side enforcement.
	t.Run("direct_http_no_header_rejected", func(t *testing.T) {
		resp := h.Get(api.RouteAdminWebhooks)
		defer resp.Body.Close()
		harness.AssertStatus(t, resp, http.StatusUnauthorized)
	})
}

// ─────────────────────────────────────────────────────────────────────────────
// TestStatusEndpoint
//
// Verifies that /api/status returns a well-formed 200 response indicating
// the service is up and RabbitMQ is connected. This is the simplest possible
// sanity check that the whole stack (service + RabbitMQ) is wired together.
// ─────────────────────────────────────────────────────────────────────────────
func TestStatusEndpoint(t *testing.T) {
	h := harness.New(t, harness.WithRabbitPort(sharedRabbitPort))

	resp := h.Get(api.RouteStatus)
	defer resp.Body.Close()

	harness.AssertStatus(t, resp, http.StatusOK)
	harness.AssertBodyContains(t, resp, `"status":"ok"`)
	harness.AssertBodyContains(t, resp, `"rabbitmq":"connected"`)
}
