package server

import (
	"net/http"

	"hooklet/internal/httpcontract"
)

// newRouter initializes the main request multiplexer and registers routes.
func (s *Server) newRouter() *http.ServeMux {
	mux := http.NewServeMux()

	// Public Routes
	mux.HandleFunc(httpcontract.RouteStatus, s.handleStatus)
	mux.HandleFunc(httpcontract.RouteTopics, s.handleTopics)
	mux.HandleFunc(httpcontract.RoutePublish, s.handleWebhook)
	mux.HandleFunc(httpcontract.RouteSubscribe, s.handleWS)

	// Admin Routes (protected by middleware)
	// We wrap each handler with the admin auth middleware
	mux.HandleFunc("/admin/webhooks", s.adminAuthMiddleware(s.handleAdminWebhooks))
	mux.HandleFunc("/admin/webhooks/", s.adminAuthMiddleware(s.handleAdminWebhookByID))
	mux.HandleFunc("/admin/consumers", s.adminAuthMiddleware(s.handleAdminConsumers))
	mux.HandleFunc("/admin/consumers/", s.adminAuthMiddleware(s.handleAdminConsumerByID))

	return mux
}
