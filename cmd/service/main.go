package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"hooklet/internal/api"
	"hooklet/internal/queue"
	"hooklet/internal/store"

	"github.com/charmbracelet/log"
	"github.com/coder/websocket"
)

// server holds the service state.
type server struct {
	mq        *queue.Client
	db        *store.Store
	startedAt time.Time

	// Track active topics for listing
	mu     sync.RWMutex
	topics map[string]struct{}
}

// Context key for admin bypass
// This key is private to the package, ensuring only this package can create or check it.
// This prevents external manipulation of the context to bypass authentication.
type contextKey string

const ctxKeyAdminBypass contextKey = "admin_bypass"

func main() {
	port := getEnv("PORT", api.DefaultPort)
	rabbitURL := getEnv("RABBITMQ_URL", api.DefaultRabbitURL)
	dbPath := getEnv("HOOKLET_DB_PATH", "hooklet.db")

	// Configure queue settings
	msgTTL, _ := strconv.Atoi(getEnv("HOOKLET_MESSAGE_TTL", strconv.Itoa(api.DefaultMessageTTL)))
	queueExpiry, _ := strconv.Atoi(getEnv("HOOKLET_QUEUE_EXPIRY", strconv.Itoa(api.DefaultQueueExpiry)))

	mqConfig := queue.Config{
		MessageTTL:  msgTTL,
		QueueExpiry: queueExpiry,
	}

	// Connect to RabbitMQ
	mqClient, err := queue.NewClient(rabbitURL, mqConfig)
	if err != nil {
		log.Fatal("Failed to connect to RabbitMQ", "error", err)
	}
	log.Info("Connected to RabbitMQ")

	// Connect to SQLite
	db, err := store.New(dbPath)
	if err != nil {
		log.Fatal("Failed to connect to SQLite", "error", err)
	}
	log.Info("Connected to SQLite", "path", dbPath)

	srv := &server{
		mq:        mqClient,
		db:        db,
		startedAt: time.Now(),
		topics:    make(map[string]struct{}),
	}

	// Setup HTTP routes
	mux := http.NewServeMux()

	// Service management endpoints (CLI uses these)
	mux.HandleFunc(api.RouteStatus, srv.handleStatus)
	mux.HandleFunc(api.RouteTopics, srv.handleTopics)

	// Admin endpoints
	mux.HandleFunc("/admin/webhooks", srv.handleAdminWebhooks)
	mux.HandleFunc("/admin/webhooks/", srv.handleAdminWebhookDelete) // for delete
	mux.HandleFunc("/admin/consumers", srv.handleAdminConsumers)
	mux.HandleFunc("/admin/consumers/", srv.handleAdminConsumerByID) // for delete/update

	// Webhook ingestion (external services POST here)
	mux.HandleFunc(api.RoutePublish, srv.handleWebhook)

	// WebSocket streaming (CLI subscribes here)
	mux.HandleFunc(api.RouteSubscribe, srv.handleWS)

	// Start server
	addr := ":" + port
	log.Info("Starting hooklet service")

	// Create listener for public TCP traffic
	tcpListener, err := net.Listen("tcp", addr)
	if err != nil {
		log.Fatal("TCP listener failed", "error", err)
	}
	log.Info("Listening on TCP", "addr", addr)

	// Create listener for Admin/Local Unix Socket
	// On Windows, if 1809+ this works as Unix Sockets.
	// If older, it falls back to net.Listen behavior or might fail, but Go >= 1.23 handles it well.
	socketPath := getEnv("HOOKLET_SOCKET", api.DefaultSocketPath)

	// Clean up old socket file
	if _, err := os.Stat(socketPath); err == nil {
		if err := os.Remove(socketPath); err != nil {
			log.Warn("Failed to remove old socket file", "path", socketPath, "error", err)
		}
	}

	unixListener, err := net.Listen("unix", socketPath)
	if err != nil {
		log.Warn("Unix socket listener failed (admin socket disabled)", "error", err)
	} else {
		log.Info("Listening on Unix Socket", "path", socketPath)
	}

	// Create request multiplexers
	// publicMux: Webhooks, Status, Topics, WebSocket (No Admin)
	publicMux := http.NewServeMux()
	publicMux.HandleFunc(api.RouteStatus, srv.handleStatus)
	publicMux.HandleFunc(api.RouteTopics, srv.handleTopics)
	publicMux.HandleFunc(api.RoutePublish, srv.handleWebhook)
	publicMux.HandleFunc(api.RouteSubscribe, srv.handleWS)

	// adminMux: Public routes + Admin routes
	adminMux := http.NewServeMux()
	// Register everything from publicMux
	adminMux.HandleFunc(api.RouteStatus, srv.handleStatus)
	adminMux.HandleFunc(api.RouteTopics, srv.handleTopics)
	adminMux.HandleFunc(api.RoutePublish, srv.handleWebhook)
	adminMux.HandleFunc(api.RouteSubscribe, srv.handleWS)
	// Add Admin routes (no checkAdminAuth needed on socket, but kept for logic consistency if reused)
	// We can wrap admin handlers to SKIP auth checks if coming from socket if we wanted,
	// but srv.checkAdminAuth already checks for local connection.
	// However, for the socket, we trust it implicitly.
	// Let's attach the handlers directly.
	adminMux.HandleFunc("/admin/webhooks", srv.handleAdminWebhooks)
	adminMux.HandleFunc("/admin/webhooks/", srv.handleAdminWebhookDelete)
	adminMux.HandleFunc("/admin/consumers", srv.handleAdminConsumers)
	adminMux.HandleFunc("/admin/consumers/", srv.handleAdminConsumerByID)

	// Trusted handler for Unix socket (injects admin bypass context)
	trustedHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := context.WithValue(r.Context(), ctxKeyAdminBypass, true)
		adminMux.ServeHTTP(w, r.WithContext(ctx))
	})

	// Create HTTP servers
	tcpServer := &http.Server{Handler: middlewareSource("api", publicMux)}
	unixServer := &http.Server{Handler: middlewareSource("unix", trustedHandler)}

	// Listen for shutdown signals
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	// Start TCP server
	go func() {
		if err := tcpServer.Serve(tcpListener); err != http.ErrServerClosed {
			log.Error("TCP Server failed", "error", err)
		}
	}()

	// Start Unix socket server
	if unixListener != nil {
		go func() {
			if err := unixServer.Serve(unixListener); err != http.ErrServerClosed {
				log.Error("Unix Socket Server failed", "error", err)
			}
		}()
	}

	log.Info("Service ready")

	// Wait for shutdown signal
	sig := <-sigChan
	log.Info("Received shutdown signal", "signal", sig)

	// Graceful shutdown with timeout
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()

	// Shutdown HTTP servers
	log.Info("Shutting down TCP server...")
	if err := tcpServer.Shutdown(shutdownCtx); err != nil {
		log.Error("TCP server shutdown error", "error", err)
	}

	if unixListener != nil {
		log.Info("Shutting down Unix socket server...")
		if err := unixServer.Shutdown(shutdownCtx); err != nil {
			log.Error("Unix server shutdown error", "error", err)
		}
		os.Remove(socketPath)
	}

	// Close RabbitMQ
	log.Info("Closing RabbitMQ connection...")
	mqClient.Close()

	// Close SQLite
	log.Info("Closing database...")
	db.Close()

	log.Info("Shutdown complete")
}

