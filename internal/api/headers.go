// Package api defines shared headers and query params between service and CLI.
package api

// HeaderAuthToken is the custom authentication header for webhook ingestion.
const HeaderAuthToken = "X-Hooklet-Token"

// HeaderAuthorization is the standard HTTP Authorization header.
// Used for WebSocket authentication: "Authorization: Bearer <token>"
const HeaderAuthorization = "Authorization"

// BearerPrefix is the prefix for Bearer token authentication.
const BearerPrefix = "Bearer "

// QueryParamTopics is the URL query parameter for specifying webhook subscriptions.
// Example: /ws?topics=webhook1,webhook2,webhook3
const QueryParamTopics = "topics"
