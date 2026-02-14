package api

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/openjobspec/ojs-backend-nats/internal/core"
)

// QueueHandler handles queue-related HTTP endpoints.
type QueueHandler struct {
	backend core.Backend
}

// NewQueueHandler creates a new QueueHandler.
func NewQueueHandler(backend core.Backend) *QueueHandler {
	return &QueueHandler{backend: backend}
}

// List handles GET /ojs/v1/queues
func (h *QueueHandler) List(w http.ResponseWriter, r *http.Request) {
	queues, err := h.backend.ListQueues(r.Context())
	if err != nil {
		WriteError(w, http.StatusInternalServerError, core.NewInternalError(err.Error()))
		return
	}

	WriteJSON(w, http.StatusOK, map[string]any{
		"queues": queues,
		"pagination": map[string]any{
			"total":    len(queues),
			"limit":    50,
			"offset":   0,
			"has_more": false,
		},
	})
}

// Stats handles GET /ojs/v1/queues/:name/stats
func (h *QueueHandler) Stats(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")

	stats, err := h.backend.QueueStats(r.Context(), name)
	if err != nil {
		if ojsErr, ok := err.(*core.OJSError); ok {
			if ojsErr.Code == core.ErrCodeNotFound {
				WriteError(w, http.StatusNotFound, ojsErr)
				return
			}
		}
		WriteError(w, http.StatusInternalServerError, core.NewInternalError(err.Error()))
		return
	}

	WriteJSON(w, http.StatusOK, map[string]any{
		"queue": map[string]any{
			"name":      stats.Queue,
			"available": stats.Stats.Available,
			"active":    stats.Stats.Active,
			"completed": stats.Stats.Completed,
			"paused":    stats.Status == "paused",
		},
	})
}

// Pause handles POST /ojs/v1/queues/:name/pause
func (h *QueueHandler) Pause(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")

	if err := h.backend.PauseQueue(r.Context(), name); err != nil {
		WriteError(w, http.StatusInternalServerError, core.NewInternalError(err.Error()))
		return
	}

	WriteJSON(w, http.StatusOK, map[string]any{
		"queue": map[string]any{
			"name":   name,
			"paused": true,
		},
	})
}

// Resume handles POST /ojs/v1/queues/:name/resume
func (h *QueueHandler) Resume(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")

	if err := h.backend.ResumeQueue(r.Context(), name); err != nil {
		WriteError(w, http.StatusInternalServerError, core.NewInternalError(err.Error()))
		return
	}

	WriteJSON(w, http.StatusOK, map[string]any{
		"queue": map[string]any{
			"name":   name,
			"paused": false,
		},
	})
}
