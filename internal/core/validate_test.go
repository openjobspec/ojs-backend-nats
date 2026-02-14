package core

import (
	"encoding/json"
	"testing"
)

func TestValidateEnqueueRequest_Valid(t *testing.T) {
	req := &EnqueueRequest{
		Type: "email.send",
		Args: json.RawMessage(`["hello@example.com"]`),
	}
	if err := ValidateEnqueueRequest(req); err != nil {
		t.Errorf("ValidateEnqueueRequest() unexpected error: %v", err)
	}
}

func TestValidateEnqueueRequest_MissingType(t *testing.T) {
	req := &EnqueueRequest{
		Args: json.RawMessage(`[]`),
	}
	err := ValidateEnqueueRequest(req)
	if err == nil {
		t.Fatal("ValidateEnqueueRequest() expected error for missing type")
	}
	if err.Code != ErrCodeInvalidRequest {
		t.Errorf("error code = %q, want %q", err.Code, ErrCodeInvalidRequest)
	}
}

func TestValidateEnqueueRequest_InvalidTypeFormat(t *testing.T) {
	tests := []string{
		"UPPERCASE",
		"123start",
		"has spaces",
		"special!chars",
	}
	for _, typ := range tests {
		req := &EnqueueRequest{
			Type: typ,
			Args: json.RawMessage(`[]`),
		}
		if err := ValidateEnqueueRequest(req); err == nil {
			t.Errorf("ValidateEnqueueRequest(type=%q) expected error", typ)
		}
	}
}

func TestValidateEnqueueRequest_ValidTypes(t *testing.T) {
	tests := []string{
		"email",
		"email.send",
		"my-job",
		"my-job.sub-type",
		"a1.b2.c3",
	}
	for _, typ := range tests {
		req := &EnqueueRequest{
			Type: typ,
			Args: json.RawMessage(`[]`),
		}
		if err := ValidateEnqueueRequest(req); err != nil {
			t.Errorf("ValidateEnqueueRequest(type=%q) unexpected error: %v", typ, err)
		}
	}
}

func TestValidateEnqueueRequest_NilArgs(t *testing.T) {
	req := &EnqueueRequest{
		Type: "test",
		Args: nil,
	}
	err := ValidateEnqueueRequest(req)
	if err == nil {
		t.Fatal("expected error for nil args")
	}
}

func TestValidateEnqueueRequest_NullArgs(t *testing.T) {
	req := &EnqueueRequest{
		Type: "test",
		Args: json.RawMessage(`null`),
	}
	err := ValidateEnqueueRequest(req)
	if err == nil {
		t.Fatal("expected error for null args")
	}
}

func TestValidateEnqueueRequest_NonArrayArgs(t *testing.T) {
	req := &EnqueueRequest{
		Type: "test",
		Args: json.RawMessage(`{"key": "value"}`),
	}
	err := ValidateEnqueueRequest(req)
	if err == nil {
		t.Fatal("expected error for object args")
	}
}

func TestValidateEnqueueRequest_InvalidID(t *testing.T) {
	req := &EnqueueRequest{
		Type:  "test",
		Args:  json.RawMessage(`[]`),
		HasID: true,
		ID:    "not-a-uuid",
	}
	err := ValidateEnqueueRequest(req)
	if err == nil {
		t.Fatal("expected error for invalid ID")
	}
}

func TestValidateEnqueueRequest_ValidID(t *testing.T) {
	req := &EnqueueRequest{
		Type:  "test",
		Args:  json.RawMessage(`[]`),
		HasID: true,
		ID:    NewUUIDv7(),
	}
	if err := ValidateEnqueueRequest(req); err != nil {
		t.Errorf("unexpected error for valid ID: %v", err)
	}
}

func TestValidateOptions_InvalidQueue(t *testing.T) {
	req := &EnqueueRequest{
		Type: "test",
		Args: json.RawMessage(`[]`),
		Options: &EnqueueOptions{
			Queue: "INVALID QUEUE",
		},
	}
	err := ValidateEnqueueRequest(req)
	if err == nil {
		t.Fatal("expected error for invalid queue name")
	}
}

