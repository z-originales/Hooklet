// Service is the background HTTP server that receives webhooks and manages RabbitMQ.
package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"hooklet/internal/api"
	"hooklet/internal/queue"

	"github.com/charmbracelet/log"
	"github.com/coder/websocket"
)

// server holds the service state.
type server struct {
	mq        *queue.Client
	startedAt time.Time

	// Track active topics for listing
	mu     sync.RWMutex
	topics map[string]struct{}
}

func main() {
	port := getEnv("PORT", api.DefaultPort)
	rabbitURL := getEnv("RABBITMQ_URL", api.DefaultRabbitURL)

	// Connect to RabbitMQ
	mqClient, err := queue.NewClient(rabbitURL)
	if err != nil {
		log.Fatal("Failed to connect to RabbitMQ", "error", err)
	}
	defer mqClient.Close()
	log.Info("Connected to RabbitMQ")

	srv := &server{
		mq:        mqClient,
		startedAt: time.Now(),
		topics:    make(map[string]struct{}),
	}

	// Setup HTTP routes
	mux := http.NewServeMux()

	// Service management endpoints (CLI uses these)
	mux.HandleFunc(api.RouteStatus, srv.handleStatus)
	mux.HandleFunc(api.RouteTopics, srv.handleTopics)

	// Webhook ingestion (external services POST here)
	mux.HandleFunc(api.RoutePublish, srv.handleWebhook)

	// WebSocket streaming (CLI subscribes here)
	mux.HandleFunc(api.RouteSubscribe, srv.handleWS)

	// Start server
	addr := ":" + port
	log.Info("Starting hooklet service", "addr", addr)

	// TODO: Add graceful shutdown with signal handling
	// TODO: Add TLS support for production (or use reverse proxy)
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatal("Server failed", "error", err)
	}
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

// handleWebhook receives POST requests and publishes to RabbitMQ.
// POST /api/webhook/{topic}
func (s *server) handleWebhook(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// TODO: Add authentication check here
	// token := r.Header.Get(api.HeaderAuthToken)
	// if expectedToken != "" && token != expectedToken {
	//     writeError(w, "Unauthorized", http.StatusUnauthorized)
	//     return
	// }

	// Extract topic from path: /api/webhook/{topic}
	topic := strings.TrimPrefix(r.URL.Path, api.RoutePublish)
	if topic == "" {
		writeError(w, "Topic required", http.StatusBadRequest)
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
// GET /ws/{topic} (WebSocket upgrade)
func (s *server) handleWS(w http.ResponseWriter, r *http.Request) {
	// Extract topic from path: /ws/{topic}
	topic := strings.TrimPrefix(r.URL.Path, api.RouteSubscribe)
	if topic == "" {
		http.Error(w, "Topic required", http.StatusBadRequest)
		return
	}

	// TODO: Add authentication check here for WebSocket connections
	// Could use query param or initial message for token

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

	log.Info("WebSocket client connected", "topic", topic, "remote", r.RemoteAddr)

	// Track topic
	s.mu.Lock()
	s.topics[topic] = struct{}{}
	s.mu.Unlock()

	// Subscribe to queue
	msgs, err := s.mq.Consume(topic)
	if err != nil {
		log.Error("Failed to consume", "topic", topic, "error", err)
		conn.Close(websocket.StatusInternalError, "Failed to subscribe")
		return
	}

	// Stream messages to WebSocket
	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			log.Info("WebSocket client disconnected", "topic", topic)
			return
		case msg, ok := <-msgs:
			if !ok {
				log.Info("Queue channel closed", "topic", topic)
				return
			}
			if err := conn.Write(ctx, websocket.MessageText, msg.Body); err != nil {
				log.Error("Failed to write to websocket", "error", err)
				return
			}
			log.Debug("Message sent to client", "topic", topic, "size", len(msg.Body))
		}
	}
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
