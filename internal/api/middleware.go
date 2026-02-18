package api

import (
	"log/slog"
	"net/http"
	"time"

	commonmw "github.com/openjobspec/ojs-go-backend-common/middleware"
)

// maxRequestBodySize is the maximum allowed request body size (1 MB).
const maxRequestBodySize = 1 << 20

// MaxBodySize limits request body size to prevent OOM from oversized payloads.
const MaxBodySize = 10 * 1024 * 1024 // 10 MB

// OJSHeaders middleware adds required OJS response headers.
func OJSHeaders(next http.Handler) http.Handler {
	return commonmw.OJSHeaders(next)
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

// LimitBody middleware restricts request body size.
func LimitBody(next http.Handler) http.Handler {
	return commonmw.LimitRequestBody(next)
}

// ValidateContentType middleware validates the Content-Type header for POST requests.
func ValidateContentType(next http.Handler) http.Handler {
	return commonmw.ValidateContentType(next)
}
