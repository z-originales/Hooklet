package server

import (
	"context"
	"io"
	"net/http"
	"strings"
	"time"

	"hooklet/internal/api"
	"hooklet/internal/store"

	"github.com/charmbracelet/log"
)

// handleWebhook receives POST requests and publishes to RabbitMQ.
// POST /api/webhook/{topic}
func (s *Server) handleWebhook(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Extract topic from path: /api/webhook/{topic}
	topic := strings.TrimPrefix(r.URL.Path, api.RoutePublish)
	if topic == "" {
		writeError(w, "Topic required", http.StatusBadRequest)
		return
	}

	// Validate webhook exists in DB
	// Strict Mode: The URL contains the topic_hash directly (e.g., /webhook/a1b2c3...).
	// This prevents topic enumeration - only those who know the hash can publish.
	// We look up the hash directly in the DB without re-hashing.
	topicHash := topic // The URL segment IS the hash
	wh, err := s.db.GetWebhookByHash(topicHash)
	if err != nil {
		log.Error("Failed to check webhook existence", "topic_hash", topicHash, "error", err)
		writeError(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	if wh == nil {
		log.Warn("Attempt to publish to non-existent webhook", "topic_hash", topicHash)
		writeError(w, "Webhook not found", http.StatusNotFound)
		return
	}

	// Verify producer authentication if webhook has a token configured
	if wh.HasToken && wh.TokenHash != nil {
		token := r.Header.Get(api.HeaderAuthToken)
		if token == "" {
			log.Warn("Missing auth token for protected webhook", "topic_hash", topicHash, "webhook", wh.Name)
			writeError(w, "Authentication required", http.StatusUnauthorized)
			return
		}
		if store.HashString(token) != *wh.TokenHash {
			log.Warn("Invalid auth token for webhook", "topic_hash", topicHash, "webhook", wh.Name)
			writeError(w, "Invalid token", http.StatusUnauthorized)
			return
		}
	}

	// Read body
	body, err := io.ReadAll(r.Body)
	if err != nil {
		log.Error("Failed to read body", "error", err)
		writeError(w, "Failed to read body", http.StatusInternalServerError)
		return
	}
	defer r.Body.Close()

	// Publish to queue
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	// Publish to queue using the webhook's original name as routing key
	// (RabbitMQ routing uses the human-readable name internally)
	if err := s.mq.Publish(ctx, wh.Name, body); err != nil {
		log.Error("Failed to publish", "topic", wh.Name, "error", err)
		writeError(w, "Failed to publish", http.StatusInternalServerError)
		return
	}

	// Track topic (by name, not hash)
	s.mu.Lock()
	s.topics[wh.Name] = struct{}{}
	s.mu.Unlock()

	log.Info("Webhook received", "topic", wh.Name, "hash", topicHash, "size", len(body))
	w.WriteHeader(http.StatusAccepted)
}
