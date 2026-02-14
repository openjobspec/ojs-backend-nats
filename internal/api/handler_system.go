package api

import (
	"net/http"

	"github.com/openjobspec/ojs-backend-nats/internal/core"
)

// SystemHandler handles system-related HTTP endpoints.
type SystemHandler struct {
	backend core.Backend
}

// NewSystemHandler creates a new SystemHandler.
func NewSystemHandler(backend core.Backend) *SystemHandler {
	return &SystemHandler{backend: backend}
}

// Manifest handles GET /ojs/manifest
func (h *SystemHandler) Manifest(w http.ResponseWriter, r *http.Request) {
	WriteJSON(w, http.StatusOK, map[string]any{
		"specversion": core.OJSVersion,
		"implementation": map[string]any{
			"name":    "ojs-backend-nats",
			"version": "0.1.0",
			"backend": "nats",
		},
		"levels": []int{0, 1, 2, 3, 4},
		"capabilities": []string{
			"push", "fetch", "ack", "fail", "cancel", "info",
			"heartbeat", "dead-letter", "retry", "cron",
			"scheduled", "workflows", "batch", "priority",
			"unique", "queue-pause", "queue-stats",
		},
	})
}

// Health handles GET /ojs/v1/health
func (h *SystemHandler) Health(w http.ResponseWriter, r *http.Request) {
	resp, err := h.backend.Health(r.Context())
	if err != nil {
		WriteJSON(w, http.StatusServiceUnavailable, resp)
		return
	}

	status := http.StatusOK
	if resp.Status != "ok" {
		status = http.StatusServiceUnavailable
	}

	WriteJSON(w, status, resp)
}
