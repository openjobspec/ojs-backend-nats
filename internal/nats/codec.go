package nats

import (
	"encoding/json"
	"strconv"

	"github.com/openjobspec/ojs-backend-nats/internal/core"
)

// jobState is the JSON-serializable state stored in NATS KV for each job.
type jobState struct {
	ID                  string            `json:"id"`
	Type                string            `json:"type"`
	State               string            `json:"state"`
	Queue               string            `json:"queue"`
	Args                json.RawMessage   `json:"args,omitempty"`
	Meta                json.RawMessage   `json:"meta,omitempty"`
	Priority            *int              `json:"priority,omitempty"`
	Attempt             int               `json:"attempt"`
	MaxAttempts         *int              `json:"max_attempts,omitempty"`
	TimeoutMs           *int              `json:"timeout_ms,omitempty"`
	CreatedAt           string            `json:"created_at,omitempty"`
	EnqueuedAt          string            `json:"enqueued_at,omitempty"`
	StartedAt           string            `json:"started_at,omitempty"`
	CompletedAt         string            `json:"completed_at,omitempty"`
	CancelledAt         string            `json:"cancelled_at,omitempty"`
	ScheduledAt         string            `json:"scheduled_at,omitempty"`
	Result              json.RawMessage   `json:"result,omitempty"`
	Error               json.RawMessage   `json:"error,omitempty"`
	Errors              []json.RawMessage `json:"errors,omitempty"`
	Tags                []string          `json:"tags,omitempty"`
	Retry               *core.RetryPolicy `json:"retry,omitempty"`
	Unique              *core.UniquePolicy `json:"unique,omitempty"`
	ExpiresAt           string            `json:"expires_at,omitempty"`
	RetryDelayMs        *int64            `json:"retry_delay_ms,omitempty"`
	ParentResults       []json.RawMessage `json:"parent_results,omitempty"`
	VisibilityTimeoutMs *int              `json:"visibility_timeout_ms,omitempty"`
	WorkflowID          string            `json:"workflow_id,omitempty"`
	WorkflowStep        int               `json:"workflow_step,omitempty"`
	WorkerID            string            `json:"worker_id,omitempty"`
	RateLimitMaxPerSec  int               `json:"rate_limit_max_per_sec,omitempty"`

	// Unknown fields for forward compatibility
	UnknownFields map[string]json.RawMessage `json:"unknown_fields,omitempty"`
}

// jobToState converts a core.Job to a jobState for KV storage.
func jobToState(job *core.Job) *jobState {
	s := &jobState{
		ID:                  job.ID,
		Type:                job.Type,
		State:               job.State,
		Queue:               job.Queue,
		Args:                job.Args,
		Meta:                job.Meta,
		Priority:            job.Priority,
		Attempt:             job.Attempt,
		MaxAttempts:         job.MaxAttempts,
		TimeoutMs:           job.TimeoutMs,
		CreatedAt:           job.CreatedAt,
		EnqueuedAt:          job.EnqueuedAt,
		StartedAt:           job.StartedAt,
		CompletedAt:         job.CompletedAt,
		CancelledAt:         job.CancelledAt,
		ScheduledAt:         job.ScheduledAt,
		Result:              job.Result,
		Error:               job.Error,
		Errors:              job.Errors,
		Tags:                job.Tags,
		Retry:               job.Retry,
		Unique:              job.Unique,
		ExpiresAt:           job.ExpiresAt,
		RetryDelayMs:        job.RetryDelayMs,
		ParentResults:       job.ParentResults,
		VisibilityTimeoutMs: job.VisibilityTimeoutMs,
		WorkflowID:          job.WorkflowID,
		WorkflowStep:        job.WorkflowStep,
		UnknownFields:       job.UnknownFields,
	}
	if job.RateLimit != nil {
		s.RateLimitMaxPerSec = job.RateLimit.MaxPerSecond
	}
	return s
}

// stateToJob converts a jobState from KV back to a core.Job.
func stateToJob(s *jobState) *core.Job {
	job := &core.Job{
		ID:                  s.ID,
		Type:                s.Type,
		State:               s.State,
		Queue:               s.Queue,
		Args:                s.Args,
		Meta:                s.Meta,
		Priority:            s.Priority,
		Attempt:             s.Attempt,
		MaxAttempts:         s.MaxAttempts,
		TimeoutMs:           s.TimeoutMs,
		CreatedAt:           s.CreatedAt,
		EnqueuedAt:          s.EnqueuedAt,
		StartedAt:           s.StartedAt,
		CompletedAt:         s.CompletedAt,
		CancelledAt:         s.CancelledAt,
		ScheduledAt:         s.ScheduledAt,
		Result:              s.Result,
		Error:               s.Error,
		Errors:              s.Errors,
		Tags:                s.Tags,
		Retry:               s.Retry,
		Unique:              s.Unique,
		ExpiresAt:           s.ExpiresAt,
		RetryDelayMs:        s.RetryDelayMs,
		ParentResults:       s.ParentResults,
		VisibilityTimeoutMs: s.VisibilityTimeoutMs,
		WorkflowID:          s.WorkflowID,
		WorkflowStep:        s.WorkflowStep,
		UnknownFields:       s.UnknownFields,
	}
	return job
}

// marshalJobState serializes job state to JSON for KV storage.
func marshalJobState(job *core.Job) ([]byte, error) {
	return json.Marshal(jobToState(job))
}

// unmarshalJobState deserializes job state from KV JSON.
func unmarshalJobState(data []byte) (*core.Job, error) {
	var s jobState
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, err
	}
	return stateToJob(&s), nil
}

// workflowState is the JSON-serializable state stored in KV for workflows.
type workflowState struct {
	ID          string                     `json:"id"`
	Name        string                     `json:"name,omitempty"`
	Type        string                     `json:"type"`
	State       string                     `json:"state"`
	Total       int                        `json:"total"`
	Completed   int                        `json:"completed"`
	Failed      int                        `json:"failed"`
	CreatedAt   string                     `json:"created_at"`
	CompletedAt string                     `json:"completed_at,omitempty"`
	Callbacks   *core.WorkflowCallbacks    `json:"callbacks,omitempty"`
	JobDefs     []core.WorkflowJobRequest  `json:"job_defs,omitempty"`
	JobIDs      []string                   `json:"job_ids,omitempty"`
	Results     map[string]json.RawMessage `json:"results,omitempty"`
}

// activeJobInfo tracks an active job's visibility deadline.
type activeJobInfo struct {
	Queue              string `json:"queue"`
	VisibilityDeadline string `json:"visibility_deadline"`
	WorkerID           string `json:"worker_id,omitempty"`
}

// queueMeta stores per-queue metadata.
type queueMeta struct {
	Name            string `json:"name"`
	Paused          bool   `json:"paused"`
	RateLimitPerSec int    `json:"rate_limit_per_sec,omitempty"`
	LastFetchMs     int64  `json:"last_fetch_ms,omitempty"`
	CompletedCount  int    `json:"completed_count"`
}

// kvStatsKey returns the KV key for queue stats.
func kvStatsKey(queue, stat string) string {
	return queue + "." + stat
}

// intToStr converts int to string.
func intToStr(n int) string {
	return strconv.Itoa(n)
}
