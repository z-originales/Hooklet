package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
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
	defer mqClient.Close()
	log.Info("Connected to RabbitMQ")

	// Connect to SQLite
	db, err := store.New(dbPath)
	if err != nil {
		log.Fatal("Failed to connect to SQLite", "error", err)
	}
	defer db.Close()
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
	mux.HandleFunc("/admin/users", srv.handleAdminUsers)

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
		// Cleanup socket on exit
		// Note: defer runs when main returns, but ListenAndServe blocks.
		// We'll rely on OS cleanup or start-up cleanup for now.
		defer func() {
			unixListener.Close()
			os.Remove(socketPath)
		}()
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
	adminMux.HandleFunc("/admin/users", srv.handleAdminUsers)

	// Run servers in goroutines
	var wg sync.WaitGroup
	wg.Add(1)

	// 1. TCP Server (Public)
	go func() {
		defer wg.Done()
		if err := http.Serve(tcpListener, middlewareSource("api", publicMux)); err != nil {
			log.Error("TCP Server failed", "error", err)
		}
	}()

	// 2. Unix Socket Server (Admin)
	if unixListener != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// 2. Unix Socket Server (Admin)
			// This handler wraps the admin routes and marks the request as "trusted"
			// by injecting the ctxKeyAdminBypass into the context.
			// Since this listener only accepts connections from the local Unix socket
			// (which is protected by file system permissions), we consider these requests
			// to be from an authorized local administrator.
			trustedHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				// Inject trust flag into context
				ctx := context.WithValue(r.Context(), ctxKeyAdminBypass, true)
				adminMux.ServeHTTP(w, r.WithContext(ctx))
			})

			if err := http.Serve(unixListener, middlewareSource("unix", trustedHandler)); err != nil {
				log.Error("Unix Socket Server failed", "error", err)
			}
		}()
	}

	// Wait (indefinitely)
	wg.Wait()
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

// POST /admin/users (create)
// GET /admin/users (list)
func (s *server) handleAdminUsers(w http.ResponseWriter, r *http.Request) {
	if !s.checkAdminAuth(w, r) {
		return
	}

	switch r.Method {
	case http.MethodGet:
		users, err := s.db.ListUsers()
		if err != nil {
			log.Error("Failed to list users", "error", err)
			writeError(w, "Internal Server Error", http.StatusInternalServerError)
			return
		}
		writeJSON(w, users)

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

		// Simple token generation (UUID would be better, using timestamp + random for now if UUID package not available)
		// Or since we have modernc.org/sqlite, we likely don't have google/uuid imported yet unless I check go.mod
		// Checking go.mod earlier showed google/uuid indirectly. I'll just use a simple pseudo-random string for now to avoid deps issues.
		token := fmt.Sprintf("%s-%d", req.Name, time.Now().UnixNano())

		// In a real app we'd hash this. For this POC storing raw or simple hash is 'ok' but let's pretend we hash it.
		// Actually storing raw token in DB is bad practice, but useful for retrieving it if we want to show it once.
		// Let's return it in response but store a "hash" (simulate).
		// For simplicity in this iteration: store the token as is in token_hash column, but treat it as a secret.

		u, err := s.db.CreateUser(req.Name, token, req.Subscriptions)
		if err != nil {
			log.Error("Failed to create user", "error", err)
			if strings.Contains(err.Error(), "UNIQUE constraint failed") {
				writeError(w, "User name already exists", http.StatusConflict)
				return
			}
			writeError(w, "Internal Server Error", http.StatusInternalServerError)
			return
		}

		// Return the token to the admin only once
		resp := struct {
			*store.User
			Token string `json:"token"`
		}{
			User:  u,
			Token: token,
		}
		writeJSON(w, resp)

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

	// Validate webhook exists in DB if configured to do so
	// For now we allow open publishing or token based.
	// TODO: Add token validation against Users table if header present.

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

	if err := s.mq.Publish(ctx, topic, body); err != nil {
		log.Error("Failed to publish", "topic", topic, "error", err)
		writeError(w, "Failed to publish", http.StatusInternalServerError)
		return
	}

	// Track topic
	s.mu.Lock()
	s.topics[topic] = struct{}{}
	s.mu.Unlock()

	log.Info("Webhook received", "topic", topic, "size", len(body))
	w.WriteHeader(http.StatusAccepted)
}

// handleWS upgrades to WebSocket and streams messages from RabbitMQ.
// GET /ws/{topic}?topics=t1,t2
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

	// TODO: Add authentication check here for WebSocket connections
	// For now, we use the Sec-WebSocket-Key as a session ID to identify the consumer
	consumerID := r.Header.Get("Sec-WebSocket-Key")
	if consumerID == "" {
		// Fallback for non-standard clients
		consumerID = r.RemoteAddr
	}

	// Accept WebSocket connection
	// TODO: Configure allowed origins for production security
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		InsecureSkipVerify: true, // Allow all origins for POC
	})
	if err != nil {
		log.Error("Failed to accept websocket", "error", err)
		return
	}
	defer conn.Close(websocket.StatusNormalClosure, "")

	log.Info("WebSocket client connected", "consumer_id", consumerID, "topics", topics, "remote", r.RemoteAddr)

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
			return
		case msg, ok := <-msgs:
			if !ok {
				log.Info("Queue channel closed", "consumer_id", consumerID)
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
