package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"hooklet/internal/config"
	"hooklet/internal/queue"
	"hooklet/internal/server"
	"hooklet/internal/store"

	"github.com/charmbracelet/log"
)

func main() {
	// 1. Load Configuration
	cfg := config.Load()

	// 2. Connect to RabbitMQ
	// Build URL if not provided directly
	rabbitURL := cfg.RabbitURL
	if rabbitURL == "" {
		rabbitURL = fmt.Sprintf("amqp://%s:%s@%s:%s/",
			cfg.RabbitUser, cfg.RabbitPass, cfg.RabbitHost, cfg.RabbitPort)
	}

	mqConfig := queue.Config{
		MessageTTL:  cfg.MessageTTL,
		QueueExpiry: cfg.QueueExpiry,
	}

	mqClient, err := queue.NewClient(rabbitURL, mqConfig)
	if err != nil {
		log.Fatal("Failed to connect to RabbitMQ", "error", err)
	}
	defer func() {
		log.Info("Closing RabbitMQ connection...")
		mqClient.Close()
	}()
	log.Info("Connected to RabbitMQ")

	// 3. Connect to SQLite
	db, err := store.New(cfg.DBPath)
	if err != nil {
		log.Fatal("Failed to connect to SQLite", "error", err)
	}
	defer func() {
		log.Info("Closing database...")
		db.Close()
	}()
	log.Info("Connected to SQLite", "path", cfg.DBPath)

	// 4. Initialize Server
	srv := server.New(cfg, db, mqClient)

	// 5. Start Server
	if err := srv.Start(); err != nil {
		log.Fatal("Failed to start server", "error", err)
	}

	log.Info("Service ready")

	// 6. Wait for shutdown signal
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	sig := <-sigChan
	log.Info("Received shutdown signal", "signal", sig)

	// 7. Graceful Shutdown
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Error("Server shutdown error", "error", err)
	}

	log.Info("Shutdown complete")
}
