package api

import (
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/openjobspec/ojs-backend-nats/internal/core"
	commonmw "github.com/openjobspec/ojs-go-backend-common/middleware"
)

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
		wrapped, sw := NewStatusResponseWriter(w)
		next.ServeHTTP(wrapped, r)
		slog.Info("http request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", sw.Status(),
			"duration_ms", time.Since(start).Milliseconds(),
			"request_id", sw.Header().Get("X-Request-Id"),
		)
	})
}

// LimitBody middleware restricts request body size.
func LimitBody(next http.Handler) http.Handler {
	return commonmw.LimitRequestBody(next)
}

// ValidateContentType middleware validates the Content-Type header for mutation
// requests (POST, PUT, PATCH).
//
// Per the OJS HTTP binding (§4.1), servers MUST reject request bodies with an
// *unsupported* content type. An absent Content-Type header is not an
// unsupported type, so it is allowed and left to body parsing to reject if the
// payload is malformed. A present header must be either the OJS media type or
// the permitted `application/json` alias (charset parameters are ignored).
func ValidateContentType(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isMutationMethod(r.Method) {
			if ct := r.Header.Get("Content-Type"); ct != "" && !isSupportedMediaType(ct) {
				WriteError(w, http.StatusBadRequest, core.NewInvalidRequestError(
					"Unsupported Content-Type. Expected 'application/openjobspec+json' or 'application/json'.",
					map[string]any{"received": ct},
				))
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// isMutationMethod reports whether the HTTP method carries a request body that
// requires Content-Type validation.
func isMutationMethod(method string) bool {
	return method == http.MethodPost || method == http.MethodPut || method == http.MethodPatch
}

// isSupportedMediaType reports whether the Content-Type header names a media
// type accepted by OJS, ignoring any parameters such as `charset`.
func isSupportedMediaType(contentType string) bool {
	mediaType := strings.TrimSpace(strings.Split(contentType, ";")[0])
	return mediaType == core.OJSMediaType || mediaType == "application/json"
}
