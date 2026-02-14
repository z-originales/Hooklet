package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"hooklet/internal/api"
	"hooklet/internal/config"
	"hooklet/internal/queue"
	"hooklet/internal/store"

	"github.com/charmbracelet/log"
	"github.com/coder/websocket"
)

// activeConn tracks an active WebSocket connection for a consumer.
type activeConn struct {
	cancel context.CancelFunc
	conn   *websocket.Conn
}

// WSHandler upgrades to WebSocket and streams messages from RabbitMQ.
// Only one active WebSocket connection per consumer is allowed. If a consumer
// reconnects, the previous connection is kicked with a close message.
type WSHandler struct {
	mq    *queue.Client
	db    *store.Store
	track func(string)
	cfg   config.Config

	mu     sync.Mutex
	active map[int64]*activeConn // consumer ID -> active connection
}

// NewWSHandler creates a handler for WebSocket streaming.
func NewWSHandler(mq *queue.Client, db *store.Store, trackTopic func(string), cfg config.Config) *WSHandler {
	return &WSHandler{
		mq:     mq,
		db:     db,
		track:  trackTopic,
		cfg:    cfg,
		active: make(map[int64]*activeConn),
	}
}

// extractBearerToken extracts the token from "Authorization: Bearer <token>" header.
func extractBearerToken(r *http.Request) string {
	header := r.Header.Get(api.HeaderAuthorization)
	if strings.HasPrefix(header, api.BearerPrefix) {
		return strings.TrimPrefix(header, api.BearerPrefix)
	}
	return ""
}

// sendAuthFailed writes an auth_failed JSON message to the WebSocket before closing.
func sendAuthFailed(conn *websocket.Conn, writeTimeout time.Duration, reason string) {
	msg, _ := json.Marshal(map[string]string{"auth_status": "auth_failed", "reason": reason})
	ctx, cancel := context.WithTimeout(context.Background(), writeTimeout)
	_ = conn.Write(ctx, websocket.MessageText, msg)
	cancel()
}

// registerConn registers a consumer's connection, kicking any existing one.
// If an old connection exists, it sends a kicked notification and close frame
// before cancelling the context, ensuring the client receives a proper close.
func (h *WSHandler) registerConn(consumerID int64, conn *websocket.Conn) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())
	writeTimeout := time.Duration(h.cfg.WSWriteTimeoutSeconds) * time.Second

	h.mu.Lock()
	if old, exists := h.active[consumerID]; exists {
		// Notify old client before closing (best-effort, like sendAuthFailed)
		kicked, _ := json.Marshal(map[string]string{"type": "kicked", "reason": "replaced_by_new_connection"})
		notifyCtx, notifyCancel := context.WithTimeout(context.Background(), writeTimeout)
		_ = old.conn.Write(notifyCtx, websocket.MessageText, kicked)
		notifyCancel()

		// Close the old WebSocket with a proper close frame
		old.conn.Close(websocket.StatusNormalClosure, "Replaced by new connection")

		// Cancel the old Subscribe goroutine
		old.cancel()

		log.Info("Kicked previous connection", "consumer_id", consumerID)
	}
	h.active[consumerID] = &activeConn{cancel: cancel, conn: conn}
	h.mu.Unlock()

	return ctx, cancel
}

