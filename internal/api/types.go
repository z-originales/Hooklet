// Package api defines shared types between service and CLI.
package api

import (
	"encoding/json"
	"time"
)

// StatusResponse is returned by the status endpoint.
type StatusResponse struct {
	Status    string    `json:"status"`
	Uptime    string    `json:"uptime"`
	StartedAt time.Time `json:"started_at"`
	RabbitMQ  string    `json:"rabbitmq"` // "connected" or "disconnected"
}

// ErrorResponse is returned on API errors.
type ErrorResponse struct {
	Error string `json:"error"`
}

// WebhookSource identifies the webhook configuration that produced an event.
type WebhookSource struct {
	WebhookID   int64  `json:"webhook_id"`
	WebhookName string `json:"webhook_name"`
}

// WebhookEvent is the message format streamed to WebSocket consumers.
// It wraps the original webhook payload with routing and tracing metadata.
type WebhookEvent struct {
	Type          string          `json:"type"`
	ID            string          `json:"id"`
	Topic         string          `json:"topic"`
	ReceivedAt    time.Time       `json:"received_at"`
	Source        WebhookSource   `json:"source"`
	Data          json.RawMessage `json:"data,omitempty"`
	DataRawBase64 string          `json:"data_raw_base64,omitempty"`
}
