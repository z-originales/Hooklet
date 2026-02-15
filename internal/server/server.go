package server

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"sync"
	"time"

	"hooklet/internal/config"
	"hooklet/internal/queue"
	"hooklet/internal/server/auth"
	"hooklet/internal/store"

	"github.com/charmbracelet/log"
)

// Server holds the service state and dependencies.
type Server struct {
	cfg       config.Config
	mq        *queue.Client
	db        *store.Store
	startedAt time.Time

	// Track active topics for listing
	mu     sync.RWMutex
	topics map[string]struct{}

	tcpServer  *http.Server
	unixServer *http.Server
}

// New creates a new Server instance.
func New(cfg config.Config, db *store.Store, mq *queue.Client) *Server {
	return &Server{
		cfg:       cfg,
		mq:        mq,
		db:        db,
		startedAt: time.Now(),
		topics:    make(map[string]struct{}),
	}
}

// Start initializes the HTTP listeners and starts the server.
func (s *Server) Start() error {
	// Setup router
	mux := s.newRouter()

	if s.cfg.AdminToken == "" && s.cfg.AdminDebug {
		log.Warn("Admin auth is disabled in debug mode", "note", "set HOOKLET_ADMIN_TOKEN or disable HOOKLET_ADMIN_DEBUG")
	}

	// Create listener for public TCP traffic
	addr := ":" + s.cfg.Port
	tcpListener, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	log.Info("Listening on TCP", "addr", addr)

	// Create listener for Admin/Local Unix Socket
	// Clean up old socket file first
	if _, err := os.Stat(s.cfg.SocketPath); err == nil {
		if err := os.Remove(s.cfg.SocketPath); err != nil {
			log.Warn("Failed to remove old socket file", "path", s.cfg.SocketPath, "error", err)
		}
	}

	unixListener, err := net.Listen("unix", s.cfg.SocketPath)
	if err != nil {
		log.Warn("Unix socket listener failed (admin socket disabled)", "error", err)
	} else {
		// Restrict socket permissions to owner only (0600)
		if err := os.Chmod(s.cfg.SocketPath, 0600); err != nil {
			log.Warn("Failed to set socket permissions", "error", err)
		}
		log.Info("Listening on Unix Socket", "path", s.cfg.SocketPath)
	}

	// Create HTTP servers
	readTimeout := time.Duration(s.cfg.HTTPReadTimeoutSeconds) * time.Second
	writeTimeout := time.Duration(s.cfg.HTTPWriteTimeoutSeconds) * time.Second
	idleTimeout := time.Duration(s.cfg.HTTPIdleTimeoutSeconds) * time.Second
	readHeaderTimeout := time.Duration(s.cfg.HTTPReadHeaderTimeoutSeconds) * time.Second

	s.tcpServer = &http.Server{
		Handler:           middlewareSource("api", mux),
		ReadTimeout:       readTimeout,
		WriteTimeout:      writeTimeout,
		IdleTimeout:       idleTimeout,
		ReadHeaderTimeout: readHeaderTimeout,
	}

	// Unix socket server uses a special handler that injects admin bypass context
	trustedHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := auth.WithAdminBypass(r.Context())
		mux.ServeHTTP(w, r.WithContext(ctx))
	})
	s.unixServer = &http.Server{
		Handler:           middlewareSource("unix", trustedHandler),
		ReadTimeout:       readTimeout,
		WriteTimeout:      writeTimeout,
		IdleTimeout:       idleTimeout,
		ReadHeaderTimeout: readHeaderTimeout,
	}

	// Start TCP server
	go func() {
		if err := s.tcpServer.Serve(tcpListener); err != http.ErrServerClosed {
			log.Error("TCP Server failed", "error", err)
		}
	}()

	// Start Unix socket server
	if unixListener != nil {
		go func() {
			if err := s.unixServer.Serve(unixListener); err != http.ErrServerClosed {
				log.Error("Unix Socket Server failed", "error", err)
			}
		}()
	}

	return nil
}

// Shutdown gracefully stops the HTTP servers.
func (s *Server) Shutdown(ctx context.Context) error {
	var errs []error

	log.Info("Shutting down TCP server...")
	if err := s.tcpServer.Shutdown(ctx); err != nil {
		log.Error("TCP server shutdown error", "error", err)
		errs = append(errs, err)
	}

	if s.unixServer != nil {
		log.Info("Shutting down Unix socket server...")
		if err := s.unixServer.Shutdown(ctx); err != nil {
			log.Error("Unix server shutdown error", "error", err)
			errs = append(errs, err)
		}
		os.Remove(s.cfg.SocketPath)
	}
	return errors.Join(errs...)
}

// trackTopic records active topics for listing.
func (s *Server) trackTopic(topic string) {
	s.mu.Lock()
	s.topics[topic] = struct{}{}
	s.mu.Unlock()
}

// listTopics returns a snapshot of active topics.
func (s *Server) listTopics() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()

	topics := make([]string, 0, len(s.topics))
	for t := range s.topics {
		topics = append(topics, t)
	}
	return topics
}

// rabbitConnected reports MQ connectivity.
func (s *Server) rabbitConnected() bool {
	return s.mq.IsConnected()
}
