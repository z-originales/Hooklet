// Package api defines shared types and constants between service and CLI.
package api

import "time"

// API route constants
const (
	// Service endpoints
	RouteStatus    = "/api/status"
	RoutePublish   = "/webhook/" // + {topic}
	RouteTopics    = "/api/topics"
	RouteSubscribe = "/ws/" // + {topic}
)

// Default configuration
const (
	DefaultPort       = "8080"
	DefaultHost       = "localhost"
	DefaultRabbitHost = "localhost"
	DefaultRabbitPort = "5672"
	DefaultRabbitUser = "guest"
	DefaultRabbitPass = "guest"
	DefaultSocketPath = "./hooklet.sock"

	// DefaultMessageTTL is how long messages stay in a queue before expiring (milliseconds).
	// If a consumer doesn't read a message within this time, it's discarded.
	// Override with HOOKLET_MESSAGE_TTL environment variable.
	DefaultMessageTTL = 300000 // 5 minutes

	// DefaultQueueExpiry is how long an unused queue persists before auto-deletion (milliseconds).
	// If no consumer connects to a queue within this time, RabbitMQ deletes it.
	// Override with HOOKLET_QUEUE_EXPIRY environment variable.
	DefaultQueueExpiry = 3600000 // 1 hour
)

// StatusResponse is returned by the status endpoint.
type StatusResponse struct {
	Status    string    `json:"status"`
	Uptime    string    `json:"uptime"`
	StartedAt time.Time `json:"started_at"`
	RabbitMQ  string    `json:"rabbitmq"` // "connected" or "disconnected"
}

// TopicsResponse lists active topics/queues.
type TopicsResponse struct {
	Topics []string `json:"topics"`
}

// PublishRequest represents a message to publish (used by CLI).
type PublishRequest struct {
	Topic   string `json:"topic"`
	Payload []byte `json:"payload"`
}

// ErrorResponse is returned on API errors.
type ErrorResponse struct {
	Error string `json:"error"`
}

// Authentication header for webhook ingestion and WebSocket connections.
const HeaderAuthToken = "X-Hooklet-Token"

// QueryParamTopics is the URL query parameter for specifying webhook subscriptions.
// Example: /ws?topics=webhook1,webhook2,webhook3
const QueryParamTopics = "topics"
