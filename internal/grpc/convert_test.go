package grpc

import (
	"encoding/json"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/openjobspec/ojs-backend-nats/internal/core"
	ojsv1 "github.com/openjobspec/ojs-proto/gen/go/ojs/v1"
)

func TestJobToProto_Full(t *testing.T) {
	priority := 7
	job := &core.Job{
		ID:        "job-1",
		Type:      "email.send",
		Queue:     "default",
		State:     core.StateActive,
		Attempt:   2,
		Priority:  &priority,
		Args:      json.RawMessage(`["a", 1]`),
		Meta:      json.RawMessage(`{"k":"v"}`),
		Result:    json.RawMessage(`{"ok":true}`),
		CreatedAt: "2026-08-11T10:00:00.000Z",
		Retry: &core.RetryPolicy{
			MaxAttempts:        5,
			BackoffCoefficient: 2.0,
			Jitter:             true,
			InitialInterval:    "1s",
			MaxInterval:        "30s",
		},
		Unique: &core.UniquePolicy{
			Keys:   []string{"type", "args"},
			Period: "1h",
		},
	}

	pj := jobToProto(job)
	if pj.Id != "job-1" {
		t.Errorf("Id = %q, want job-1", pj.Id)
	}
	if pj.State != ojsv1.JobState_JOB_STATE_ACTIVE {
		t.Errorf("State = %v, want ACTIVE", pj.State)
	}
	if pj.Priority != 7 {
		t.Errorf("Priority = %d, want 7", pj.Priority)
	}
	if len(pj.Args) != 2 {
		t.Errorf("Args len = %d, want 2", len(pj.Args))
	}
	if pj.Meta == nil || pj.Meta.Fields["k"].GetStringValue() != "v" {
		t.Error("Meta not converted")
	}
	if pj.Result == nil {
		t.Error("Result not converted")
	}
	if pj.RetryPolicy == nil || pj.RetryPolicy.MaxAttempts != 5 {
		t.Error("RetryPolicy not converted")
	}
	if pj.RetryPolicy.InitialInterval == nil {
		t.Error("RetryPolicy.InitialInterval not set")
	}
	if pj.UniquePolicy == nil || len(pj.UniquePolicy.Key) != 2 {
		t.Error("UniquePolicy not converted")
	}
	if pj.CreatedAt == nil {
		t.Error("CreatedAt not converted")
	}
}

func TestJobToProto_Nil(t *testing.T) {
	if jobToProto(nil) != nil {
		t.Error("jobToProto(nil) should be nil")
	}
}

func TestStateToProto_AllStates(t *testing.T) {
	cases := map[string]ojsv1.JobState{
		core.StateScheduled: ojsv1.JobState_JOB_STATE_SCHEDULED,
		core.StateAvailable: ojsv1.JobState_JOB_STATE_AVAILABLE,
		core.StatePending:   ojsv1.JobState_JOB_STATE_PENDING,
		core.StateActive:    ojsv1.JobState_JOB_STATE_ACTIVE,
		core.StateCompleted: ojsv1.JobState_JOB_STATE_COMPLETED,
		core.StateRetryable: ojsv1.JobState_JOB_STATE_RETRYABLE,
		core.StateCancelled: ojsv1.JobState_JOB_STATE_CANCELLED,
		core.StateDiscarded: ojsv1.JobState_JOB_STATE_DISCARDED,
	}
	for state, want := range cases {
		if got := stateToProto[state]; got != want {
			t.Errorf("stateToProto[%q] = %v, want %v", state, got, want)
		}
	}
}