// handleStatus returns service health information.
// GET /api/status
func (s *server) handleStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// TODO: Add actual RabbitMQ health check (ping)
	status := api.StatusResponse{
		Status:    "ok",
		Uptime:    time.Since(s.startedAt).Round(time.Second).String(),
		StartedAt: s.startedAt,
		RabbitMQ:  "connected",
	}

	writeJSON(w, status)
}

// handleTopics returns the list of active topics.
// GET /api/topics
func (s *server) handleTopics(w http.ResponseWriter, r *http.Request) {
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

// checkAdminAuth verifies if the request is authorized to perform admin actions.
// It supports two authentication mechanisms:
//  1. "Admin Bypass" via Context: If the request comes from the Unix socket listener,
//     it has a special context value (ctxKeyAdminBypass) injected by the trustedHandler.
//     This allows password-less local administration via the CLI.
//  2. Token Authentication: For remote requests (TCP/API), a valid X-Hooklet-Admin-Token header
//     matching the HOOKLET_ADMIN_TOKEN env var is required.
func (s *server) checkAdminAuth(w http.ResponseWriter, r *http.Request) bool {
	// 1. Check if request comes from trusted Unix socket (Admin Bypass)
	if bypass, ok := r.Context().Value(ctxKeyAdminBypass).(bool); ok && bypass {
		return true
	}

	// 2. Standard Admin Auth
	// TODO: Implement real admin auth
	// For now we check a simple env var or header
	token := r.Header.Get("X-Hooklet-Admin-Token")
	expected := os.Getenv("HOOKLET_ADMIN_TOKEN")

	// If no auth is configured, allow everyone
	if expected == "" {
		return true
	}

	// Otherwise (remote OR local with token provided), enforce verification
	if token != expected {
		writeError(w, "Unauthorized", http.StatusUnauthorized)
		return false
	}
	return true
}

// POST /admin/webhooks (create)
// GET /admin/webhooks (list)
func (s *server) handleAdminWebhooks(w http.ResponseWriter, r *http.Request) {
	if !s.checkAdminAuth(w, r) {
		return
	}

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
			Name string `json:"name"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, "Invalid request body", http.StatusBadRequest)
			return
		}
		if req.Name == "" {
			writeError(w, "Name is required", http.StatusBadRequest)
			return
		}

		wh, err := s.db.CreateWebhook(req.Name)
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
		writeJSON(w, wh)

	default:
		writeError(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

// DELETE /admin/webhooks/{id}
func (s *server) handleAdminWebhookDelete(w http.ResponseWriter, r *http.Request) {
	if !s.checkAdminAuth(w, r) {
		return
	}

	if r.Method != http.MethodDelete {
		writeError(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	idStr := strings.TrimPrefix(r.URL.Path, "/admin/webhooks/")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		writeError(w, "Invalid ID", http.StatusBadRequest)
		return
	}

	if err := s.db.DeleteWebhook(id); err != nil {
		log.Error("Failed to delete webhook", "error", err)
		writeError(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// POST /admin/consumers (create)
// GET /admin/consumers (list)
func (s *server) handleAdminConsumers(w http.ResponseWriter, r *http.Request) {
	if !s.checkAdminAuth(w, r) {
		return
	}

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
			Subscriptions string `json:"subscriptions"` // comma separated or "*"
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

		consumer, err := s.db.CreateConsumer(req.Name, token, req.Subscriptions)
		if err != nil {
			log.Error("Failed to create consumer", "error", err)
			if strings.Contains(err.Error(), "UNIQUE constraint failed") {
				writeError(w, "Consumer name already exists", http.StatusConflict)
				return
			}
			writeError(w, "Internal Server Error", http.StatusInternalServerError)
			return
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

// handleAdminConsumerByID handles DELETE and PATCH for /admin/consumers/{id}
func (s *server) handleAdminConsumerByID(w http.ResponseWriter, r *http.Request) {
	if !s.checkAdminAuth(w, r) {
		return
	}

	idStr := strings.TrimPrefix(r.URL.Path, "/admin/consumers/")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		writeError(w, "Invalid ID", http.StatusBadRequest)
		return
	}

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
			if err := s.db.UpdateConsumerSubscriptions(id, *req.Subscriptions); err != nil {
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

// handleWebhook receives POST requests and publishes to RabbitMQ.
// POST /api/webhook/{topic}
func (s *server) handleWebhook(w http.ResponseWriter, r *http.Request) {
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

// handleWS upgrades to WebSocket and streams messages from RabbitMQ.
// GET /ws/?topics=t1,t2
// Authentication: Client must send {"type":"auth","token":"..."} as first message.
// This prevents token leakage via URL query params, logs, or referrer headers.
func (s *server) handleWS(w http.ResponseWriter, r *http.Request) {
	// Parse topics from URL query params
	var topics []string
	if t := r.URL.Query().Get(api.QueryParamTopics); t != "" {
		topics = strings.Split(t, ",")
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

	// Accept WebSocket connection FIRST, then authenticate via message
	// This prevents token from appearing in URL/logs
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		InsecureSkipVerify: true, // Allow all origins for POC
	})
	if err != nil {
		log.Error("Failed to accept websocket", "error", err)
		return
	}

	// Set a deadline for authentication
	authCtx, authCancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer authCancel()

	var consumer *store.Consumer

	// Check if this is a trusted admin connection (CLI via Unix socket)
	if bypass, ok := r.Context().Value(ctxKeyAdminBypass).(bool); ok && bypass {
		// Create a synthetic admin consumer for tracking
		consumer = &store.Consumer{
			ID:            0,
			Name:          "admin-cli",
			Subscriptions: "*",
		}
		log.Debug("WebSocket admin bypass active")
	} else {
		// Standard Auth: Wait for auth message from client
		// Client must send: {"type":"auth","token":"..."}
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

		// Validate Token against DB
		consumer, err = s.db.GetConsumerByToken(authReq.Token)
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
	// Consumer must have permission to subscribe to the requested topics
	// consumer.Subscriptions can be "*" or "topic1,topic2"
	allowed := false
	if consumer.Subscriptions == "*" {
		allowed = true
	} else {
		// Check each requested topic against consumer subscriptions
		subs := strings.Split(consumer.Subscriptions, ",")
		subMap := make(map[string]bool)
		for _, sub := range subs {
			subMap[strings.TrimSpace(sub)] = true
		}

		// All requested topics must be allowed
		allAllowed := true
		for _, t := range topics {
			if !subMap[t] {
				allAllowed = false
				break
			}
		}
		allowed = allAllowed
	}

	if !allowed {
		log.Warn("Consumer tried to subscribe to unauthorized topics", "consumer", consumer.Name, "topics", topics)
		conn.Close(websocket.StatusPolicyViolation, "Unauthorized topics")
		return
	}

	// Send auth success acknowledgement
	ack := map[string]string{"type": "auth_ok", "consumer": consumer.Name}
	ackBytes, _ := json.Marshal(ack)
	if err := conn.Write(r.Context(), websocket.MessageText, ackBytes); err != nil {
		log.Error("Failed to send auth ack", "error", err)
		conn.Close(websocket.StatusInternalError, "Failed to send ack")
		return
	}

	// Consumer ID is now the Consumer ID / Name to ensure tracking
	consumerID := fmt.Sprintf("%s-%d", consumer.Name, consumer.ID)

	log.Info("WebSocket client authenticated", "consumer_id", consumerID, "topics", topics, "remote", r.RemoteAddr)

	// Track topics
	s.mu.Lock()
	for _, t := range topics {
		s.topics[t] = struct{}{}
	}
	s.mu.Unlock()

	// Subscribe to queue with specific topics
	msgs, err := s.mq.Subscribe(consumerID, topics)
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
			if err := conn.Write(ctx, websocket.MessageText, msg.Body); err != nil {
				log.Error("Failed to write to websocket", "error", err)
				return
			}
			// Acknowledge message only after successful write to WebSocket
			if err := msg.Ack(false); err != nil {
				log.Error("Failed to ack message", "error", err)
			}
			log.Debug("Message sent to client", "size", len(msg.Body))
		}
	}
}

// middlewareSource adds logging context for the request source (tcp vs unix).
func middlewareSource(source string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.Info("Request received", "source", source, "method", r.Method, "path", r.URL.Path)
		next.ServeHTTP(w, r)
	})
}

// writeJSON sends a JSON response.
func writeJSON(w http.ResponseWriter, data any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(data); err != nil {
		log.Error("Failed to encode JSON response", "error", err)
	}
}

// writeError sends a JSON error response.
func writeError(w http.ResponseWriter, message string, status int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(api.ErrorResponse{Error: message})
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// generateSecureToken returns a cryptographically secure random token.
// Returns 32 random bytes encoded as 64 hex characters.
func generateSecureToken() (string, error) {
	bytes := make([]byte, 32)
	if _, err := rand.Read(bytes); err != nil {
		return "", fmt.Errorf("failed to generate token: %w", err)
	}
	return hex.EncodeToString(bytes), nil
}
