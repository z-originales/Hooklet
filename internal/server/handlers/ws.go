package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"hooklet/internal/api"
	"hooklet/internal/queue"
	"hooklet/internal/server/auth"
	"hooklet/internal/store"

	"github.com/charmbracelet/log"
	"github.com/coder/websocket"
)

// WSHandler upgrades to WebSocket and streams messages from RabbitMQ.
type WSHandler struct {
	mq    *queue.Client
	db    *store.Store
	track func(string)
}

// NewWSHandler creates a handler for WebSocket streaming.
func NewWSHandler(mq *queue.Client, db *store.Store, trackTopic func(string)) *WSHandler {
	return &WSHandler{mq: mq, db: db, track: trackTopic}
}

// extractBearerToken extracts the token from "Authorization: Bearer <token>" header.
func extractBearerToken(r *http.Request) string {
	header := r.Header.Get(api.HeaderAuthorization)
	if strings.HasPrefix(header, api.BearerPrefix) {
		return strings.TrimPrefix(header, api.BearerPrefix)
	}
	return ""
}

// Subscribe handles GET /ws?topics=t1,t2 and /ws/{topic}.
func (h *WSHandler) Subscribe(w http.ResponseWriter, r *http.Request) {
	const writeTimeout = 10 * time.Second

	// Parse topics from URL query params
	var topics []string
	if t := r.URL.Query().Get(api.QueryParamTopics); t != "" {
		for _, name := range strings.Split(t, ",") {
			name = strings.TrimSpace(name)
			if name != "" {
				topics = append(topics, name)
			}
		}
	}

	// Also support topic from path for backward compatibility / convenience
	// /ws/{topic}
	pathTopic := strings.TrimPrefix(r.URL.Path, api.RouteSubscribe)
	if pathTopic != "" && pathTopic != "/" {
		topics = append(topics, pathTopic)
	}

	if len(topics) == 0 {
		http.Error(w, "No topics specified", http.StatusBadRequest)
		return
	}

	var consumer *store.Consumer
	var isAdminBypass bool

	// 1. Check admin bypass (CLI via Unix socket)
	if auth.IsAdminBypass(r.Context()) {
		consumer = &store.Consumer{ID: 0, Name: "admin-cli"}
		isAdminBypass = true
		log.Debug("WebSocket admin bypass active")
	} else if token := extractBearerToken(r); token != "" {
		// 2. Header-based auth: validate BEFORE upgrading WebSocket
		var err error
		consumer, err = h.db.GetConsumerByToken(token)
		if err != nil {
			log.Error("Failed to validate bearer token", "error", err)
			http.Error(w, "Internal error", http.StatusInternalServerError)
			return
		}
		if consumer == nil {
			log.Warn("WebSocket rejected: invalid bearer token", "remote", r.RemoteAddr)
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		log.Debug("WebSocket pre-authenticated via header", "consumer", consumer.Name)
	}

	// Accept WebSocket connection
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		InsecureSkipVerify: true, // Allow all origins for POC
	})
	if err != nil {
		log.Error("Failed to accept websocket", "error", err)
		return
	}

	// 3. If not yet authenticated, wait for auth message (fallback for browser clients)
	if consumer == nil {
		authCtx, authCancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer authCancel()

		_, authMsg, err := conn.Read(authCtx)
		if err != nil {
			log.Warn("WebSocket auth timeout or read error", "remote", r.RemoteAddr, "error", err)
			conn.Close(websocket.StatusPolicyViolation, "Authentication timeout")
			return
		}

		var authReq struct {
			Type  string `json:"type"`
			Token string `json:"token"`
		}
		if err := json.Unmarshal(authMsg, &authReq); err != nil || authReq.Type != "auth" || authReq.Token == "" {
			log.Warn("WebSocket invalid auth message", "remote", r.RemoteAddr)
			conn.Close(websocket.StatusPolicyViolation, "Invalid auth message")
			return
		}

		consumer, err = h.db.GetConsumerByToken(authReq.Token)
		if err != nil {
			log.Error("Failed to validate token", "error", err)
			conn.Close(websocket.StatusInternalError, "Internal error")
			return
		}
		if consumer == nil {
			log.Warn("WebSocket connection with invalid token", "remote", r.RemoteAddr)
			conn.Close(websocket.StatusPolicyViolation, "Invalid token")
			return
		}
	}

	// Authorization Check (Subscriptions)
	// Admin bypass has access to everything, regular consumers need explicit permission
	if !isAdminBypass {
		for _, t := range topics {
			allowed, err := h.db.ConsumerCanAccess(consumer.ID, t)
			if err != nil {
				log.Error("Failed to check consumer access", "error", err)
				conn.Close(websocket.StatusInternalError, "Internal error")
				return
			}
			if !allowed {
				log.Warn("Consumer tried to subscribe to unauthorized topic", "consumer", consumer.Name, "topic", t)
				conn.Close(websocket.StatusPolicyViolation, "Unauthorized topic: "+t)
				return
			}
		}
	}

	// Send auth success acknowledgement
	ack := map[string]string{"type": "auth_ok", "consumer": consumer.Name}
	ackBytes, _ := json.Marshal(ack)
	ackCtx, ackCancel := context.WithTimeout(r.Context(), writeTimeout)
	if err := conn.Write(ackCtx, websocket.MessageText, ackBytes); err != nil {
		ackCancel()
		log.Error("Failed to send auth ack", "error", err)
		conn.Close(websocket.StatusInternalError, "Failed to send ack")
		return
	}
	ackCancel()

	// Consumer ID is now the Consumer ID / Name to ensure tracking
	consumerID := fmt.Sprintf("%s-%d", consumer.Name, consumer.ID)

	log.Info("WebSocket client authenticated", "consumer_id", consumerID, "topics", topics, "remote", r.RemoteAddr)

	// Track topics
	if h.track != nil {
		for _, t := range topics {
			h.track(t)
		}
	}

	// Subscribe to queue with specific topics
	msgs, err := h.mq.Subscribe(consumerID, topics)
	if err != nil {
		log.Error("Failed to subscribe", "consumer_id", consumerID, "error", err)
		conn.Close(websocket.StatusInternalError, "Failed to subscribe")
		return
	}

	// Stream messages to WebSocket
	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			log.Info("WebSocket client disconnected", "consumer_id", consumerID)
			conn.Close(websocket.StatusNormalClosure, "")
			return
		case msg, ok := <-msgs:
			if !ok {
				log.Info("Queue channel closed", "consumer_id", consumerID)
				conn.Close(websocket.StatusNormalClosure, "Queue closed")
				return
			}
			writeCtx, writeCancel := context.WithTimeout(ctx, writeTimeout)
			if err := conn.Write(writeCtx, websocket.MessageText, msg.Body); err != nil {
				writeCancel()
				log.Error("Failed to write to websocket", "error", err)
				conn.Close(websocket.StatusInternalError, "Write failed")
				return
			}
			writeCancel()
			// Acknowledge message only after successful write to WebSocket
			if err := msg.Ack(false); err != nil {
				log.Error("Failed to ack message", "error", err)
			}
			log.Debug("Message sent to client", "size", len(msg.Body))
		}
	}
}