func TestEnqueueRequestToJob(t *testing.T) {
	req := &ojsv1.EnqueueRequest{
		Type: "email.send",
		Args: []*structpb.Value{structpb.NewStringValue("user@example.com")},
		Options: &ojsv1.EnqueueOptions{
			Queue:    "mail",
			Priority: 9,
			Retry:    &ojsv1.RetryPolicy{MaxAttempts: 4, InitialInterval: durationpb.New(0)},
			Unique:   &ojsv1.UniquePolicy{Key: []string{"type"}},
		},
	}

	job, err := enqueueRequestToJob(req)
	if err != nil {
		t.Fatalf("enqueueRequestToJob() error = %v", err)
	}
	if job.Type != "email.send" {
		t.Errorf("Type = %q, want email.send", job.Type)
	}
	if job.Queue != "mail" {
		t.Errorf("Queue = %q, want mail", job.Queue)
	}
	if job.Priority == nil || *job.Priority != 9 {
		t.Errorf("Priority = %v, want 9", job.Priority)
	}
	if job.Retry == nil || job.Retry.MaxAttempts != 4 {
		t.Error("Retry not converted")
	}
	if job.Unique == nil || len(job.Unique.Keys) != 1 {
		t.Error("Unique not converted")
	}
	if len(job.Args) == 0 {
		t.Error("Args not marshaled")
	}
}

func TestProtoRetryAndUniqueToCore(t *testing.T) {
	r, err := protoRetryToCore(&ojsv1.RetryPolicy{
		MaxAttempts:        3,
		BackoffCoefficient: 1.5,
		Jitter:             true,
		InitialInterval:    durationpb.New(0),
		MaxInterval:        durationpb.New(0),
	})
	if err != nil {
		t.Fatalf("protoRetryToCore() error = %v", err)
	}
	if r.MaxAttempts != 3 || r.BackoffCoefficient != 1.5 {
		t.Errorf("protoRetryToCore = %+v", r)
	}

	u, err := protoUniqueToCore(&ojsv1.UniquePolicy{Key: []string{"type", "args"}, Period: durationpb.New(time.Second)})
	if err != nil {
		t.Fatalf("protoUniqueToCore() error = %v", err)
	}
	if len(u.Keys) != 2 {
		t.Errorf("protoUniqueToCore keys = %v", u.Keys)
	}
}

func TestWorkflowToProto(t *testing.T) {
	if workflowToProto(nil) != nil {
		t.Error("workflowToProto(nil) should be nil")
	}
	pw := workflowToProto(&core.Workflow{
		ID:          "wf-1",
		Name:        "pipeline",
		State:       "running",
		CreatedAt:   "2026-08-11T10:00:00.000Z",
		CompletedAt: "2026-08-11T11:00:00.000Z",
	})
	if pw.Id != "wf-1" {
		t.Errorf("Id = %q, want wf-1", pw.Id)
	}
	if pw.State != ojsv1.WorkflowState_WORKFLOW_STATE_RUNNING {
		t.Errorf("State = %v, want RUNNING", pw.State)
	}
}

func TestDirectiveToWorkerState(t *testing.T) {
	cases := map[string]ojsv1.WorkerState{
		"quiet":     ojsv1.WorkerState_WORKER_STATE_QUIET,
		"terminate": ojsv1.WorkerState_WORKER_STATE_TERMINATE,
		"continue":  ojsv1.WorkerState_WORKER_STATE_RUNNING,
		"":          ojsv1.WorkerState_WORKER_STATE_RUNNING,
	}
	for directive, want := range cases {
		if got := directiveToWorkerState(directive); got != want {
			t.Errorf("directiveToWorkerState(%q) = %v, want %v", directive, got, want)
		}
	}
}

