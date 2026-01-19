// Package api defines shared types and constants between service and CLI.
package api

import "time"

// API route constants
const (
	// Service endpoints
	RouteStatus    = "/api/status"
	RoutePublish   = "/api/webhook/" // + {topic}
	RouteTopics    = "/api/topics"
	RouteSubscribe = "/ws/" // + {topic}
)

// Default configuration
const (
	DefaultPort      = "8080"
	DefaultHost      = "localhost"
	DefaultRabbitURL = "amqp://guest:guest@localhost:5672/"
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

// TODO: Add authentication header constant when implementing security
// const HeaderAuthToken = "X-Hooklet-Token"
