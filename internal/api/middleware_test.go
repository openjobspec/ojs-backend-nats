package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/openjobspec/ojs-backend-nats/internal/core"
)

func TestOJSHeaders_SetsVersion(t *testing.T) {
	handler := OJSHeaders(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if got := rec.Header().Get("OJS-Version"); got != core.OJSVersion {
		t.Errorf("OJS-Version = %q, want %q", got, core.OJSVersion)
	}
	if got := rec.Header().Get("Content-Type"); got != core.OJSMediaType {
		t.Errorf("Content-Type = %q, want %q", got, core.OJSMediaType)
	}
}

func TestOJSHeaders_GeneratesRequestID(t *testing.T) {
	handler := OJSHeaders(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	reqID := rec.Header().Get("X-Request-Id")
	if reqID == "" {
		t.Fatal("X-Request-Id should be generated when not provided")
	}
	if !strings.HasPrefix(reqID, "req_") {
		t.Errorf("X-Request-Id = %q, should start with 'req_'", reqID)
	}
}

func TestOJSHeaders_EchoesRequestID(t *testing.T) {
	handler := OJSHeaders(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Request-Id", "custom-id-123")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if got := rec.Header().Get("X-Request-Id"); got != "custom-id-123" {
		t.Errorf("X-Request-Id = %q, want %q", got, "custom-id-123")
	}
}

func TestValidateContentType_AcceptsOJSMediaType(t *testing.T) {
	called := false
	handler := ValidateContentType(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	}))

	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", core.OJSMediaType)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if !called {
		t.Error("handler should be called for OJS media type")
	}
}

func TestValidateContentType_AcceptsJSON(t *testing.T) {
	called := false
	handler := ValidateContentType(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	}))

	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if !called {
		t.Error("handler should be called for application/json")
	}
}

func TestValidateContentType_AcceptsJSONWithCharset(t *testing.T) {
	called := false
	handler := ValidateContentType(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	}))

	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if !called {
		t.Error("handler should be called for application/json with charset")
	}
}

func TestValidateContentType_RejectsInvalidType(t *testing.T) {
	called := false
	handler := ValidateContentType(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	}))

	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`data`))
	req.Header.Set("Content-Type", "text/plain")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if called {
		t.Error("handler should not be called for text/plain")
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
}

func TestValidateContentType_AllowsGET(t *testing.T) {
	called := false
	handler := ValidateContentType(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Content-Type", "text/plain")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if !called {
		t.Error("GET requests should not validate Content-Type")
	}
}

func TestValidateContentType_AllowsEmptyContentType(t *testing.T) {
	called := false
	handler := ValidateContentType(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	}))

	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{}`))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if !called {
		t.Error("empty Content-Type should be allowed")
	}
}

func TestRequestLogger_CapturesStatus(t *testing.T) {
	handler := RequestLogger(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
	}))

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusCreated)
	}
}

func TestStatusWriter_DefaultStatus(t *testing.T) {
	rec := httptest.NewRecorder()
	sw := &statusWriter{ResponseWriter: rec, status: http.StatusOK}

	// Without calling WriteHeader, status should default to 200
	if sw.status != http.StatusOK {
		t.Errorf("default status = %d, want %d", sw.status, http.StatusOK)
	}
}

func TestStatusWriter_WriteHeader(t *testing.T) {
	rec := httptest.NewRecorder()
	sw := &statusWriter{ResponseWriter: rec, status: http.StatusOK}
	sw.WriteHeader(http.StatusNotFound)

	if sw.status != http.StatusNotFound {
		t.Errorf("status = %d, want %d", sw.status, http.StatusNotFound)
	}
}

func TestLimitBody_SetsMaxBytesReader(t *testing.T) {
	handler := LimitBody(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Try to read more than MaxBodySize
		buf := make([]byte, MaxBodySize+1)
		_, err := r.Body.Read(buf)
		if err == nil {
			t.Error("expected error reading oversized body")
		}
	}))

	body := strings.NewReader(strings.Repeat("x", MaxBodySize+1))
	req := httptest.NewRequest(http.MethodPost, "/", body)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
}
