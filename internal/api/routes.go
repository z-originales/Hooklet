// Package api defines shared API routes between service and CLI.
package api

// API route constants
const (
	// Service endpoints
	RouteStatus    = "/api/status"
	RoutePublish   = "/webhook/" // + {hash}
	RouteTopics    = "/api/topics"
	RouteSubscribe = "/ws/" // + {topic}
)
