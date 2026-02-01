// Package api defines shared headers and query params between service and CLI.
package api

// Authentication header for webhook ingestion and WebSocket connections.
const HeaderAuthToken = "X-Hooklet-Token"

// QueryParamTopics is the URL query parameter for specifying webhook subscriptions.
// Example: /ws?topics=webhook1,webhook2,webhook3
const QueryParamTopics = "topics"
