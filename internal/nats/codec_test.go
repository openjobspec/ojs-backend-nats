package nats

import (
	"encoding/json"
	"testing"

	"github.com/openjobspec/ojs-backend-nats/internal/core"
)

func TestJobToStateRoundTrip(t *testing.T) {
	original := &core.Job{
		ID:      "job_01",
		Type:    "email.send",
		State:   "available",
		Queue:   "emails",
		Args:    json.RawMessage(`["user@example.com","Welcome"]`),
		Attempt: 1,
	}

	state := jobToState(original)
	if state.ID != original.ID {
		t.Errorf("ID mismatch: got %q, want %q", state.ID, original.ID)
	}
	if state.Type != original.Type {
		t.Errorf("Type mismatch: got %q, want %q", state.Type, original.Type)
	}

	back := stateToJob(state)
	if back.ID != original.ID {
		t.Errorf("round-trip ID mismatch: got %q, want %q", back.ID, original.ID)
	}
	if back.Queue != original.Queue {
		t.Errorf("round-trip Queue mismatch: got %q, want %q", back.Queue, original.Queue)
	}
	if back.Attempt != original.Attempt {
		t.Errorf("round-trip Attempt mismatch: got %d, want %d", back.Attempt, original.Attempt)
	}
}

func TestMarshalUnmarshalJobState(t *testing.T) {
	priority := 5
	maxAttempts := 3
	job := &core.Job{
		ID:          "job_02",
		Type:        "payment.charge",
		State:       "active",
		Queue:       "payments",
		Args:        json.RawMessage(`[99.99]`),
		Priority:    &priority,
		MaxAttempts: &maxAttempts,
		Tags:        []string{"vip", "urgent"},
		CreatedAt:   "2026-03-01T00:00:00Z",
	}

	data, err := marshalJobState(job)
	if err != nil {
		t.Fatalf("marshal error: %v", err)
	}

	restored, err := unmarshalJobState(data)
	if err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}

	if restored.ID != job.ID {
		t.Errorf("ID mismatch: got %q, want %q", restored.ID, job.ID)
	}
	if restored.Priority == nil || *restored.Priority != priority {
		t.Errorf("Priority mismatch: got %v, want %d", restored.Priority, priority)
	}
	if restored.MaxAttempts == nil || *restored.MaxAttempts != maxAttempts {
		t.Errorf("MaxAttempts mismatch: got %v, want %d", restored.MaxAttempts, maxAttempts)
	}
	if len(restored.Tags) != 2 || restored.Tags[0] != "vip" {
		t.Errorf("Tags mismatch: got %v", restored.Tags)
	}
}

func TestMarshalJobStateWithRetryPolicy(t *testing.T) {
	job := &core.Job{
		ID:    "job_03",
		Type:  "webhook.send",
		State: "retryable",
		Queue: "webhooks",
		Args:  json.RawMessage(`[]`),
		Retry: &core.RetryPolicy{
			MaxAttempts: 5,
			BackoffType: "exponential",
		},
	}

	data, err := marshalJobState(job)
	if err != nil {
		t.Fatalf("marshal error: %v", err)
	}

	restored, err := unmarshalJobState(data)
	if err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}

	if restored.Retry == nil {
		t.Fatal("expected retry policy, got nil")
	}
	if restored.Retry.MaxAttempts != 5 {
		t.Errorf("Retry.MaxAttempts: got %d, want 5", restored.Retry.MaxAttempts)
	}
	if restored.Retry.BackoffType != "exponential" {
		t.Errorf("Retry.BackoffType: got %q, want 'exponential'", restored.Retry.BackoffType)
	}
}

func TestMarshalJobStateWithWorkflow(t *testing.T) {
	job := &core.Job{
		ID:           "job_04",
		Type:         "data.transform",
		State:        "active",
		Queue:        "default",
		Args:         json.RawMessage(`[]`),
		WorkflowID:   "wf_001",
		WorkflowStep: 2,
	}

	data, err := marshalJobState(job)
	if err != nil {
		t.Fatalf("marshal error: %v", err)
	}

	restored, err := unmarshalJobState(data)
	if err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}

	if restored.WorkflowID != "wf_001" {
		t.Errorf("WorkflowID: got %q, want 'wf_001'", restored.WorkflowID)
	}
	if restored.WorkflowStep != 2 {
		t.Errorf("WorkflowStep: got %d, want 2", restored.WorkflowStep)
	}
}

func TestUnmarshalInvalidJSON(t *testing.T) {
	_, err := unmarshalJobState([]byte("not json"))
	if err == nil {
		t.Error("expected error for invalid JSON, got nil")
	}
}

func TestKvStatsKey(t *testing.T) {
	tests := []struct {
		queue, stat, want string
	}{
		{"default", "completed", "default.completed"},
		{"emails", "active", "emails.active"},
		{"high-priority", "scheduled", "high-priority.scheduled"},
	}
	for _, tt := range tests {
		got := kvStatsKey(tt.queue, tt.stat)
		if got != tt.want {
			t.Errorf("kvStatsKey(%q, %q) = %q, want %q", tt.queue, tt.stat, got, tt.want)
		}
	}
}

func TestIntToStr(t *testing.T) {
	tests := []struct {
		input int
		want  string
	}{
		{0, "0"},
		{1, "1"},
		{42, "42"},
		{-1, "-1"},
	}
	for _, tt := range tests {
		got := intToStr(tt.input)
		if got != tt.want {
			t.Errorf("intToStr(%d) = %q, want %q", tt.input, got, tt.want)
		}
	}
}
