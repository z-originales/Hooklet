package server

import (
	"net/http"

	"hooklet/internal/server/auth"
	"hooklet/internal/server/httpresponse"

	"github.com/charmbracelet/log"
)

// middlewareSource adds logging context for the request source (tcp vs unix).
func middlewareSource(source string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.Info("Request received", "source", source, "method", r.Method, "path", r.URL.Path)
		next.ServeHTTP(w, r)
	})
}

// adminAuthMiddleware checks if the request is authorized for admin routes.
// It supports two authentication mechanisms:
//  1. "Admin Bypass" via Context (from Unix socket).
//  2. Token Authentication via Header/Env var.
func (s *Server) adminAuthMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// 1. Check if request comes from trusted Unix socket (Admin Bypass)
		if auth.IsAdminBypass(r.Context()) {
			next.ServeHTTP(w, r)
			return
		}

		// 2. Standard Admin Auth
		token := r.Header.Get("X-Hooklet-Admin-Token")
		// Use config instead of os.Getenv directly
		expected := s.cfg.AdminToken

		// If no auth is configured, allow everyone (dev mode / insecure)
		if expected == "" {
			next.ServeHTTP(w, r)
			return
		}

		// Otherwise (remote OR local with token provided), enforce verification
		if token != expected {
			httpresponse.WriteError(w, "Unauthorized", http.StatusUnauthorized)
			return
		}

		next.ServeHTTP(w, r)
	}
}
