package httpresponse

import (
	"encoding/json"
	"net/http"

	"hooklet/internal/api"

	"github.com/charmbracelet/log"
)

// WriteJSON sends a JSON response.
func WriteJSON(w http.ResponseWriter, data any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(data); err != nil {
		log.Error("Failed to encode JSON response", "error", err)
	}
}

// WriteJSONSensitive sends a JSON response with no-cache headers.
// Use this for responses containing sensitive data like tokens.
func WriteJSONSensitive(w http.ResponseWriter, data any) {
	w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate, private")
	w.Header().Set("Pragma", "no-cache")
	WriteJSON(w, data)
}

// WriteError sends a JSON error response.
func WriteError(w http.ResponseWriter, message string, status int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(api.ErrorResponse{Error: message})
}
