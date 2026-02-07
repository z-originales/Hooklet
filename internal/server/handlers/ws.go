package handlers

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"hooklet/internal/api"
	"hooklet/internal/config"
	"hooklet/internal/queue"
	"hooklet/internal/server/auth"
	"hooklet/internal/store"

	"github.com/charmbracelet/log"
	"github.com/coder/websocket"
)

// generateConnectionID creates a random 8-character hex string for unique queue naming.
// This ensures each WebSocket connection gets its own RabbitMQ queue (fan-out behavior).
func generateConnectionID() string {
	b := make([]byte, 4) // 4 bytes = 8 hex characters
	if _, err := rand.Read(b); err != nil {
		// Fallback to timestamp if crypto/rand fails (extremely unlikely)
		return fmt.Sprintf("%08x", time.Now().UnixNano()&0xFFFFFFFF)
	}
	return hex.EncodeToString(b)
}

// WSHandler upgrades to WebSocket and streams messages from RabbitMQ.
type WSHandler struct {
	mq    *queue.Client
	db    *store.Store
	track func(string)
	cfg   config.Config
}

// NewWSHandler creates a handler for WebSocket streaming.
func NewWSHandler(mq *queue.Client, db *store.Store, trackTopic func(string), cfg config.Config) *WSHandler {
	return &WSHandler{mq: mq, db: db, track: trackTopic, cfg: cfg}
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
	writeTimeout := time.Duration(h.cfg.WSWriteTimeoutSeconds) * time.Second
	authTimeout := time.Duration(h.cfg.WSAuthTimeoutSeconds) * time.Second

	// Parse topics from URL query params
	requested := make([]string, 0)
	if queryTopics := r.URL.Query().Get(api.QueryParamTopics); queryTopics != "" {
		for _, name := range strings.Split(queryTopics, ",") {
			name = strings.TrimSpace(name)
			if name != "" {
				requested = append(requested, name)
			}
		}
	}

	// Also support topic from path for backward compatibility / convenience
	// /ws/{topic}
	pathTopic := strings.TrimPrefix(r.URL.Path, api.RouteSubscribe)
	if pathTopic != "" && pathTopic != "/" {
		requested = append(requested, pathTopic)
	}

	if len(requested) == 0 {
		http.Error(w, "No topics specified", http.StatusBadRequest)
		return
	}

	// Deduplicate topics
	topicSet := make(map[string]struct{}, len(requested))
	var topics []string
	for _, topic := range requested {
		if _, exists := topicSet[topic]; exists {
			continue
		}
		topicSet[topic] = struct{}{}
		topics = append(topics, topic)
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
	acceptOpts := &websocket.AcceptOptions{InsecureSkipVerify: true}
	if len(h.cfg.WSAllowedOrigins) > 0 {
		acceptOpts.OriginPatterns = h.cfg.WSAllowedOrigins
		acceptOpts.InsecureSkipVerify = false
	}

	conn, err := websocket.Accept(w, r, acceptOpts)
	if err != nil {
		log.Error("Failed to accept websocket", "error", err)
		return
	}

	// 3. If not yet authenticated, wait for auth message (fallback for browser clients)
	if consumer == nil {
		authCtx, authCancel := context.WithTimeout(r.Context(), authTimeout)
		defer authCancel()

		_, authMsg, err := conn.Read(authCtx)
		if err != nil {
			log.Warn("WebSocket auth timeout or read error", "remote", r.RemoteAddr, "error", err)
			conn.Close(websocket.StatusPolicyViolation, "Authentication timeout")
			return
		}

		if h.cfg.WSAuthMaxBytes > 0 && int64(len(authMsg)) > h.cfg.WSAuthMaxBytes {
			log.Warn("WebSocket auth message too large", "remote", r.RemoteAddr, "size", len(authMsg))
			conn.Close(websocket.StatusPolicyViolation, "Auth message too large")
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
		subs, err := h.db.GetConsumerSubscriptions(consumer.ID)
		if err != nil {
			log.Error("Failed to load consumer subscriptions", "error", err)
			conn.Close(websocket.StatusInternalError, "Internal error")
			return
		}
		for _, topic := range topics {
			allowed := false
			for _, sub := range subs {
				if topic == sub.Name {
					allowed = true
					break
				}
				if !strings.Contains(topic, "*") && store.MatchTopic(sub.Name, topic) {
					allowed = true
					break
				}
			}
			if !allowed {
				log.Warn("Consumer tried to subscribe to unauthorized topic", "consumer", consumer.Name, "topic", topic)
				conn.Close(websocket.StatusPolicyViolation, "Unauthorized topic: "+topic)
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

	// Consumer ID includes a unique connection ID to ensure each WebSocket gets its own queue
	// This enables fan-out behavior: all connections receive all messages (no round-robin)
	connectionID := generateConnectionID()
	consumerID := fmt.Sprintf("%s-%d-%s", consumer.Name, consumer.ID, connectionID)

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
