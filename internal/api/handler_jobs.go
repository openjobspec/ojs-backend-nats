package api

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/openjobspec/ojs-backend-nats/internal/core"
)

// JobHandler handles job-related HTTP endpoints.
type JobHandler struct {
	backend   core.Backend
	publisher core.EventPublisher
}

// NewJobHandler creates a new JobHandler.
func NewJobHandler(backend core.Backend) *JobHandler {
	return &JobHandler{backend: backend}
}

// SetEventPublisher sets the event publisher for real-time notifications.
func (h *JobHandler) SetEventPublisher(pub core.EventPublisher) {
	h.publisher = pub
}

// Create handles POST /ojs/v1/jobs
func (h *JobHandler) Create(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		WriteError(w, http.StatusBadRequest, core.NewInvalidRequestError("Failed to read request body.", nil))
		return
	}

	req, err := core.ParseEnqueueRequest(body)
	if err != nil {
		WriteError(w, http.StatusBadRequest, core.NewInvalidRequestError("Invalid JSON in request body.", nil))
		return
	}

	if ojsErr := core.ValidateEnqueueRequest(req); ojsErr != nil {
		status := http.StatusBadRequest
		if ojsErr.Code == core.ErrCodeValidationError {
			status = http.StatusUnprocessableEntity
		}
		WriteError(w, status, ojsErr)
		return
	}

	job := requestToJob(req)

	created, pushErr := h.backend.Push(r.Context(), job)
	if pushErr != nil {
		if ojsErr, ok := pushErr.(*core.OJSError); ok {
			status := http.StatusInternalServerError
			switch ojsErr.Code {
			case core.ErrCodeDuplicate:
				status = http.StatusConflict
			case core.ErrCodeInvalidRequest:
				status = http.StatusBadRequest
			}
			WriteError(w, status, ojsErr)
			return
		}
		WriteError(w, http.StatusInternalServerError, core.NewInternalError(pushErr.Error()))
		return
	}

	w.Header().Set("Location", "/ojs/v1/jobs/"+created.ID)
	status := http.StatusCreated
	if created.IsExisting {
		status = http.StatusOK
	}

	// Publish real-time event
	if h.publisher != nil {
		_ = h.publisher.PublishJobEvent(core.NewStateChangedEvent(
			created.ID, created.Queue, created.Type, "", created.State,
		))
	}

	WriteJSON(w, status, map[string]any{"job": created})
}

// Get handles GET /ojs/v1/jobs/:id
func (h *JobHandler) Get(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	job, err := h.backend.Info(r.Context(), id)
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

	WriteJSON(w, http.StatusOK, map[string]any{"job": job})
}

// Cancel handles DELETE /ojs/v1/jobs/:id
func (h *JobHandler) Cancel(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	job, err := h.backend.Cancel(r.Context(), id)
	if err != nil {
		if ojsErr, ok := err.(*core.OJSError); ok {
			switch ojsErr.Code {
			case core.ErrCodeNotFound:
				WriteError(w, http.StatusNotFound, ojsErr)
				return
			case core.ErrCodeConflict, core.ErrCodeInvalidRequest:
				WriteError(w, http.StatusConflict, ojsErr)
				return
			}
		}
		WriteError(w, http.StatusInternalServerError, core.NewInternalError(err.Error()))
		return
	}

	// Publish real-time event
	if h.publisher != nil {
		_ = h.publisher.PublishJobEvent(core.NewStateChangedEvent(
			job.ID, job.Queue, job.Type, "", core.StateCancelled,
		))
	}

	WriteJSON(w, http.StatusOK, map[string]any{"job": job})
}

func requestToJob(req *core.EnqueueRequest) *core.Job {
	job := &core.Job{
		Type:          req.Type,
		Args:          req.Args,
		Meta:          req.Meta,
		Queue:         "default",
		UnknownFields: req.UnknownFields,
	}

	if req.ID != "" {
		job.ID = req.ID
	}

	if req.Options != nil {
		if req.Options.Queue != "" {
			job.Queue = req.Options.Queue
		}
		if req.Options.Priority != nil {
			job.Priority = req.Options.Priority
		}
		if req.Options.TimeoutMs != nil {
			job.TimeoutMs = req.Options.TimeoutMs
		}
		if req.Options.Tags != nil {
			job.Tags = req.Options.Tags
		}
		retryPolicy := req.Options.Retry
		if retryPolicy == nil {
			retryPolicy = req.Options.RetryPolicy
		}
		if retryPolicy != nil {
			job.Retry = retryPolicy
			job.MaxAttempts = &retryPolicy.MaxAttempts
		}
		if req.Options.Unique != nil {
			job.Unique = req.Options.Unique
		}
		// Handle scheduled_at / delay_until: prefer scheduled_at, fall back to delay_until
		scheduledAtRaw := req.Options.ScheduledAt
		if scheduledAtRaw == "" {
			scheduledAtRaw = req.Options.DelayUntil
		}
		if scheduledAtRaw != "" {
			job.ScheduledAt = resolveRelativeTime(scheduledAtRaw)
		}
		if req.Options.ExpiresAt != "" {
			job.ExpiresAt = resolveRelativeTime(req.Options.ExpiresAt)
		}
		if req.Options.RateLimit != nil {
			job.RateLimit = req.Options.RateLimit
		}
		if req.Options.Metadata != nil && len(req.Options.Metadata) > 0 {
			job.Meta = req.Options.Metadata
		}
		if req.Options.VisibilityTimeoutMs != nil {
			job.VisibilityTimeoutMs = req.Options.VisibilityTimeoutMs
		} else if req.Options.VisibilityTimeout != "" {
			if d, err := core.ParseISO8601Duration(req.Options.VisibilityTimeout); err == nil {
				ms := int(d.Milliseconds())
				job.VisibilityTimeoutMs = &ms
			}
		}
	}

	// Apply default retry policy max_attempts if not set
	if job.MaxAttempts == nil {
		defaultMax := core.DefaultRetryPolicy().MaxAttempts
		job.MaxAttempts = &defaultMax
	}

	// Set error field from request body if present (for unknown fields)
	if req.UnknownFields != nil {
		if errField, ok := req.UnknownFields["error"]; ok {
			job.Error = json.RawMessage(errField)
			delete(job.UnknownFields, "error")
		}
	}

	return job
}

// resolveRelativeTime converts a relative ISO 8601 duration prefixed with "+"
// (e.g., "+PT5S") into an absolute RFC3339 timestamp by adding the duration to
// the current time. If the value is not a relative format, it is returned as-is.
func resolveRelativeTime(value string) string {
	if strings.HasPrefix(value, "+PT") {
		durStr := value[1:] // strip the leading "+"
		d, err := core.ParseISO8601Duration(durStr)
		if err == nil {
			return time.Now().Add(d).UTC().Format(time.RFC3339)
		}
	}
	return value
}
