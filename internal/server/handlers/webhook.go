package handlers

import (
	"context"
	"io"
	"net/http"
	"strings"
	"time"

	"hooklet/internal/api"
	"hooklet/internal/queue"
	"hooklet/internal/server/httpresponse"
	"hooklet/internal/store"

	"github.com/charmbracelet/log"
)

// WebhookHandler receives POST requests and publishes to RabbitMQ.
type WebhookHandler struct {
	mq    *queue.Client
	db    *store.Store
	track func(string)
}

// NewWebhookHandler creates a handler for webhook ingestion.
func NewWebhookHandler(mq *queue.Client, db *store.Store, trackTopic func(string)) *WebhookHandler {
	return &WebhookHandler{mq: mq, db: db, track: trackTopic}
}

// Publish handles POST /webhook/{topic}.
func (h *WebhookHandler) Publish(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		httpresponse.WriteError(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Extract topic from path: /webhook/{topic}
	topic := strings.TrimPrefix(r.URL.Path, api.RoutePublish)
	if topic == "" {
		httpresponse.WriteError(w, "Topic required", http.StatusBadRequest)
		return
	}

	// Validate webhook exists in DB
	// Strict Mode: The URL contains the topic_hash directly (e.g., /webhook/a1b2c3...).
	// This prevents topic enumeration - only those who know the hash can publish.
	// We look up the hash directly in the DB without re-hashing.
	topicHash := topic // The URL segment IS the hash
	wh, err := h.db.GetWebhookByHash(topicHash)
	if err != nil {
		log.Error("Failed to check webhook existence", "topic_hash", topicHash, "error", err)
		httpresponse.WriteError(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	if wh == nil {
		log.Warn("Attempt to publish to non-existent webhook", "topic_hash", topicHash)
		httpresponse.WriteError(w, "Webhook not found", http.StatusNotFound)
		return
	}

	// Verify producer authentication if webhook has a token configured
	if wh.HasToken && wh.TokenHash != nil {
		token := r.Header.Get(api.HeaderAuthToken)
		if token == "" {
			log.Warn("Missing auth token for protected webhook", "topic_hash", topicHash, "webhook", wh.Name)
			httpresponse.WriteError(w, "Authentication required", http.StatusUnauthorized)
			return
		}
		if store.HashString(token) != *wh.TokenHash {
			log.Warn("Invalid auth token for webhook", "topic_hash", topicHash, "webhook", wh.Name)
			httpresponse.WriteError(w, "Invalid token", http.StatusUnauthorized)
			return
		}
	}

	// Read body
	defer r.Body.Close()
	body, err := io.ReadAll(r.Body)
	if err != nil {
		log.Error("Failed to read body", "error", err)
		httpresponse.WriteError(w, "Failed to read body", http.StatusInternalServerError)
		return
	}

	// Publish to queue
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	// Publish to queue using the webhook's original name as routing key
	// (RabbitMQ routing uses the human-readable name internally)
	if err := h.mq.Publish(ctx, wh.Name, body); err != nil {
		log.Error("Failed to publish", "topic", wh.Name, "error", err)
		httpresponse.WriteError(w, "Failed to publish", http.StatusInternalServerError)
		return
	}

	// Track topic (by name, not hash)
	if h.track != nil {
		h.track(wh.Name)
	}

	log.Info("Webhook received", "topic", wh.Name, "hash", topicHash, "size", len(body))
	w.WriteHeader(http.StatusAccepted)
}
