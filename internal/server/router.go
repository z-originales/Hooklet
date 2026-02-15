package server

import (
	"net/http"

	"hooklet/internal/api"
	"hooklet/internal/server/handlers"
)

// newRouter initializes the main request multiplexer and registers routes.
func (s *Server) newRouter() *http.ServeMux {
	mux := http.NewServeMux()

	adminHandler := handlers.NewAdminHandler(s.db)
	publicHandler := handlers.NewPublicHandler(s.listTopics, s.startedAt, s.rabbitConnected)
	webhookHandler := handlers.NewWebhookHandler(s.mq, s.db, s.trackTopic, s.cfg.MaxBodyBytes)
	wsHandler := handlers.NewWSHandler(s.mq, s.db, s.trackTopic, s.cfg)

	// Public Routes
	mux.HandleFunc(api.RouteStatus, publicHandler.Status)
	mux.HandleFunc(api.RouteTopics, publicHandler.TopicsList)
	mux.HandleFunc(api.RoutePublish, webhookHandler.Publish)
	mux.HandleFunc(api.RouteSubscribe, wsHandler.Subscribe)

	// Admin Routes (protected by middleware)
	mux.HandleFunc(api.RouteAdminWebhooks, s.adminAuthMiddleware(adminHandler.Webhooks))
	mux.HandleFunc(api.RouteAdminWebhooksN, s.adminAuthMiddleware(adminHandler.WebhookByID))
	mux.HandleFunc(api.RouteAdminConsumers, s.adminAuthMiddleware(adminHandler.Consumers))
	mux.HandleFunc(api.RouteAdminConsumerN, s.adminAuthMiddleware(adminHandler.ConsumerByID))

	return mux
}