// unregisterConn removes the consumer from the active map.
// Only removes if the cancel matches (avoids race with a newer connection).
func (h *WSHandler) unregisterConn(consumerID int64, cancel context.CancelFunc) {
	h.mu.Lock()
	if current, exists := h.active[consumerID]; exists && fmt.Sprintf("%p", current.cancel) == fmt.Sprintf("%p", cancel) {
		delete(h.active, consumerID)
	}
	h.mu.Unlock()
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

	// 1. Header-based auth: validate BEFORE upgrading WebSocket
	if token := extractBearerToken(r); token != "" {
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

	// 2. If not yet authenticated, wait for auth message (fallback for browser clients)
	if consumer == nil {
		authCtx, authCancel := context.WithTimeout(r.Context(), authTimeout)
		defer authCancel()

		// Limit auth message size to prevent OOM DOS
		if h.cfg.WSAuthMaxBytes > 0 {
			conn.SetReadLimit(h.cfg.WSAuthMaxBytes)
		}

		_, authMsg, err := conn.Read(authCtx)
		if err != nil {
			log.Warn("WebSocket auth timeout or read error", "remote", r.RemoteAddr, "error", err)
			sendAuthFailed(conn, writeTimeout, "authentication timeout")
			conn.Close(websocket.StatusPolicyViolation, "Authentication timeout")
			return
		}

		var authReq struct {
			Type  string `json:"type"`
			Token string `json:"token"`
		}
		if err := json.Unmarshal(authMsg, &authReq); err != nil || authReq.Type != "auth" || authReq.Token == "" {
			log.Warn("WebSocket invalid auth message", "remote", r.RemoteAddr)
			sendAuthFailed(conn, writeTimeout, "invalid auth message")
			conn.Close(websocket.StatusPolicyViolation, "Invalid auth message")
			return
		}

		consumer, err = h.db.GetConsumerByToken(authReq.Token)
		if err != nil {
			log.Error("Failed to validate token", "error", err)
			sendAuthFailed(conn, writeTimeout, "internal error")
			conn.Close(websocket.StatusInternalError, "Internal error")
			return
		}
		if consumer == nil {
			log.Warn("WebSocket connection with invalid token", "remote", r.RemoteAddr)
			sendAuthFailed(conn, writeTimeout, "invalid token")
			conn.Close(websocket.StatusPolicyViolation, "Invalid token")
			return
		}
	}

	// Authorization Check (Subscriptions)
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
			sendAuthFailed(conn, writeTimeout, "unauthorized topic: "+topic)
			conn.Close(websocket.StatusPolicyViolation, "Unauthorized topic: "+topic)
			return
		}
	}

	// Send auth success acknowledgement
	ack := map[string]interface{}{
		"consumer_name":        consumer.Name,
		"auth_status":          "auth_ok",
		"queue_lifetime_ms":    h.cfg.QueueExpiry,
		"message_retention_ms": h.cfg.MessageTTL,
	}
	ackBytes, _ := json.Marshal(ack)
	ackCtx, ackCancel := context.WithTimeout(r.Context(), writeTimeout)
	if err := conn.Write(ackCtx, websocket.MessageText, ackBytes); err != nil {
		ackCancel()
		log.Error("Failed to send auth ack", "error", err)
		conn.Close(websocket.StatusInternalError, "Failed to send ack")
		return
	}
	ackCancel()

	// Register this connection, kicking any previous one for the same consumer
	connCtx, connCancel := h.registerConn(consumer.ID, conn)
	defer h.unregisterConn(consumer.ID, connCancel)
	defer connCancel()

	// Stable queue name per consumer (no random suffix)
	consumerTag := fmt.Sprintf("%s-%d", consumer.Name, consumer.ID)

	log.Info("WebSocket client connected", "consumer", consumer.Name, "consumer_id", consumer.ID, "topics", topics, "remote", r.RemoteAddr)

	// Track topics
	if h.track != nil {
		for _, t := range topics {
			h.track(t)
		}
	}

	// Subscribe to queue with specific topics
	msgs, err := h.mq.Subscribe(connCtx, consumerTag, topics)
	if err != nil {
		log.Error("Failed to subscribe", "consumer", consumer.Name, "error", err)
		conn.Close(websocket.StatusInternalError, "Failed to subscribe")
		return
	}

	// Discard reader: keeps conn.Read() active so coder/websocket can process
	// incoming close frames. Without this, client-initiated closes are never
	// acknowledged and the client gets 1006 ABNORMAL_CLOSURE.
	conn.SetReadLimit(4096) // No client messages expected after auth
	clientGone := make(chan struct{})
	go func() {
		defer close(clientGone)
		for {
			if _, _, err := conn.Read(connCtx); err != nil {
				return
			}
		}
	}()

	// Stream messages directly from RabbitMQ to WebSocket
	for {
		select {
		case <-connCtx.Done():
			// Connection was kicked by registerConn — WebSocket already closed there
			log.Info("Connection replaced by new login", "consumer", consumer.Name)
			return
		case <-clientGone:
			log.Info("WebSocket client disconnected", "consumer", consumer.Name)
			return
		case msg, ok := <-msgs:
			if !ok {
				log.Info("Queue channel closed", "consumer", consumer.Name)
				conn.Close(websocket.StatusNormalClosure, "Queue closed")
				return
			}
			writeCtx, writeCancel := context.WithTimeout(connCtx, writeTimeout)
			if err := conn.Write(writeCtx, websocket.MessageText, msg.Body); err != nil {
				writeCancel()
				log.Error("Failed to write to websocket", "consumer", consumer.Name, "error", err)
				return
			}
			writeCancel()
			// Acknowledge message only after successful write to WebSocket
			if err := msg.Ack(false); err != nil {
				log.Error("Failed to ack message", "error", err)
			}
			log.Debug("Message sent to client", "consumer", consumer.Name, "size", len(msg.Body))
		}
	}
}
