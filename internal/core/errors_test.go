package core

import "testing"

func TestOJSError_Error(t *testing.T) {
	err := &OJSError{Code: "not_found", Message: "Job 'abc' not found."}
	got := err.Error()
	want := "[not_found] Job 'abc' not found."
	if got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
}

func TestNewInvalidRequestError(t *testing.T) {
	err := NewInvalidRequestError("bad input", map[string]any{"field": "type"})
	if err.Code != ErrCodeInvalidRequest {
		t.Errorf("Code = %q, want %q", err.Code, ErrCodeInvalidRequest)
	}
	if err.Retryable {
		t.Error("expected Retryable = false")
	}
	if err.Details["field"] != "type" {
		t.Errorf("Details[field] = %v, want %q", err.Details["field"], "type")
	}
}

func TestNewNotFoundError(t *testing.T) {
	err := NewNotFoundError("Job", "123")
	if err.Code != ErrCodeNotFound {
		t.Errorf("Code = %q, want %q", err.Code, ErrCodeNotFound)
	}
	if err.Details["resource_type"] != "Job" {
		t.Errorf("Details[resource_type] = %v, want %q", err.Details["resource_type"], "Job")
	}
	if err.Details["resource_id"] != "123" {
		t.Errorf("Details[resource_id] = %v, want %q", err.Details["resource_id"], "123")
	}
}

func TestNewValidationError(t *testing.T) {
	err := NewValidationError("invalid field", nil)
	if err.Code != ErrCodeValidationError {
		t.Errorf("Code = %q, want %q", err.Code, ErrCodeValidationError)
	}
	if err.Retryable {
		t.Error("expected Retryable = false")
	}
}

func TestNewConflictError(t *testing.T) {
	err := NewConflictError("already processed", map[string]any{"job_id": "abc"})
	if err.Code != ErrCodeConflict {
		t.Errorf("Code = %q, want %q", err.Code, ErrCodeConflict)
	}
	if err.Retryable {
		t.Error("expected Retryable = false")
	}
	if err.Details["job_id"] != "abc" {
		t.Errorf("Details[job_id] = %v, want %q", err.Details["job_id"], "abc")
	}
}

func TestNewInternalError(t *testing.T) {
	err := NewInternalError("something broke")
	if err.Code != ErrCodeInternalError {
		t.Errorf("Code = %q, want %q", err.Code, ErrCodeInternalError)
	}
	if !err.Retryable {
		t.Error("expected Retryable = true for internal errors")
	}
}
