package handlers

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"hooklet/internal/server/auth"
	"hooklet/internal/server/httpresponse"
	"hooklet/internal/store"

	"github.com/charmbracelet/log"
)

// AdminHandler hosts admin routes.
type AdminHandler struct {
	db *store.Store
}

// NewAdminHandler creates a handler for admin endpoints.
func NewAdminHandler(db *store.Store) *AdminHandler {
	return &AdminHandler{db: db}
}

// Webhooks handles /admin/webhooks for GET and POST.
func (h *AdminHandler) Webhooks(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		webhooks, err := h.db.ListWebhooks()
		if err != nil {
			log.Error("Failed to list webhooks", "error", err)
			httpresponse.WriteError(w, "Internal Server Error", http.StatusInternalServerError)
			return
		}
		httpresponse.WriteJSON(w, webhooks)

	case http.MethodPost:
		var req struct {
			Name      string `json:"name"`
			WithToken bool   `json:"with_token"` // If true, generate an auth token for producers
		}
		if err := decodeJSON(r, &req); err != nil {
			httpresponse.WriteError(w, "Invalid request body", http.StatusBadRequest)
			return
		}
		if req.Name == "" {
			httpresponse.WriteError(w, "Name is required", http.StatusBadRequest)
			return
		}

		var wh *store.Webhook
		var token string
		var err error

		if req.WithToken {
			// Generate token for producer authentication
			token, err = auth.GenerateSecureToken()
			if err != nil {
				log.Error("Failed to generate token", "error", err)
				httpresponse.WriteError(w, "Internal Server Error", http.StatusInternalServerError)
				return
			}
			wh, err = h.db.CreateWebhookWithToken(req.Name, token)
		} else {
			wh, err = h.db.CreateWebhook(req.Name)
		}

		if err != nil {
			log.Error("Failed to create webhook", "error", err)
			// check for constraint error (duplicate name)
			if strings.Contains(err.Error(), "UNIQUE constraint failed") {
				httpresponse.WriteError(w, "Webhook name already exists", http.StatusConflict)
				return
			}
			httpresponse.WriteError(w, "Internal Server Error", http.StatusInternalServerError)
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
			httpresponse.WriteJSONSensitive(w, resp)
		} else {
			httpresponse.WriteJSON(w, wh)
		}

	default:
		httpresponse.WriteError(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

// WebhookByID handles /admin/webhooks/{id} and token subroutes.
func (h *AdminHandler) WebhookByID(w http.ResponseWriter, r *http.Request) {
	// Parse path: /admin/webhooks/{id}[/action]
	path := strings.TrimPrefix(r.URL.Path, "/admin/webhooks/")
	parts := strings.SplitN(path, "/", 2)

	idStr := parts[0]
	id, err := parseID(idStr)
	if err != nil {
		httpresponse.WriteError(w, "Invalid ID", http.StatusBadRequest)
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
			httpresponse.WriteError(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if err := h.db.DeleteWebhook(id); err != nil {
			log.Error("Failed to delete webhook", "error", err)
			httpresponse.WriteError(w, "Internal Server Error", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)

	case "set-token": // POST /admin/webhooks/{id}/set-token
		if r.Method != http.MethodPost {
			httpresponse.WriteError(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		// Verify webhook exists
		wh, err := h.db.GetWebhookByID(id)
		if err != nil {
			log.Error("Failed to get webhook", "error", err)
			httpresponse.WriteError(w, "Internal Server Error", http.StatusInternalServerError)
			return
		}
		if wh == nil {
			httpresponse.WriteError(w, "Webhook not found", http.StatusNotFound)
			return
		}

		// Generate new token
		token, err := auth.GenerateSecureToken()
		if err != nil {
			log.Error("Failed to generate token", "error", err)
			httpresponse.WriteError(w, "Internal Server Error", http.StatusInternalServerError)
			return
		}

		tokenHash := store.HashString(token)
		if err := h.db.SetWebhookToken(id, tokenHash); err != nil {
			log.Error("Failed to set webhook token", "error", err)
			httpresponse.WriteError(w, "Internal Server Error", http.StatusInternalServerError)
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
		httpresponse.WriteJSONSensitive(w, resp)

	case "clear-token": // POST /admin/webhooks/{id}/clear-token
		if r.Method != http.MethodPost {
			httpresponse.WriteError(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		// Verify webhook exists
		wh, err := h.db.GetWebhookByID(id)
		if err != nil {
			log.Error("Failed to get webhook", "error", err)
			httpresponse.WriteError(w, "Internal Server Error", http.StatusInternalServerError)
			return
		}
		if wh == nil {
			httpresponse.WriteError(w, "Webhook not found", http.StatusNotFound)
			return
		}

		if err := h.db.ClearWebhookToken(id); err != nil {
			log.Error("Failed to clear webhook token", "error", err)
			httpresponse.WriteError(w, "Internal Server Error", http.StatusInternalServerError)
			return
		}

		w.WriteHeader(http.StatusNoContent)

	default:
		httpresponse.WriteError(w, "Unknown action", http.StatusNotFound)
	}
}

// Consumers handles /admin/consumers for GET and POST.
func (h *AdminHandler) Consumers(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		consumers, err := h.db.ListConsumers()
		if err != nil {
			log.Error("Failed to list consumers", "error", err)
			httpresponse.WriteError(w, "Internal Server Error", http.StatusInternalServerError)
			return
		}
		httpresponse.WriteJSON(w, consumers)

	case http.MethodPost:
		var req struct {
			Name          string `json:"name"`
			Subscriptions string `json:"subscriptions"` // comma separated topics/patterns (use "**" for all)
		}
		if err := decodeJSON(r, &req); err != nil {
			httpresponse.WriteError(w, "Invalid request body", http.StatusBadRequest)
			return
		}
		if req.Name == "" {
			httpresponse.WriteError(w, "Name is required", http.StatusBadRequest)
			return
		}

		// Generate cryptographically secure token
		token, err := auth.GenerateSecureToken()
		if err != nil {
			log.Error("Failed to generate token", "error", err)
			httpresponse.WriteError(w, "Internal Server Error", http.StatusInternalServerError)
			return
		}

		consumer, err := h.db.CreateConsumer(req.Name, token)
		if err != nil {
			log.Error("Failed to create consumer", "error", err)
			if strings.Contains(err.Error(), "UNIQUE constraint failed") {
				httpresponse.WriteError(w, "Consumer name already exists", http.StatusConflict)
				return
			}
			httpresponse.WriteError(w, "Internal Server Error", http.StatusInternalServerError)
			return
		}

		// Set initial subscriptions if provided
		if req.Subscriptions != "" {
			if err := h.db.SetConsumerSubscriptions(consumer.ID, req.Subscriptions); err != nil {
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
		httpresponse.WriteJSONSensitive(w, resp)

	default:
		httpresponse.WriteError(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

// ConsumerByID handles DELETE, PATCH, and sub-routes for /admin/consumers/{id}.
func (h *AdminHandler) ConsumerByID(w http.ResponseWriter, r *http.Request) {
	// Parse the path: /admin/consumers/{id} or /admin/consumers/{id}/{action}
	path := strings.TrimPrefix(r.URL.Path, "/admin/consumers/")
	parts := strings.Split(path, "/")

	id, err := parseID(parts[0])
	if err != nil {
		httpresponse.WriteError(w, "Invalid ID", http.StatusBadRequest)
		return
	}

	// Check for sub-routes (subscribe/unsubscribe)
	if len(parts) > 1 {
		switch parts[1] {
		case "subscribe":
			h.handleConsumerSubscribe(w, r, id)
		case "unsubscribe":
			h.handleConsumerUnsubscribe(w, r, id)
		default:
			httpresponse.WriteError(w, "Unknown action", http.StatusNotFound)
		}
		return
	}

	// Handle main consumer routes
	switch r.Method {
	case http.MethodDelete:
		if err := h.db.DeleteConsumer(id); err != nil {
			if strings.Contains(err.Error(), "not found") {
				httpresponse.WriteError(w, "Consumer not found", http.StatusNotFound)
				return
			}
			log.Error("Failed to delete consumer", "error", err)
			httpresponse.WriteError(w, "Internal Server Error", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)

	case http.MethodPatch:
		var req struct {
			Subscriptions   *string `json:"subscriptions,omitempty"`
			RegenerateToken bool    `json:"regenerate_token,omitempty"`
		}
		if err := decodeJSON(r, &req); err != nil {
			httpresponse.WriteError(w, "Invalid request body", http.StatusBadRequest)
			return
		}

		if req.Subscriptions != nil {
			if err := h.db.SetConsumerSubscriptions(id, *req.Subscriptions); err != nil {
				if strings.Contains(err.Error(), "not found") {
					httpresponse.WriteError(w, "Consumer not found", http.StatusNotFound)
					return
				}
				log.Error("Failed to update consumer", "error", err)
				httpresponse.WriteError(w, "Internal Server Error", http.StatusInternalServerError)
				return
			}
		}

		if req.RegenerateToken {
			newToken, err := auth.GenerateSecureToken()
			if err != nil {
				log.Error("Failed to generate token", "error", err)
				httpresponse.WriteError(w, "Internal Server Error", http.StatusInternalServerError)
				return
			}
			newHash := store.HashString(newToken)
			if err := h.db.RegenerateConsumerToken(id, newHash); err != nil {
				if strings.Contains(err.Error(), "not found") {
					httpresponse.WriteError(w, "Consumer not found", http.StatusNotFound)
					return
				}
				log.Error("Failed to regenerate token", "error", err)
				httpresponse.WriteError(w, "Internal Server Error", http.StatusInternalServerError)
				return
			}
			httpresponse.WriteJSONSensitive(w, map[string]string{"token": newToken})
			return
		}

		w.WriteHeader(http.StatusNoContent)

	default:
		httpresponse.WriteError(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

// handleConsumerSubscribe adds a subscription for a consumer.
// POST /admin/consumers/{id}/subscribe
func (h *AdminHandler) handleConsumerSubscribe(w http.ResponseWriter, r *http.Request, consumerID int64) {
	if r.Method != http.MethodPost {
		httpresponse.WriteError(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		Topic string `json:"topic"`
	}
	if err := decodeJSON(r, &req); err != nil {
		httpresponse.WriteError(w, "Invalid request body", http.StatusBadRequest)
		return
	}
	if req.Topic == "" {
		httpresponse.WriteError(w, "Topic is required", http.StatusBadRequest)
		return
	}

	if err := h.db.Subscribe(consumerID, req.Topic); err != nil {
		log.Error("Failed to subscribe", "consumer_id", consumerID, "topic", req.Topic, "error", err)
		httpresponse.WriteError(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// handleConsumerUnsubscribe removes a subscription from a consumer.
// POST /admin/consumers/{id}/unsubscribe
func (h *AdminHandler) handleConsumerUnsubscribe(w http.ResponseWriter, r *http.Request, consumerID int64) {
	if r.Method != http.MethodPost {
		httpresponse.WriteError(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		Topic string `json:"topic"`
	}
	if err := decodeJSON(r, &req); err != nil {
		httpresponse.WriteError(w, "Invalid request body", http.StatusBadRequest)
		return
	}
	if req.Topic == "" {
		httpresponse.WriteError(w, "Topic is required", http.StatusBadRequest)
		return
	}

	if err := h.db.Unsubscribe(consumerID, req.Topic); err != nil {
		if strings.Contains(err.Error(), "not found") {
			httpresponse.WriteError(w, "Subscription not found", http.StatusNotFound)
			return
		}
		log.Error("Failed to unsubscribe", "consumer_id", consumerID, "topic", req.Topic, "error", err)
		httpresponse.WriteError(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// parseID parses a string ID to int64.
func parseID(s string) (int64, error) {
	// Simple wrapper, could use strconv directly but this keeps imports cleaner if we want
	// to add more complex validation later.
	var id int64
	_, err := fmt.Sscanf(s, "%d", &id)
	return id, err
}

func decodeJSON(r *http.Request, dst any) error {
	return json.NewDecoder(r.Body).Decode(dst)
}
