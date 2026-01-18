package main

import (
	"context"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"hooklet/internal/queue"

	"github.com/charmbracelet/log"
	"github.com/coder/websocket"
)

const (
	defaultPort       = "8080"
	defaultRabbitURL  = "amqp://guest:guest@localhost:5672/"
	webhookPathPrefix = "/api/webhook/"
	wsPathPrefix      = "/ws/"
)

func main() {
	port := getEnv("PORT", defaultPort)
	rabbitURL := getEnv("RABBITMQ_URL", defaultRabbitURL)

	// Connect to RabbitMQ
	mqClient, err := queue.NewClient(rabbitURL)
	if err != nil {
		log.Fatal("Failed to connect to RabbitMQ", "error", err)
	}
	defer mqClient.Close()
	log.Info("Connected to RabbitMQ")

	// Setup HTTP routes
	mux := http.NewServeMux()
	mux.HandleFunc(webhookPathPrefix, handleWebhook(mqClient))
	mux.HandleFunc(wsPathPrefix, handleWS(mqClient))

	// Start server
	addr := ":" + port
	log.Info("Starting server", "addr", addr)
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatal("Server failed", "error", err)
	}
}

// handleWebhook receives POST requests and publishes to RabbitMQ.
func handleWebhook(mq *queue.Client) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		// Extract topic from path: /api/webhook/{topic}
		topic := strings.TrimPrefix(r.URL.Path, webhookPathPrefix)
		if topic == "" {
			http.Error(w, "Topic required", http.StatusBadRequest)
			return
		}

		// Read body
		body, err := io.ReadAll(r.Body)
		if err != nil {
			log.Error("Failed to read body", "error", err)
			http.Error(w, "Failed to read body", http.StatusInternalServerError)
			return
		}
		defer r.Body.Close()

		// Publish to queue
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()

		if err := mq.Publish(ctx, topic, body); err != nil {
			log.Error("Failed to publish", "topic", topic, "error", err)
			http.Error(w, "Failed to publish", http.StatusInternalServerError)
			return
		}

		log.Info("Webhook received", "topic", topic, "size", len(body))
		w.WriteHeader(http.StatusAccepted)
	}
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// handleWS upgrades to WebSocket and streams messages from RabbitMQ.
func handleWS(mq *queue.Client) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Extract topic from path: /ws/{topic}
		topic := strings.TrimPrefix(r.URL.Path, wsPathPrefix)
		if topic == "" {
			http.Error(w, "Topic required", http.StatusBadRequest)
			return
		}

		// Accept WebSocket connection
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
			InsecureSkipVerify: true, // Allow all origins for POC
		})
		if err != nil {
			log.Error("Failed to accept websocket", "error", err)
			return
		}
		defer conn.Close(websocket.StatusNormalClosure, "")

		log.Info("WebSocket client connected", "topic", topic, "remote", r.RemoteAddr)

		// Subscribe to queue
		msgs, err := mq.Consume(topic)
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
}
