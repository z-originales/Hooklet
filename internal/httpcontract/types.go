// Package httpcontract defines shared types and constants between service and CLI.
package httpcontract

import "time"

// API route constants
const (
	// Service endpoints
	RouteStatus    = "/api/status"
	RoutePublish   = "/webhook/" // + {topic}
	RouteTopics    = "/api/topics"
	RouteSubscribe = "/ws/" // + {topic}
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

// ErrorResponse is returned on API errors.
type ErrorResponse struct {
	Error string `json:"error"`
}

// Authentication header for webhook ingestion and WebSocket connections.
const HeaderAuthToken = "X-Hooklet-Token"

// QueryParamTopics is the URL query parameter for specifying webhook subscriptions.
// Example: /ws?topics=webhook1,webhook2,webhook3
const QueryParamTopics = "topics"
