package main

import (
	"context"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"hooklet/internal/queue"

	"github.com/charmbracelet/log"
	"github.com/coder/websocket"
)

const (
	defaultPort        = "8080"
	defaultRabbitURL   = "amqp://guest:guest@localhost:5672/"
	webhookPathPrefix  = "/api/webhook/"
	wsPathPrefix       = "/ws/"
	defaultMessageTTL  = 300000  // 5 minutes
	defaultQueueExpiry = 3600000 // 1 hour
)

func main() {
	port := getEnv("PORT", defaultPort)
	rabbitURL := getEnv("RABBITMQ_URL", defaultRabbitURL)

	// Configure queue settings
	msgTTL, _ := strconv.Atoi(getEnv("HOOKLET_MESSAGE_TTL", strconv.Itoa(defaultMessageTTL)))
	queueExpiry, _ := strconv.Atoi(getEnv("HOOKLET_QUEUE_EXPIRY", strconv.Itoa(defaultQueueExpiry)))

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
		// Parse topics from URL query params
		var topics []string
		if t := r.URL.Query().Get("topics"); t != "" {
			topics = strings.Split(t, ",")
		}

		// Also support topic from path for backward compatibility / convenience
		// /ws/{topic}
		pathTopic := strings.TrimPrefix(r.URL.Path, wsPathPrefix)
		if pathTopic != "" && pathTopic != "/" {
			topics = append(topics, pathTopic)
		}

		if len(topics) == 0 {
			http.Error(w, "No topics specified", http.StatusBadRequest)
			return
		}

		// For now, we use the Sec-WebSocket-Key as a session ID to identify the consumer
		consumerID := r.Header.Get("Sec-WebSocket-Key")
		if consumerID == "" {
			consumerID = r.RemoteAddr
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

		log.Info("WebSocket client connected", "consumer_id", consumerID, "topics", topics, "remote", r.RemoteAddr)

		// Subscribe to queue with specific topics
		msgs, err := mq.Subscribe(consumerID, topics)
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
}
