package api

import (
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/openjobspec/ojs-backend-nats/internal/core"
)

// OJSHeaders middleware adds required OJS response headers.
func OJSHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("OJS-Version", core.OJSVersion)
		w.Header().Set("Content-Type", core.OJSMediaType)

		// Generate or echo X-Request-Id
		reqID := r.Header.Get("X-Request-Id")
		if reqID == "" {
			reqID = "req_" + core.NewUUIDv7()
		}
		w.Header().Set("X-Request-Id", reqID)

		next.ServeHTTP(w, r)
	})
}

// RequestLogger middleware logs HTTP requests with structured logging.
func RequestLogger(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(sw, r)
		slog.Info("http request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", sw.status,
			"duration_ms", time.Since(start).Milliseconds(),
			"request_id", w.Header().Get("X-Request-Id"),
		)
	})
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(status int) {
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

// MaxBodySize limits request body size to prevent OOM from oversized payloads.
const MaxBodySize = 10 * 1024 * 1024 // 10 MB

// LimitBody middleware restricts request body size.
func LimitBody(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Body != nil {
			r.Body = http.MaxBytesReader(w, r.Body, MaxBodySize)
		}
		next.ServeHTTP(w, r)
	})
}

// ValidateContentType middleware validates the Content-Type header for POST requests.
func ValidateContentType(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost || r.Method == http.MethodPut || r.Method == http.MethodPatch {
			ct := r.Header.Get("Content-Type")
			if ct != "" {
				// Extract media type (ignore parameters like charset)
				mediaType := strings.Split(ct, ";")[0]
				mediaType = strings.TrimSpace(mediaType)
				if mediaType != core.OJSMediaType && mediaType != "application/json" {
					WriteError(w, http.StatusBadRequest, core.NewInvalidRequestError(
						"Unsupported Content-Type. Expected 'application/openjobspec+json' or 'application/json'.",
						map[string]any{
							"received": ct,
						},
					))
					return
				}
			}
		}
		next.ServeHTTP(w, r)
	})
}
