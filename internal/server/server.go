package server

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"sync"
	"time"

	"hooklet/internal/config"
	"hooklet/internal/httpcontract"
	"hooklet/internal/queue"
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
		log.Info("Listening on Unix Socket", "path", s.cfg.SocketPath)
	}

	// Create HTTP servers
	s.tcpServer = &http.Server{Handler: middlewareSource("api", mux)}

	// Unix socket server uses a special handler that injects admin bypass context
	trustedHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := context.WithValue(r.Context(), ctxKeyAdminBypass, true)
		mux.ServeHTTP(w, r.WithContext(ctx))
	})
	s.unixServer = &http.Server{Handler: middlewareSource("unix", trustedHandler)}

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
	log.Info("Shutting down TCP server...")
	if err := s.tcpServer.Shutdown(ctx); err != nil {
		log.Error("TCP server shutdown error", "error", err)
	}

	if s.unixServer != nil {
		log.Info("Shutting down Unix socket server...")
		if err := s.unixServer.Shutdown(ctx); err != nil {
			log.Error("Unix server shutdown error", "error", err)
		}
		os.Remove(s.cfg.SocketPath)
	}
	return nil
}

// Helper: writeJSON sends a JSON response.
func writeJSON(w http.ResponseWriter, data any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(data); err != nil {
		log.Error("Failed to encode JSON response", "error", err)
	}
}

// Helper: writeError sends a JSON error response.
func writeError(w http.ResponseWriter, message string, status int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(httpcontract.ErrorResponse{Error: message})
}