func TestValidateOptions_ValidQueue(t *testing.T) {
	queues := []string{"default", "high-priority", "queue.sub", "q1"}
	for _, q := range queues {
		req := &EnqueueRequest{
			Type: "test",
			Args: json.RawMessage(`[]`),
			Options: &EnqueueOptions{
				Queue: q,
			},
		}
		if err := ValidateEnqueueRequest(req); err != nil {
			t.Errorf("unexpected error for queue=%q: %v", q, err)
		}
	}
}

func TestValidateOptions_Priority(t *testing.T) {
	valid := []int{-100, -1, 0, 1, 100}
	for _, p := range valid {
		pp := p
		req := &EnqueueRequest{
			Type:    "test",
			Args:    json.RawMessage(`[]`),
			Options: &EnqueueOptions{Priority: &pp},
		}
		if err := ValidateEnqueueRequest(req); err != nil {
			t.Errorf("unexpected error for priority=%d: %v", p, err)
		}
	}

	invalid := []int{-101, 101, 200, -200}
	for _, p := range invalid {
		pp := p
		req := &EnqueueRequest{
			Type:    "test",
			Args:    json.RawMessage(`[]`),
			Options: &EnqueueOptions{Priority: &pp},
		}
		if err := ValidateEnqueueRequest(req); err == nil {
			t.Errorf("expected error for priority=%d", p)
		}
	}
}

func TestValidateRetryPolicy_InvalidMaxAttempts(t *testing.T) {
	req := &EnqueueRequest{
		Type: "test",
		Args: json.RawMessage(`[]`),
		Options: &EnqueueOptions{
			Retry: &RetryPolicy{MaxAttempts: -1},
		},
	}
	err := ValidateEnqueueRequest(req)
	if err == nil {
		t.Fatal("expected error for negative max_attempts")
	}
}

func TestValidateRetryPolicy_InvalidBackoffCoefficient(t *testing.T) {
	req := &EnqueueRequest{
		Type: "test",
		Args: json.RawMessage(`[]`),
		Options: &EnqueueOptions{
			Retry: &RetryPolicy{BackoffCoefficient: 0.5},
		},
	}
	err := ValidateEnqueueRequest(req)
	if err == nil {
		t.Fatal("expected error for backoff_coefficient < 1.0")
	}
}

func TestValidateRetryPolicy_InvalidDuration(t *testing.T) {
	req := &EnqueueRequest{
		Type: "test",
		Args: json.RawMessage(`[]`),
		Options: &EnqueueOptions{
			Retry: &RetryPolicy{InitialInterval: "invalid"},
		},
	}
	err := ValidateEnqueueRequest(req)
	if err == nil {
		t.Fatal("expected error for invalid initial_interval")
	}
}

func TestValidateRetryPolicy_UsesRetryPolicyAlias(t *testing.T) {
	req := &EnqueueRequest{
		Type: "test",
		Args: json.RawMessage(`[]`),
		Options: &EnqueueOptions{
			RetryPolicy: &RetryPolicy{MaxAttempts: -1},
		},
	}
	err := ValidateEnqueueRequest(req)
	if err == nil {
		t.Fatal("expected error via RetryPolicy alias")
	}
}

func TestDetectJSONType(t *testing.T) {
	tests := []struct {
		input json.RawMessage
		want  string
	}{
		{json.RawMessage(`"hello"`), "string"},
		{json.RawMessage(`42`), "number"},
		{json.RawMessage(`true`), "boolean"},
		{json.RawMessage(`false`), "boolean"},
		{json.RawMessage(`null`), "null"},
		{json.RawMessage(`{}`), "object"},
		{json.RawMessage(`[]`), "array"},
		{json.RawMessage(``), "empty"},
	}

	for _, tt := range tests {
		got := detectJSONType(tt.input)
		if got != tt.want {
			t.Errorf("detectJSONType(%s) = %q, want %q", tt.input, got, tt.want)
		}
	}
}
