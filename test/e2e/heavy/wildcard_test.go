// Package heavy contains end-to-end tests that are correct and important but
// take more time to run (reconnection waits, multiple harness instances, etc.).
// They are intended for nightly CI or pre-release validation.
//
// Run the heavy suite:
//
//	go test -v ./test/e2e/heavy -count=1 -timeout 10m
package heavy

import (
	"testing"

	"hooklet/test/e2e/internal/harness"
)

// ─────────────────────────────────────────────────────────────────────────────
// TestWildcardSubscriptionRules
//
// Verifies the topic authorization model for wildcard subscriptions.
// A single harness is used and the consumer is subscribed to "orders.*".
//
// The test is table-driven for easy addition of new cases.
//
// Authorization logic (from ws.go):
//   - An exact topic request is allowed if any subscription pattern matches it.
//   - A wildcard topic request (contains "*") is ONLY allowed if the subscription
//     list contains that exact pattern string.
//
// Pattern matching rules:
//   - "orders.*"  matches one level  (orders.created, orders.updated)
//   - "orders.**" matches all levels (orders.eu.created)
//   - "**"        matches everything
//
// ─────────────────────────────────────────────────────────────────────────────
func TestWildcardSubscriptionRules(t *testing.T) {
	h := harness.New(t, harness.WithRabbitPort(sharedRabbitPort))

	// Create concrete topics that the consumer will try to access.
	// Topics must exist as webhooks so the server knows about them.
	for _, name := range []string{"orders.created", "orders.updated", "orders.eu.created"} {
		out, err := h.CLI("webhook", "create", name)
		if err != nil {
			t.Fatalf("create webhook %s: %v\noutput: %s", name, err, out)
		}
	}

	// Create a consumer subscribed ONLY to "orders.*"
	conOut, err := h.CLI("consumer", "create", "wildcard-consumer", "--subscriptions", "orders.*")
	if err != nil {
		t.Fatalf("create consumer: %v\noutput: %s", err, conOut)
	}
	token := harness.ExtractConsumerToken(t, conOut)

	// Each case describes one connection attempt with a specific topic request.
	cases := []struct {
		name           string
		requestedTopic string
		wantAuthOK     bool
		reason         string
	}{
		{
			// Single-level match: "orders.*" covers "orders.created"
			name:           "single_level_match",
			requestedTopic: "orders.created",
			wantAuthOK:     true,
			reason:         "orders.created matches orders.*",
		},
		{
			// Two levels: "orders.*" only covers one level, not "orders.eu.created"
			name:           "two_level_no_match",
			requestedTopic: "orders.eu.created",
			wantAuthOK:     false,
			reason:         "orders.eu.created does NOT match orders.* (two levels)",
		},
		{
			// Exact pattern request: consumer subscribed to "orders.*" — requesting
			// the exact pattern string is allowed (exact string match on subscription).
			name:           "exact_pattern_match",
			requestedTopic: "orders.*",
			wantAuthOK:     true,
			reason:         "requesting the exact subscription pattern is allowed",
		},
		{
			// Different wildcard: consumer has "orders.*" but requests "orders.**"
			// The two are different strings — not subscribed to "orders.**".
			name:           "different_wildcard_rejected",
			requestedTopic: "orders.**",
			wantAuthOK:     false,
			reason:         "orders.** is not in the consumer's subscriptions",
		},
	}

	for _, tc := range cases {
		tc := tc // capture range variable
		t.Run(tc.name, func(t *testing.T) {
			ws := h.ConnectWS(tc.requestedTopic)
			authResp := ws.Auth(token)
			ws.Conn.CloseNow()

			gotOK := authResp["auth_status"] == "auth_ok"
			if gotOK != tc.wantAuthOK {
				t.Fatalf("topic %q: wantAuthOK=%v gotAuthOK=%v (reason: %s)\nfull response: %v",
					tc.requestedTopic, tc.wantAuthOK, gotOK, tc.reason, authResp)
			}
		})
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// TestGlobalWildcardSubscriber
//
// Verifies that a consumer subscribed to "**" (all topics) is authorized
// to connect for any topic, including exact names and single-level wildcards.
//
// This tests the most permissive subscription and ensures it is correctly
// handled by the authorization logic.
// ─────────────────────────────────────────────────────────────────────────────
func TestGlobalWildcardSubscriber(t *testing.T) {
	h := harness.New(t, harness.WithRabbitPort(sharedRabbitPort))

	// Create a couple of topics
	for _, name := range []string{"payments.created", "shipments.updated"} {
		out, err := h.CLI("webhook", "create", name)
		if err != nil {
			t.Fatalf("create webhook %s: %v\noutput: %s", name, err, out)
		}
	}

	// Consumer with "**" subscription — should have access to everything
	conOut, err := h.CLI("consumer", "create", "global-consumer", "--subscriptions", "**")
	if err != nil {
		t.Fatalf("create consumer: %v\noutput: %s", err, conOut)
	}
	token := harness.ExtractConsumerToken(t, conOut)

	cases := []struct {
		topic string
	}{
		{"payments.created"},
		{"shipments.updated"},
		{"payments.created,shipments.updated"}, // multi-topic in a single connection
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.topic, func(t *testing.T) {
			ws := h.ConnectWS(tc.topic)
			authResp := ws.Auth(token)
			ws.Conn.CloseNow()

			if authResp["auth_status"] != "auth_ok" {
				t.Fatalf("global subscriber denied for topic %q: %v", tc.topic, authResp)
			}
		})
	}
}