func TestEnqueueProjection_FullOptions(t *testing.T) {
	now := time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)
	delay := now.Add(time.Hour)
	meta, err := structpb.NewStruct(map[string]any{"tenant": "acme"})
	if err != nil {
		t.Fatal(err)
	}
	job, err := enqueueFieldsToJob(
		"email.send",
		[]*structpb.Value{structpb.NewStringValue("user@example.com")},
		&ojsv1.EnqueueOptions{
			Queue:             "mail",
			Priority:          8,
			DelayUntil:        timestamppb.New(delay),
			Timeout:           durationpb.New(45 * time.Second),
			VisibilityTimeout: durationpb.New(15 * time.Second),
			Ttl:               durationpb.New(2 * time.Hour),
			Tags:              []string{"urgent", "customer"},
			TraceId:           "trace-123",
			Meta:              meta,
			MaxAttempts:       7,
			Retry: &ojsv1.RetryPolicy{
				MaxAttempts:        5,
				InitialInterval:    durationpb.New(time.Second),
				BackoffCoefficient: 2,
				MaxInterval:        durationpb.New(time.Minute),
				Jitter:             true,
				NonRetryableErrors: []string{"fatal.*"},
				OnExhaustion:       "dead_letter",
			},
			Unique: &ojsv1.UniquePolicy{
				Key:        []string{"type", "args"},
				Period:     durationpb.New(time.Hour),
				OnConflict: ojsv1.UniqueConflictAction_UNIQUE_CONFLICT_ACTION_IGNORE,
				States:     []ojsv1.JobState{ojsv1.JobState_JOB_STATE_AVAILABLE, ojsv1.JobState_JOB_STATE_ACTIVE},
			},
		},
		now,
	)
	if err != nil {
		t.Fatalf("enqueueFieldsToJob() error = %v", err)
	}
	if job.Queue != "mail" || job.Priority == nil || *job.Priority != 8 {
		t.Fatalf("queue/priority projection = %q/%v", job.Queue, job.Priority)
	}
	if job.TimeoutMs == nil || *job.TimeoutMs != 45000 {
		t.Fatalf("timeout_ms = %v, want 45000", job.TimeoutMs)
	}
	if job.VisibilityTimeoutMs == nil || *job.VisibilityTimeoutMs != 15000 {
		t.Fatalf("visibility_timeout_ms = %v, want 15000", job.VisibilityTimeoutMs)
	}
	if job.ScheduledAt != core.FormatTime(delay) || job.ExpiresAt != core.FormatTime(now.Add(2*time.Hour)) {
		t.Fatalf("schedule/expiry = %q/%q", job.ScheduledAt, job.ExpiresAt)
	}
	if job.MaxAttempts == nil || *job.MaxAttempts != 5 {
		t.Fatalf("max_attempts = %v, want retry override 5", job.MaxAttempts)
	}
	if job.Retry == nil || job.Retry.OnExhaustion != "dead_letter" || len(job.Retry.NonRetryableErrors) != 1 {
		t.Fatalf("retry projection = %+v", job.Retry)
	}
	if job.Unique == nil || job.Unique.OnConflict != "ignore" || len(job.Unique.States) != 2 {
		t.Fatalf("unique projection = %+v", job.Unique)
	}
	var gotMeta map[string]any
	if err := json.Unmarshal(job.Meta, &gotMeta); err != nil {
		t.Fatalf("metadata unmarshal = %v", err)
	}
	if gotMeta["tenant"] != "acme" || gotMeta["trace_id"] != "trace-123" {
		t.Fatalf("metadata = %#v", gotMeta)
	}
}

func TestEnqueueProjection_DefaultQueueAndBatchDefaults(t *testing.T) {
	job, err := enqueueFieldsToJob("job.run", nil, nil, time.Now())
	if err != nil {
		t.Fatalf("enqueueFieldsToJob() error = %v", err)
	}
	if job.Queue != "default" || string(job.Args) != "[]" {
		t.Fatalf("default projection queue=%q args=%s", job.Queue, job.Args)
	}

	defaults := &ojsv1.EnqueueOptions{
		Queue:       "bulk",
		MaxAttempts: 4,
		Timeout:     durationpb.New(time.Minute),
		Tags:        []string{"default-tag"},
	}
	entry := &ojsv1.BatchJobEntry{
		Type:    "job.run",
		Options: &ojsv1.EnqueueOptions{Queue: "override", Priority: 3},
	}
	batchJob, err := enqueueJobRequestToJob(entry, defaults, time.Now())
	if err != nil {
		t.Fatalf("enqueueJobRequestToJob() error = %v", err)
	}
	if batchJob.Queue != "override" || batchJob.Priority == nil || *batchJob.Priority != 3 {
		t.Fatalf("batch override projection = %+v", batchJob)
	}
	if batchJob.TimeoutMs == nil || *batchJob.TimeoutMs != 60000 || batchJob.MaxAttempts == nil || *batchJob.MaxAttempts != 4 {
		t.Fatalf("batch defaults not preserved: %+v", batchJob)
	}
	if len(batchJob.Tags) != 1 || batchJob.Tags[0] != "default-tag" {
		t.Fatalf("batch tags = %v", batchJob.Tags)
	}
}

