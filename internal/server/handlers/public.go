package handlers

import (
	"net/http"
	"time"

	"hooklet/internal/api"
	"hooklet/internal/server/httpresponse"
)

// PublicHandler hosts public routes (status).
type PublicHandler struct {
	StartedAt       time.Time
	RabbitConnected func() bool
}

// NewPublicHandler creates a handler for public endpoints.
func NewPublicHandler(startedAt time.Time, rabbitConnected func() bool) *PublicHandler {
	return &PublicHandler{
		StartedAt:       startedAt,
		RabbitConnected: rabbitConnected,
	}
}

// Status handles GET /api/status.
func (h *PublicHandler) Status(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		httpresponse.WriteError(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	rabbitStatus := "disconnected"
	if h.RabbitConnected != nil && h.RabbitConnected() {
		rabbitStatus = "connected"
	}

	status := api.StatusResponse{
		Status:    "ok",
		Uptime:    time.Since(h.StartedAt).Round(time.Second).String(),
		StartedAt: h.StartedAt,
		RabbitMQ:  rabbitStatus,
	}

	httpresponse.WriteJSON(w, status)
}
