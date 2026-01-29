package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"hooklet/internal/api"
	"hooklet/internal/store"

	"github.com/charmbracelet/log"
)

// handleStatus returns service health information.
// GET /api/status
func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	rabbitStatus := "disconnected"
	if s.mq.IsConnected() {
		rabbitStatus = "connected"
	}

	status := api.StatusResponse{
		Status:    "ok",
		Uptime:    time.Since(s.startedAt).Round(time.Second).String(),
		StartedAt: s.startedAt,
		RabbitMQ:  rabbitStatus,
	}

	writeJSON(w, status)
}

// handleTopics returns the list of active topics.
// GET /api/topics
func (s *Server) handleTopics(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	s.mu.RLock()
	topics := make([]string, 0, len(s.topics))
	for t := range s.topics {
		topics = append(topics, t)
	}
	s.mu.RUnlock()

	writeJSON(w, api.TopicsResponse{Topics: topics})
}

// Admin Handlers

// POST /admin/webhooks (create)
// GET /admin/webhooks (list)
func (s *Server) handleAdminWebhooks(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		webhooks, err := s.db.ListWebhooks()
		if err != nil {
			log.Error("Failed to list webhooks", "error", err)
			writeError(w, "Internal Server Error", http.StatusInternalServerError)
			return
		}
		writeJSON(w, webhooks)

	case http.MethodPost:
		var req struct {
			Name      string `json:"name"`
			WithToken bool   `json:"with_token"` // If true, generate an auth token for producers
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, "Invalid request body", http.StatusBadRequest)
			return
		}
		if req.Name == "" {
			writeError(w, "Name is required", http.StatusBadRequest)
			return
		}

		var wh *store.Webhook
		var token string
		var err error

		if req.WithToken {
			// Generate token for producer authentication
			token, err = generateSecureToken()
			if err != nil {
				log.Error("Failed to generate token", "error", err)
				writeError(w, "Internal Server Error", http.StatusInternalServerError)
				return
			}
			wh, err = s.db.CreateWebhookWithToken(req.Name, token)
		} else {
			wh, err = s.db.CreateWebhook(req.Name)
		}

		if err != nil {
			log.Error("Failed to create webhook", "error", err)
			// check for constraint error (duplicate name)
			if strings.Contains(err.Error(), "UNIQUE constraint failed") {
				writeError(w, "Webhook name already exists", http.StatusConflict)
				return
			}
			writeError(w, "Internal Server Error", http.StatusInternalServerError)
			return
		}

		// If token was generated, return it (only shown once)
		if token != "" {
			resp := struct {
				*store.Webhook
				Token string `json:"token,omitempty"`
			}{
				Webhook: wh,
				Token:   token,
			}
			writeJSON(w, resp)
		} else {
			writeJSON(w, wh)
		}

	default:
		writeError(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

// /admin/webhooks/{id} - DELETE (delete webhook)
// /admin/webhooks/{id}/set-token - POST (generate/regenerate token)
// /admin/webhooks/{id}/clear-token - POST (remove token, disable auth)
func (s *Server) handleAdminWebhookByID(w http.ResponseWriter, r *http.Request) {
	// Parse path: /admin/webhooks/{id}[/action]
	path := strings.TrimPrefix(r.URL.Path, "/admin/webhooks/")
	parts := strings.SplitN(path, "/", 2)

	idStr := parts[0]
	id, err := parseID(idStr)
	if err != nil {
		writeError(w, "Invalid ID", http.StatusBadRequest)
		return
	}

	// Determine action
	action := ""
	if len(parts) > 1 {
		action = parts[1]
	}

	switch action {
	case "": // DELETE /admin/webhooks/{id}
		if r.Method != http.MethodDelete {
			writeError(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if err := s.db.DeleteWebhook(id); err != nil {
			log.Error("Failed to delete webhook", "error", err)
			writeError(w, "Internal Server Error", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)

	case "set-token": // POST /admin/webhooks/{id}/set-token
		if r.Method != http.MethodPost {
			writeError(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		// Verify webhook exists
		wh, err := s.db.GetWebhookByID(id)
		if err != nil {
			log.Error("Failed to get webhook", "error", err)
			writeError(w, "Internal Server Error", http.StatusInternalServerError)
			return
		}
		if wh == nil {
			writeError(w, "Webhook not found", http.StatusNotFound)
			return
		}

		// Generate new token
		token, err := generateSecureToken()
		if err != nil {
			log.Error("Failed to generate token", "error", err)
			writeError(w, "Internal Server Error", http.StatusInternalServerError)
			return
		}

		tokenHash := store.HashString(token)
		if err := s.db.SetWebhookToken(id, tokenHash); err != nil {
			log.Error("Failed to set webhook token", "error", err)
			writeError(w, "Internal Server Error", http.StatusInternalServerError)
			return
		}

		// Return token (shown only once)
		resp := struct {
			ID    int64  `json:"id"`
			Name  string `json:"name"`
			Token string `json:"token"`
		}{
			ID:    wh.ID,
			Name:  wh.Name,
			Token: token,
		}
		writeJSON(w, resp)

	case "clear-token": // POST /admin/webhooks/{id}/clear-token
		if r.Method != http.MethodPost {
			writeError(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		// Verify webhook exists
		wh, err := s.db.GetWebhookByID(id)
		if err != nil {
			log.Error("Failed to get webhook", "error", err)
			writeError(w, "Internal Server Error", http.StatusInternalServerError)
			return
		}
		if wh == nil {
			writeError(w, "Webhook not found", http.StatusNotFound)
			return
		}

		if err := s.db.ClearWebhookToken(id); err != nil {
			log.Error("Failed to clear webhook token", "error", err)
			writeError(w, "Internal Server Error", http.StatusInternalServerError)
			return
		}

		w.WriteHeader(http.StatusNoContent)

	default:
		writeError(w, "Unknown action", http.StatusNotFound)
	}
}

// POST /admin/consumers (create)
// GET /admin/consumers (list)
func (s *Server) handleAdminConsumers(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		consumers, err := s.db.ListConsumers()
		if err != nil {
			log.Error("Failed to list consumers", "error", err)
			writeError(w, "Internal Server Error", http.StatusInternalServerError)
			return
		}
		writeJSON(w, consumers)

	case http.MethodPost:
		var req struct {
			Name          string `json:"name"`
			Subscriptions string `json:"subscriptions"` // comma separated topics/patterns (use "**" for all)
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, "Invalid request body", http.StatusBadRequest)
			return
		}
		if req.Name == "" {
			writeError(w, "Name is required", http.StatusBadRequest)
			return
		}

		// Generate cryptographically secure token
		token, err := generateSecureToken()
		if err != nil {
			log.Error("Failed to generate token", "error", err)
			writeError(w, "Internal Server Error", http.StatusInternalServerError)
			return
		}

		consumer, err := s.db.CreateConsumer(req.Name, token)
		if err != nil {
			log.Error("Failed to create consumer", "error", err)
			if strings.Contains(err.Error(), "UNIQUE constraint failed") {
				writeError(w, "Consumer name already exists", http.StatusConflict)
				return
			}
			writeError(w, "Internal Server Error", http.StatusInternalServerError)
			return
		}

		// Set initial subscriptions if provided
		if req.Subscriptions != "" {
			if err := s.db.SetConsumerSubscriptions(consumer.ID, req.Subscriptions); err != nil {
				log.Error("Failed to set subscriptions", "error", err)
				// Consumer created but subscriptions failed - still return success with warning
			}
		}

		// Return the token to the admin only once
		resp := struct {
			*store.Consumer
			Token string `json:"token"`
		}{
			Consumer: consumer,
			Token:    token,
		}
		writeJSON(w, resp)

	default:
		writeError(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

// handleAdminConsumerByID handles DELETE, PATCH, and sub-routes for /admin/consumers/{id}
// Supports:
//   - DELETE /admin/consumers/{id} - Delete consumer
//   - PATCH /admin/consumers/{id} - Update subscriptions or regenerate token
//   - POST /admin/consumers/{id}/subscribe - Add a subscription
//   - POST /admin/consumers/{id}/unsubscribe - Remove a subscription
func (s *Server) handleAdminConsumerByID(w http.ResponseWriter, r *http.Request) {
	// Parse the path: /admin/consumers/{id} or /admin/consumers/{id}/{action}
	path := strings.TrimPrefix(r.URL.Path, "/admin/consumers/")
	parts := strings.Split(path, "/")

	id, err := parseID(parts[0])
	if err != nil {
		writeError(w, "Invalid ID", http.StatusBadRequest)
		return
	}

	// Check for sub-routes (subscribe/unsubscribe)
	if len(parts) > 1 {
		switch parts[1] {
		case "subscribe":
			s.handleConsumerSubscribe(w, r, id)
		case "unsubscribe":
			s.handleConsumerUnsubscribe(w, r, id)
		default:
			writeError(w, "Unknown action", http.StatusNotFound)
		}
		return
	}

	// Handle main consumer routes
	switch r.Method {
	case http.MethodDelete:
		if err := s.db.DeleteConsumer(id); err != nil {
			if strings.Contains(err.Error(), "not found") {
				writeError(w, "Consumer not found", http.StatusNotFound)
				return
			}
			log.Error("Failed to delete consumer", "error", err)
			writeError(w, "Internal Server Error", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)

	case http.MethodPatch:
		var req struct {
			Subscriptions   *string `json:"subscriptions,omitempty"`
			RegenerateToken bool    `json:"regenerate_token,omitempty"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, "Invalid request body", http.StatusBadRequest)
			return
		}

		if req.Subscriptions != nil {
			if err := s.db.SetConsumerSubscriptions(id, *req.Subscriptions); err != nil {
				if strings.Contains(err.Error(), "not found") {
					writeError(w, "Consumer not found", http.StatusNotFound)
					return
				}
				log.Error("Failed to update consumer", "error", err)
				writeError(w, "Internal Server Error", http.StatusInternalServerError)
				return
			}
		}

		if req.RegenerateToken {
			newToken, err := generateSecureToken()
			if err != nil {
				log.Error("Failed to generate token", "error", err)
				writeError(w, "Internal Server Error", http.StatusInternalServerError)
				return
			}
			newHash := store.HashString(newToken)
			if err := s.db.RegenerateConsumerToken(id, newHash); err != nil {
				if strings.Contains(err.Error(), "not found") {
					writeError(w, "Consumer not found", http.StatusNotFound)
					return
				}
				log.Error("Failed to regenerate token", "error", err)
				writeError(w, "Internal Server Error", http.StatusInternalServerError)
				return
			}
			writeJSON(w, map[string]string{"token": newToken})
			return
		}

		w.WriteHeader(http.StatusNoContent)

	default:
		writeError(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

// handleConsumerSubscribe adds a subscription for a consumer.
// POST /admin/consumers/{id}/subscribe
func (s *Server) handleConsumerSubscribe(w http.ResponseWriter, r *http.Request, consumerID int64) {
	if r.Method != http.MethodPost {
		writeError(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		Topic string `json:"topic"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, "Invalid request body", http.StatusBadRequest)
		return
	}
	if req.Topic == "" {
		writeError(w, "Topic is required", http.StatusBadRequest)
		return
	}

	if err := s.db.Subscribe(consumerID, req.Topic); err != nil {
		log.Error("Failed to subscribe", "consumer_id", consumerID, "topic", req.Topic, "error", err)
		writeError(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// handleConsumerUnsubscribe removes a subscription from a consumer.
// POST /admin/consumers/{id}/unsubscribe
func (s *Server) handleConsumerUnsubscribe(w http.ResponseWriter, r *http.Request, consumerID int64) {
	if r.Method != http.MethodPost {
		writeError(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		Topic string `json:"topic"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, "Invalid request body", http.StatusBadRequest)
		return
	}
	if req.Topic == "" {
		writeError(w, "Topic is required", http.StatusBadRequest)
		return
	}

	if err := s.db.Unsubscribe(consumerID, req.Topic); err != nil {
		if strings.Contains(err.Error(), "not found") {
			writeError(w, "Subscription not found", http.StatusNotFound)
			return
		}
		log.Error("Failed to unsubscribe", "consumer_id", consumerID, "topic", req.Topic, "error", err)
		writeError(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// Helpers for this file

// parseID parses a string ID to int64.
func parseID(s string) (int64, error) {
	// Simple wrapper, could use strconv directly but this keeps imports cleaner if we want
	// to add more complex validation later.
	var id int64
	_, err := fmt.Sscanf(s, "%d", &id)
	return id, err
}
