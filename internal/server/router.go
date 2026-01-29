package server

import (
	"net/http"

	"hooklet/internal/api"
)

// newRouter initializes the main request multiplexer and registers routes.
func (s *Server) newRouter() *http.ServeMux {
	mux := http.NewServeMux()

	// Public Routes
	mux.HandleFunc(api.RouteStatus, s.handleStatus)
	mux.HandleFunc(api.RouteTopics, s.handleTopics)
	mux.HandleFunc(api.RoutePublish, s.handleWebhook)
	mux.HandleFunc(api.RouteSubscribe, s.handleWS)

	// Admin Routes (protected by middleware)
	// We wrap each handler with the admin auth middleware
	mux.HandleFunc("/admin/webhooks", s.adminAuthMiddleware(s.handleAdminWebhooks))
	mux.HandleFunc("/admin/webhooks/", s.adminAuthMiddleware(s.handleAdminWebhookByID))
	mux.HandleFunc("/admin/consumers", s.adminAuthMiddleware(s.handleAdminConsumers))
	mux.HandleFunc("/admin/consumers/", s.adminAuthMiddleware(s.handleAdminConsumerByID))

	return mux
}