func TestEnqueueProjection_RejectsUnsupportedUniqueFields(t *testing.T) {
	_, err := enqueueFieldsToJob("job.run", nil, &ojsv1.EnqueueOptions{
		Unique: &ojsv1.UniquePolicy{
			Key:      []string{"args"},
			ArgsKeys: []string{"customer_id"},
		},
	}, time.Now())
	if err == nil {
		t.Fatal("unsupported args_keys accepted")
	}
	if ojsErr, ok := err.(*core.OJSError); !ok || ojsErr.Code != core.ErrCodeUnsupported {
		t.Fatalf("error = %T %v, want unsupported OJSError", err, err)
	}
}

func TestProtoWorkflowRequest_GroupAndLinearChain(t *testing.T) {
	group, err := protoToWorkflowRequest(&ojsv1.CreateWorkflowRequest{
		Name: "parallel",
		Steps: []*ojsv1.WorkflowStep{
			{Id: "a", Type: "step.a", Options: &ojsv1.EnqueueOptions{Queue: "q-a", Priority: 1}},
			{Id: "b", Type: "step.b", Options: &ojsv1.EnqueueOptions{Queue: "q-b", MaxAttempts: 4}},
		},
	})
	if err != nil {
		t.Fatalf("group conversion error = %v", err)
	}
	if group.Type != "group" || len(group.Jobs) != 2 || group.Jobs[0].Options.Queue != "q-a" {
		t.Fatalf("group conversion = %+v", group)
	}

	chain, err := protoToWorkflowRequest(&ojsv1.CreateWorkflowRequest{
		Name: "linear",
		Steps: []*ojsv1.WorkflowStep{
			{Id: "third", Type: "step.three", DependsOn: []string{"second"}},
			{Id: "first", Type: "step.one"},
			{Id: "second", Type: "step.two", DependsOn: []string{"first"}, Options: &ojsv1.EnqueueOptions{Queue: "linear"}},
		},
	})
	if err != nil {
		t.Fatalf("chain conversion error = %v", err)
	}
	if chain.Type != "chain" || len(chain.Steps) != 3 {
		t.Fatalf("chain conversion = %+v", chain)
	}
	if chain.Steps[0].Name != "first" || chain.Steps[1].Name != "second" || chain.Steps[2].Name != "third" {
		t.Fatalf("chain order = %q, %q, %q", chain.Steps[0].Name, chain.Steps[1].Name, chain.Steps[2].Name)
	}
	if chain.Steps[1].Options.Queue != "linear" {
		t.Fatalf("chain options were dropped: %+v", chain.Steps[1].Options)
	}
}

func TestProtoWorkflowRequest_RejectsInvalidDAGs(t *testing.T) {
	tests := []struct {
		name  string
		steps []*ojsv1.WorkflowStep
	}{
		{"zero jobs", nil},
		{"unknown dependency", []*ojsv1.WorkflowStep{{Id: "a", Type: "step.a", DependsOn: []string{"missing"}}}},
		{"cycle", []*ojsv1.WorkflowStep{
			{Id: "a", Type: "step.a", DependsOn: []string{"b"}},
			{Id: "b", Type: "step.b", DependsOn: []string{"a"}},
		}},
		{"branching", []*ojsv1.WorkflowStep{
			{Id: "a", Type: "step.a"},
			{Id: "b", Type: "step.b", DependsOn: []string{"a"}},
			{Id: "c", Type: "step.c", DependsOn: []string{"a"}},
		}},
		{"fan-in", []*ojsv1.WorkflowStep{
			{Id: "a", Type: "step.a"},
			{Id: "b", Type: "step.b"},
			{Id: "c", Type: "step.c", DependsOn: []string{"a", "b"}},
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := protoToWorkflowRequest(&ojsv1.CreateWorkflowRequest{Steps: tt.steps}); err == nil {
				t.Fatal("protoToWorkflowRequest() error = nil")
			}
		})
	}
}
