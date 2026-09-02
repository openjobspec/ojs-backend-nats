package nats

import (
	"encoding/json"
	"testing"

	"github.com/openjobspec/ojs-backend-nats/internal/core"
)

func TestBuildJobErrorPayload_Nil(t *testing.T) {
	data, err := buildJobErrorPayload(nil, 0)
	if err != nil {
		t.Fatalf("buildJobErrorPayload(nil) error = %v", err)
	}
	if data != nil {
		t.Fatalf("buildJobErrorPayload(nil) = %q, want nil", data)
	}
}

func TestBuildJobErrorPayload_Fields(t *testing.T) {
	retryable := false
	jobErr := &core.JobError{
		Message:   "boom",
		Code:      "code_val",
		Type:      "type_val",
		Retryable: &retryable,
		Details:   map[string]any{"k": "v"},
	}

	data, err := buildJobErrorPayload(jobErr, 2)
	if err != nil {
		t.Fatalf("buildJobErrorPayload() error = %v", err)
	}

	var got map[string]any
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}

	if got["message"] != "boom" {
		t.Errorf("message = %v, want boom", got["message"])
	}
	if got["attempt"].(float64) != 2 {
		t.Errorf("attempt = %v, want 2", got["attempt"])
	}
	// Type takes precedence over Code when both are set.
	if got["type"] != "type_val" {
		t.Errorf("type = %v, want type_val", got["type"])
	}
	if got["retryable"] != false {
		t.Errorf("retryable = %v, want false", got["retryable"])
	}
	if _, ok := got["details"]; !ok {
		t.Error("details missing from payload")
	}
}

func TestBuildJobErrorPayload_CodeOnly(t *testing.T) {
	data, err := buildJobErrorPayload(&core.JobError{Message: "m", Code: "only_code"}, 0)
	if err != nil {
		t.Fatalf("buildJobErrorPayload() error = %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if got["type"] != "only_code" {
		t.Errorf("type = %v, want only_code", got["type"])
	}
	if _, ok := got["retryable"]; ok {
		t.Error("retryable should be omitted when nil")
	}
}
