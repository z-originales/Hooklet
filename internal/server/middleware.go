package server

import (
	"net/http"

	"github.com/charmbracelet/log"
)

// Context key for admin bypass
// This key is private to the package, ensuring only this package can create or check it.
type contextKey string

const ctxKeyAdminBypass contextKey = "admin_bypass"

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
		if bypass, ok := r.Context().Value(ctxKeyAdminBypass).(bool); ok && bypass {
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
			// writeError is defined in server.go, but accessible here as part of same package
			writeError(w, "Unauthorized", http.StatusUnauthorized)
			return
		}

		next.ServeHTTP(w, r)
	}
}

// producerAuthMiddleware verifies the token for webhook ingestion.
func (s *Server) producerAuthMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Logic to check producer token needs the webhook context (topic hash).
		// Since extracting the topic happens inside the handler in the current design,
		// we might need to keep the auth check inside the handler OR parse the path here.
		//
		// For now, to keep it simple and safe, we will leave producer auth inside the
		// handleWebhook function as it depends on looking up the specific webhook first.
		// Moving it here would require duplicating the lookup logic.

		next.ServeHTTP(w, r)
	}
}
