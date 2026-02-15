package httpresponse

import (
	"encoding/json"
	"net/http"

	"hooklet/internal/api"

	"github.com/charmbracelet/log"
)

// WriteJSON sends a JSON response with status 200.
func WriteJSON(w http.ResponseWriter, data any) {
	WriteJSONStatus(w, http.StatusOK, data)
}

// WriteJSONStatus sends a JSON response with a custom status code.
func WriteJSONStatus(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(data); err != nil {
		log.Error("Failed to encode JSON response", "error", err)
	}
}

// WriteJSONSensitive sends a JSON response with no-cache headers.
// Use this for responses containing sensitive data like tokens.
func WriteJSONSensitive(w http.ResponseWriter, data any) {
	WriteJSONSensitiveStatus(w, http.StatusOK, data)
}

// WriteJSONSensitiveStatus sends a JSON response with no-cache headers and a custom status code.
func WriteJSONSensitiveStatus(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate, private")
	w.Header().Set("Pragma", "no-cache")
	WriteJSONStatus(w, status, data)
}

// WriteError sends a JSON error response.
func WriteError(w http.ResponseWriter, message string, status int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(api.ErrorResponse{Error: message}); err != nil {
		log.Error("Failed to encode error response", "error", err)
	}
}
