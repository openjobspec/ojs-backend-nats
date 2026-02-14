package core

import (
	"encoding/json"
	"testing"
	"time"
)

func TestFormatTime(t *testing.T) {
	ts := time.Date(2024, 6, 15, 12, 30, 45, 123000000, time.UTC)
	got := FormatTime(ts)
	want := "2024-06-15T12:30:45.123Z"
	if got != want {
		t.Errorf("FormatTime() = %q, want %q", got, want)
	}
}

func TestFormatTime_NonUTC(t *testing.T) {
	loc := time.FixedZone("EST", -5*3600)
	ts := time.Date(2024, 6, 15, 12, 0, 0, 0, loc)
	got := FormatTime(ts)
	// Should be converted to UTC: 17:00
	want := "2024-06-15T17:00:00.000Z"
	if got != want {
		t.Errorf("FormatTime(non-UTC) = %q, want %q", got, want)
	}
}

func TestNowFormatted(t *testing.T) {
	result := NowFormatted()
	if result == "" {
		t.Fatal("NowFormatted() returned empty string")
	}
	_, err := time.Parse(TimeFormat, result)
	if err != nil {
		t.Errorf("NowFormatted() = %q, not parseable: %v", result, err)
	}
}

func TestJobMarshalJSON(t *testing.T) {
	job := Job{
		ID:    "test-id",
		Type:  "email.send",
		State: StateAvailable,
		Queue: "default",
		Args:  json.RawMessage(`["test@example.com"]`),
	}

	data, err := json.Marshal(&job)
	if err != nil {
		t.Fatalf("MarshalJSON() error: %v", err)
	}

	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("Unmarshal output error: %v", err)
	}

	if m["id"] != "test-id" {
		t.Errorf("id = %v, want %q", m["id"], "test-id")
	}
	if m["type"] != "email.send" {
		t.Errorf("type = %v, want %q", m["type"], "email.send")
	}
	if m["state"] != StateAvailable {
		t.Errorf("state = %v, want %q", m["state"], StateAvailable)
	}
}

func TestJobMarshalJSON_OmitsEmptyFields(t *testing.T) {
	job := Job{
		ID:    "test-id",
		Type:  "test",
		State: StateAvailable,
		Queue: "default",
	}

	data, err := json.Marshal(&job)
	if err != nil {
		t.Fatalf("MarshalJSON() error: %v", err)
	}

	var m map[string]any
	json.Unmarshal(data, &m)

	// These should not be present when empty
	for _, field := range []string{"started_at", "completed_at", "cancelled_at", "result", "error", "tags", "errors"} {
		if _, exists := m[field]; exists {
			t.Errorf("field %q should be omitted when empty", field)
		}
	}
}

func TestJobMarshalJSON_UnknownFields(t *testing.T) {
	job := Job{
		ID:    "test-id",
		Type:  "test",
		State: StateAvailable,
		Queue: "default",
		UnknownFields: map[string]json.RawMessage{
			"custom_field": json.RawMessage(`"custom_value"`),
		},
	}

	data, err := json.Marshal(&job)
	if err != nil {
		t.Fatalf("MarshalJSON() error: %v", err)
	}

	var m map[string]any
	json.Unmarshal(data, &m)

	if m["custom_field"] != "custom_value" {
		t.Errorf("custom_field = %v, want %q", m["custom_field"], "custom_value")
	}
}

func TestParseEnqueueRequest(t *testing.T) {
	input := `{"type":"email.send","args":["test@example.com"],"custom":"value"}`
	req, err := ParseEnqueueRequest([]byte(input))
	if err != nil {
		t.Fatalf("ParseEnqueueRequest() error: %v", err)
	}

	if req.Type != "email.send" {
		t.Errorf("Type = %q, want %q", req.Type, "email.send")
	}
	if req.HasID {
		t.Error("HasID should be false when id not provided")
	}
	if string(req.UnknownFields["custom"]) != `"value"` {
		t.Errorf("UnknownFields[custom] = %s, want %q", req.UnknownFields["custom"], `"value"`)
	}
}

func TestParseEnqueueRequest_WithID(t *testing.T) {
	id := NewUUIDv7()
	input := `{"id":"` + id + `","type":"test","args":[]}`
	req, err := ParseEnqueueRequest([]byte(input))
	if err != nil {
		t.Fatalf("ParseEnqueueRequest() error: %v", err)
	}
	if !req.HasID {
		t.Error("HasID should be true when id is provided")
	}
	if req.ID != id {
		t.Errorf("ID = %q, want %q", req.ID, id)
	}
}
