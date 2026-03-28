package handlers

import (
	"context"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
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
	mq           *queue.Client
	db           *store.Store
	track        func(string)
	maxBodyBytes int64
}

// NewWebhookHandler creates a handler for webhook ingestion.
func NewWebhookHandler(mq *queue.Client, db *store.Store, trackTopic func(string), maxBodyBytes int64) *WebhookHandler {
	return &WebhookHandler{mq: mq, db: db, track: trackTopic, maxBodyBytes: maxBodyBytes}
}

// Publish handles POST /webhook/{topic}.
func (h *WebhookHandler) Publish(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		httpresponse.WriteError(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if h.maxBodyBytes > 0 {
		r.Body = http.MaxBytesReader(w, r.Body, h.maxBodyBytes)
	}

	// Extract webhook hash from path: /webhook/{hash}
	hookHash := strings.TrimPrefix(r.URL.Path, api.RoutePublish)
	if hookHash == "" {
		httpresponse.WriteError(w, "Webhook hash required", http.StatusBadRequest)
		return
	}

	// Validate webhook exists in DB
	// Strict Mode: The URL contains the topic_hash directly (e.g., /webhook/a1b2c3...).
	// This prevents topic enumeration - only those who know the hash can publish.
	wh, err := h.db.GetWebhookByHash(hookHash)
	if err != nil {
		log.Error("Failed to check webhook existence", "topic_hash", hookHash, "error", err)
		httpresponse.WriteError(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	if wh == nil {
		log.Warn("Attempt to publish to non-existent webhook", "topic_hash", hookHash)
		httpresponse.WriteError(w, "Webhook not found", http.StatusNotFound)
		return
	}

	// Verify producer authentication if webhook has a token configured
	if wh.HasToken && wh.TokenHash != nil {
		token := r.Header.Get(api.HeaderAuthToken)
		if token == "" {
			log.Warn("Missing auth token for protected webhook", "topic_hash", hookHash, "webhook", wh.Name)
			httpresponse.WriteError(w, "Authentication required", http.StatusUnauthorized)
			return
		}
		// Constant-time comparison to prevent timing attacks
		tokenHash := store.HashString(token)
		if subtle.ConstantTimeCompare([]byte(tokenHash), []byte(*wh.TokenHash)) != 1 {
			log.Warn("Invalid auth token for webhook", "topic_hash", hookHash, "webhook", wh.Name)
			httpresponse.WriteError(w, "Invalid token", http.StatusUnauthorized)
			return
		}
	}

	// Read body
	defer func() {
		if err := r.Body.Close(); err != nil {
			log.Debug("Failed to close request body", "error", err)
		}
	}()
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			httpresponse.WriteError(w, "Payload too large", http.StatusRequestEntityTooLarge)
			return
		}
		log.Error("Failed to read body", "error", err)
		httpresponse.WriteError(w, "Failed to read body", http.StatusInternalServerError)
		return
	}

	// Build envelope with transport metadata
	receivedAt := time.Now().UTC()
	env := api.WebhookEvent{
		Type:       "webhook",
		ID:         strconv.FormatInt(receivedAt.UnixNano(), 36),
		Topic:      wh.Name,
		ReceivedAt: receivedAt,
		Source: api.WebhookSource{
			WebhookID:   wh.ID,
			WebhookName: wh.Name,
		},
	}
	if json.Valid(body) {
		env.Data = json.RawMessage(body)
	} else {
		env.DataRawBase64 = base64.StdEncoding.EncodeToString(body)
	}

	envelopedBody, err := json.Marshal(env)
	if err != nil {
		log.Error("Failed to build webhook envelope", "topic", wh.Name, "error", err)
		httpresponse.WriteError(w, "Failed to process payload", http.StatusInternalServerError)
		return
	}

	// Publish to queue
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	// Publish to queue using the webhook's original name as routing key
	if err := h.mq.Publish(ctx, wh.Name, envelopedBody); err != nil {
		if errors.Is(err, queue.ErrNoRoute) {
			log.Warn("No consumer bound for webhook", "topic", wh.Name)
			httpresponse.WriteError(w, "No active consumer", http.StatusServiceUnavailable)
			return
		}
		log.Error("Failed to publish", "topic", wh.Name, "error", err)
		httpresponse.WriteError(w, "Failed to publish", http.StatusInternalServerError)
		return
	}

	// Track topic (by name, not hash)
	if h.track != nil {
		h.track(wh.Name)
	}

	log.Info("Webhook received", "topic", wh.Name, "hash", hookHash, "size", len(body))
	log.Debug("Webhook hit: message routed", "topic", wh.Name)
	w.WriteHeader(http.StatusAccepted)
}
