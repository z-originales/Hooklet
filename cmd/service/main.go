package main

import (
	"context"
	"fmt"
	"net/url"
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
	// Load configuration from environment
	cfg := config.Load()

	// Configure log level
	if level, err := log.ParseLevel(cfg.LogLevel); err == nil {
		log.SetLevel(level)
	} else {
		log.Warn("Invalid log level, using default", "level", cfg.LogLevel)
	}

	// Build RabbitMQ URL from components (or use full URL if provided)
	rabbitURL := cfg.RabbitURL
	if rabbitURL == "" {
		// Percent-encode credentials to handle special characters
		rabbitURL = fmt.Sprintf("amqp://%s:%s@%s:%s/",
			url.QueryEscape(cfg.RabbitUser),
			url.QueryEscape(cfg.RabbitPass),
			cfg.RabbitHost, cfg.RabbitPort)
	}

	// Connect to RabbitMQ
	mqConfig := queue.Config{
		MessageTTL:  cfg.MessageTTL,
		QueueExpiry: cfg.QueueExpiry,
	}
	mqClient, err := queue.NewClient(rabbitURL, mqConfig)
	if err != nil {
		log.Fatal("Failed to connect to RabbitMQ", "error", err)
	}
	log.Info("Connected to RabbitMQ")

	// Connect to SQLite
	db, err := store.New(cfg.DBPath)
	if err != nil {
		mqClient.Close()
		log.Fatal("Failed to connect to SQLite", "error", err)
	}
	log.Info("Connected to SQLite", "path", cfg.DBPath)

	// Create and start the server
	srv := server.New(cfg, db, mqClient)
	if err := srv.Start(); err != nil {
		db.Close()
		mqClient.Close()
		log.Fatal("Failed to start server", "error", err)
	}
	log.Info("Service ready", "port", cfg.Port)

	// Wait for shutdown signal
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	sig := <-sigChan
	log.Info("Received shutdown signal", "signal", sig)

	// Graceful shutdown with timeout
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()

	// Shutdown HTTP servers
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Error("Server shutdown error", "error", err)
	}

	// Close RabbitMQ
	log.Info("Closing RabbitMQ connection...")
	mqClient.Close()

	// Close SQLite
	log.Info("Closing database...")
	db.Close()

	log.Info("Shutdown complete")
}
