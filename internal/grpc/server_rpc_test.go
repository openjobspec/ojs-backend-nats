package grpc

import (
	"context"
	"os"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/openjobspec/ojs-backend-nats/internal/core"
	natsbackend "github.com/openjobspec/ojs-backend-nats/internal/nats"
	ojsv1 "github.com/openjobspec/ojs-proto/gen/go/ojs/v1"
)

func newSuffix() string { return core.NewUUIDv7() }

func newRPCServer(t *testing.T) *Server {
	t.Helper()
	natsURL := os.Getenv("NATS_URL")
	if natsURL == "" {
		natsURL = "nats://localhost:4222"
	}
	backend, err := natsbackend.New(natsURL)
	if err != nil {
		t.Skipf("skipping gRPC RPC test; NATS unavailable at %s: %v", natsURL, err)
	}
	t.Cleanup(func() { _ = backend.Close() })
	return New(backend)
}

func mustValues(t *testing.T, args ...any) []*structpb.Value {
	t.Helper()
	vals := make([]*structpb.Value, 0, len(args))
	for _, a := range args {
		v, err := structpb.NewValue(a)
		if err != nil {
			t.Fatalf("structpb.NewValue(%v): %v", a, err)
		}
		vals = append(vals, v)
	}
	return vals
}

func TestGRPC_ManifestAndHealth(t *testing.T) {
	s := newRPCServer(t)
	ctx := context.Background()

	man, err := s.Manifest(ctx, &ojsv1.ManifestRequest{})
	if err != nil {
		t.Fatalf("Manifest() error = %v", err)
	}
	if man.Backend != "nats" {
		t.Errorf("Manifest backend = %q, want nats", man.Backend)
	}

	h, err := s.Health(ctx, &ojsv1.HealthRequest{})
	if err != nil {
		t.Fatalf("Health() error = %v", err)
	}
	if h.Status != ojsv1.HealthStatus_HEALTH_STATUS_OK {
		t.Errorf("Health status = %v, want OK", h.Status)
	}
}

func TestGRPC_EnqueueGetCancel(t *testing.T) {
	s := newRPCServer(t)
	ctx := context.Background()
	queue := "grpc-it-" + newSuffix()

	enq, err := s.Enqueue(ctx, &ojsv1.EnqueueRequest{
		Type:    "email.send",
		Args:    mustValues(t, "user@example.com"),
		Options: &ojsv1.EnqueueOptions{Queue: queue, Priority: 5},
	})
	if err != nil {
		t.Fatalf("Enqueue() error = %v", err)
	}
	if enq.Job == nil || enq.Job.Id == "" {
		t.Fatal("Enqueue() returned no job id")
	}
	id := enq.Job.Id

	got, err := s.GetJob(ctx, &ojsv1.GetJobRequest{JobId: id})
	if err != nil {
		t.Fatalf("GetJob() error = %v", err)
	}
	if got.Job.Id != id {
		t.Errorf("GetJob id = %q, want %q", got.Job.Id, id)
	}
	if got.Job.Queue != queue {
		t.Errorf("GetJob queue = %q, want %q", got.Job.Queue, queue)
	}

	can, err := s.CancelJob(ctx, &ojsv1.CancelJobRequest{JobId: id})
	if err != nil {
		t.Fatalf("CancelJob() error = %v", err)
	}
	if can.Job.State != ojsv1.JobState_JOB_STATE_CANCELLED {
		t.Errorf("CancelJob state = %v, want CANCELLED", can.Job.State)
	}
}

func TestGRPC_GetJobNotFound(t *testing.T) {
	s := newRPCServer(t)
	_, err := s.GetJob(context.Background(), &ojsv1.GetJobRequest{JobId: "does-not-exist"})
	if err == nil {
		t.Fatal("GetJob(missing) = nil error, want NotFound")
	}
}

func TestGRPC_EnqueueBatchFetchAck(t *testing.T) {
	s := newRPCServer(t)
	ctx := context.Background()
	queue := "grpc-it-batch-" + newSuffix()

	batch, err := s.EnqueueBatch(ctx, &ojsv1.EnqueueBatchRequest{
		Jobs: []*ojsv1.BatchJobEntry{
			{Type: "t1", Args: mustValues(t, "a"), Options: &ojsv1.EnqueueOptions{Queue: queue}},
			{Type: "t2", Args: mustValues(t, "b"), Options: &ojsv1.EnqueueOptions{Queue: queue}},
		},
	})
	if err != nil {
		t.Fatalf("EnqueueBatch() error = %v", err)
	}
	if len(batch.Jobs) != 2 {
		t.Fatalf("EnqueueBatch returned %d jobs, want 2", len(batch.Jobs))
	}

	fetch, err := s.Fetch(ctx, &ojsv1.FetchRequest{Queues: []string{queue}, Count: 2, WorkerId: "w1"})
	if err != nil {
		t.Fatalf("Fetch() error = %v", err)
	}
	if len(fetch.Jobs) == 0 {
		t.Fatal("Fetch returned no jobs")
	}

	ack, err := s.Ack(ctx, &ojsv1.AckRequest{JobId: fetch.Jobs[0].Id})
	if err != nil {
		t.Fatalf("Ack() error = %v", err)
	}
	if !ack.Acknowledged {
		t.Error("Ack Acknowledged = false, want true")
	}
}

func TestGRPC_NackRetryable(t *testing.T) {
	s := newRPCServer(t)
	ctx := context.Background()
	queue := "grpc-it-nack-" + newSuffix()

	if _, err := s.Enqueue(ctx, &ojsv1.EnqueueRequest{
		Type:    "t",
		Options: &ojsv1.EnqueueOptions{Queue: queue, MaxAttempts: 5},
	}); err != nil {
		t.Fatalf("Enqueue() error = %v", err)
	}

	fetch, err := s.Fetch(ctx, &ojsv1.FetchRequest{Queues: []string{queue}, Count: 1, WorkerId: "w1"})
	if err != nil || len(fetch.Jobs) == 0 {
		t.Fatalf("Fetch() error = %v jobs=%d", err, len(fetch.Jobs))
	}

	nack, err := s.Nack(ctx, &ojsv1.NackRequest{
		JobId: fetch.Jobs[0].Id,
		Error: &ojsv1.JobError{Message: "temporary failure", Code: "io_error"},
	})
	if err != nil {
		t.Fatalf("Nack() error = %v", err)
	}
	if nack.State != ojsv1.JobState_JOB_STATE_RETRYABLE {
		t.Errorf("Nack state = %v, want RETRYABLE", nack.State)
	}
	if nack.NextAttemptAt == nil {
		t.Error("Nack next_attempt_at = nil, want retry deadline")
	}
}

func TestGRPC_QueueOps(t *testing.T) {
	s := newRPCServer(t)
	ctx := context.Background()
	queue := "grpc-it-queue-" + newSuffix()

	if _, err := s.Enqueue(ctx, &ojsv1.EnqueueRequest{Type: "t", Options: &ojsv1.EnqueueOptions{Queue: queue}}); err != nil {
		t.Fatalf("Enqueue() error = %v", err)
	}

	if _, err := s.PauseQueue(ctx, &ojsv1.PauseQueueRequest{Queue: queue}); err != nil {
		t.Fatalf("PauseQueue() error = %v", err)
	}
	if _, err := s.ResumeQueue(ctx, &ojsv1.ResumeQueueRequest{Queue: queue}); err != nil {
		t.Fatalf("ResumeQueue() error = %v", err)
	}

	stats, err := s.QueueStats(ctx, &ojsv1.QueueStatsRequest{Queue: queue})
	if err != nil {
		t.Fatalf("QueueStats() error = %v", err)
	}
	if stats.Queue != queue {
		t.Errorf("QueueStats queue = %q, want %q", stats.Queue, queue)
	}

	list, err := s.ListQueues(ctx, &ojsv1.ListQueuesRequest{})
	if err != nil {
		t.Fatalf("ListQueues() error = %v", err)
	}
	found := false
	for _, q := range list.Queues {
		if q.Name == queue {
			found = true
		}
	}
	if !found {
		t.Errorf("ListQueues did not include %q", queue)
	}
}

func TestGRPC_CronLifecycle(t *testing.T) {
	s := newRPCServer(t)
	ctx := context.Background()
	name := "grpc-cron-" + newSuffix()

	reg, err := s.RegisterCron(ctx, &ojsv1.RegisterCronRequest{
		Name: name,
		Cron: "*/5 * * * *",
		Type: "report.generate",
		Args: mustValues(t, "arg1"),
	})
	if err != nil {
		t.Fatalf("RegisterCron() error = %v", err)
	}
	if reg.Name != name {
		t.Errorf("RegisterCron name = %q, want %q", reg.Name, name)
	}

	listed, err := s.ListCron(ctx, &ojsv1.ListCronRequest{})
	if err != nil {
		t.Fatalf("ListCron() error = %v", err)
	}
	found := false
	for _, e := range listed.Entries {
		if e.Name == name {
			found = true
		}
	}
	if !found {
		t.Errorf("ListCron did not include %q", name)
	}

	if _, err := s.UnregisterCron(ctx, &ojsv1.UnregisterCronRequest{Name: name}); err != nil {
		t.Fatalf("UnregisterCron() error = %v", err)
	}
}

func TestGRPC_WorkflowLifecycle(t *testing.T) {
	s := newRPCServer(t)
	ctx := context.Background()
	queue := "grpc-wf-" + newSuffix()

	create, err := s.CreateWorkflow(ctx, &ojsv1.CreateWorkflowRequest{
		Name: "wf",
		Steps: []*ojsv1.WorkflowStep{
			{Id: "s1", Type: "step.one", Options: &ojsv1.EnqueueOptions{Queue: queue}},
			{Id: "s2", Type: "step.two", Options: &ojsv1.EnqueueOptions{Queue: queue}},
		},
	})
	if err != nil {
		t.Fatalf("CreateWorkflow() error = %v", err)
	}
	if create.Workflow == nil || create.Workflow.Id == "" {
		t.Fatal("CreateWorkflow returned no workflow id")
	}
	id := create.Workflow.Id

	got, err := s.GetWorkflow(ctx, &ojsv1.GetWorkflowRequest{WorkflowId: id})
	if err != nil {
		t.Fatalf("GetWorkflow() error = %v", err)
	}
	if got.Workflow.Id != id {
		t.Errorf("GetWorkflow id = %q, want %q", got.Workflow.Id, id)
	}

	can, err := s.CancelWorkflow(ctx, &ojsv1.CancelWorkflowRequest{WorkflowId: id})
	if err != nil {
		t.Fatalf("CancelWorkflow() error = %v", err)
	}
	if can.Workflow.State != ojsv1.WorkflowState_WORKFLOW_STATE_CANCELLED {
		t.Errorf("CancelWorkflow state = %v, want CANCELLED", can.Workflow.State)
	}
}

func TestGRPC_WorkflowLinearDependenciesExecuteAsChain(t *testing.T) {
	s := newRPCServer(t)
	ctx := context.Background()
	queue := "grpc-wf-chain-" + newSuffix()
	created, err := s.CreateWorkflow(ctx, &ojsv1.CreateWorkflowRequest{
		Name: "linear",
		Steps: []*ojsv1.WorkflowStep{
			{Id: "second", Type: "step.second", DependsOn: []string{"first"}, Options: &ojsv1.EnqueueOptions{Queue: queue, Priority: 7}},
			{Id: "first", Type: "step.first", Options: &ojsv1.EnqueueOptions{Queue: queue, Timeout: durationpb.New(time.Minute)}},
		},
	})
	if err != nil {
		t.Fatalf("CreateWorkflow() error = %v", err)
	}
	if created.Workflow == nil {
		t.Fatal("CreateWorkflow() returned no workflow")
	}

	first, err := s.Fetch(ctx, &ojsv1.FetchRequest{Queues: []string{queue}, Count: 1, WorkerId: "wf-worker"})
	if err != nil || len(first.Jobs) != 1 {
		t.Fatalf("first Fetch() error=%v jobs=%d", err, len(first.Jobs))
	}
	if first.Jobs[0].Type != "step.first" || first.Jobs[0].Timeout == nil ||
		first.Jobs[0].Timeout.AsDuration() != time.Minute {
		t.Fatalf("first workflow job/options = %+v", first.Jobs[0])
	}
	if _, err := s.Ack(ctx, &ojsv1.AckRequest{JobId: first.Jobs[0].Id}); err != nil {
		t.Fatalf("first Ack() error = %v", err)
	}

	var second *ojsv1.Job
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		fetched, fetchErr := s.Fetch(ctx, &ojsv1.FetchRequest{Queues: []string{queue}, Count: 1, WorkerId: "wf-worker"})
		if fetchErr != nil {
			t.Fatalf("second Fetch() error = %v", fetchErr)
		}
		if len(fetched.Jobs) == 1 {
			second = fetched.Jobs[0]
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if second == nil || second.Type != "step.second" || second.Priority != 7 {
		t.Fatalf("second workflow job/options = %+v", second)
	}
}

func TestGRPC_ListDeadLetter(t *testing.T) {
	s := newRPCServer(t)
	if _, err := s.ListDeadLetter(context.Background(), &ojsv1.ListDeadLetterRequest{Limit: 10}); err != nil {
		t.Fatalf("ListDeadLetter() error = %v", err)
	}
}

func TestGRPC_EnqueueFullOptionsRoundTrip(t *testing.T) {
	s := newRPCServer(t)
	ctx := context.Background()
	queue := "grpc-options-" + newSuffix()
	scheduledAt := time.Now().Add(10 * time.Minute).UTC()
	meta, err := structpb.NewStruct(map[string]any{"tenant": "acme"})
	if err != nil {
		t.Fatal(err)
	}

	resp, err := s.Enqueue(ctx, &ojsv1.EnqueueRequest{
		Type: "email.send",
		Args: mustValues(t, "user@example.com"),
		Options: &ojsv1.EnqueueOptions{
			Queue:             queue,
			Priority:          9,
			DelayUntil:        timestamppb.New(scheduledAt),
			Timeout:           durationpb.New(45 * time.Second),
			VisibilityTimeout: durationpb.New(20 * time.Second),
			Ttl:               durationpb.New(time.Hour),
			Tags:              []string{"urgent"},
			TraceId:           "trace-123",
			Meta:              meta,
			MaxAttempts:       6,
			Retry: &ojsv1.RetryPolicy{
				MaxAttempts:        4,
				InitialInterval:    durationpb.New(time.Second),
				BackoffCoefficient: 2,
				MaxInterval:        durationpb.New(time.Minute),
				NonRetryableErrors: []string{"fatal"},
				OnExhaustion:       "dead_letter",
			},
			Unique: &ojsv1.UniquePolicy{
				Key:        []string{"type", "queue", "args"},
				Period:     durationpb.New(time.Hour),
				OnConflict: ojsv1.UniqueConflictAction_UNIQUE_CONFLICT_ACTION_IGNORE,
				States:     []ojsv1.JobState{ojsv1.JobState_JOB_STATE_SCHEDULED},
			},
		},
	})
	if err != nil {
		t.Fatalf("Enqueue() error = %v", err)
	}
	job := resp.Job
	if job.Queue != queue || job.Priority != 9 || job.State != ojsv1.JobState_JOB_STATE_SCHEDULED {
		t.Fatalf(
			"job queue/priority/state = %q/%d/%v; expected %q/9/%v; equal=%t/%t/%t",
			job.Queue,
			job.Priority,
			job.State,
			queue,
			ojsv1.JobState_JOB_STATE_SCHEDULED,
			job.Queue == queue,
			job.Priority == 9,
			job.State == ojsv1.JobState_JOB_STATE_SCHEDULED,
		)
	}
	if job.MaxAttempts != 4 || job.RetryPolicy == nil || job.RetryPolicy.OnExhaustion != "dead_letter" {
		t.Fatalf("retry/max attempts round trip = %d/%+v", job.MaxAttempts, job.RetryPolicy)
	}
	if job.Timeout == nil || job.Timeout.AsDuration() != 45*time.Second {
		t.Fatalf("timeout = %v", job.Timeout)
	}
	if job.VisibilityTimeout == nil || job.VisibilityTimeout.AsDuration() != 20*time.Second {
		t.Fatalf("visibility timeout = %v", job.VisibilityTimeout)
	}
	if job.UniquePolicy == nil || job.UniquePolicy.OnConflict != ojsv1.UniqueConflictAction_UNIQUE_CONFLICT_ACTION_IGNORE {
		t.Fatalf("unique policy = %+v", job.UniquePolicy)
	}
	if job.Meta == nil || job.Meta.Fields["tenant"].GetStringValue() != "acme" ||
		job.Meta.Fields["trace_id"].GetStringValue() != "trace-123" {
		t.Fatalf("metadata = %+v", job.Meta)
	}
	if job.TraceId != "trace-123" {
		t.Fatalf("trace_id = %q, want trace-123", job.TraceId)
	}
	if job.ExpiresAt == nil || len(job.Tags) != 1 {
		t.Fatalf("expiry/tags missing: expires=%v tags=%v", job.ExpiresAt, job.Tags)
	}
}

func TestGRPC_EnqueueBatchProjectsDefaultsAndOverrides(t *testing.T) {
	s := newRPCServer(t)
	queue := "grpc-batch-options-" + newSuffix()
	resp, err := s.EnqueueBatch(context.Background(), &ojsv1.EnqueueBatchRequest{
		DefaultOptions: &ojsv1.EnqueueOptions{
			Queue:       queue,
			MaxAttempts: 5,
			Timeout:     durationpb.New(30 * time.Second),
			Tags:        []string{"batch"},
		},
		Jobs: []*ojsv1.BatchJobEntry{
			{Type: "batch.one", Args: mustValues(t, 1)},
			{Type: "batch.two", Args: mustValues(t, 2), Options: &ojsv1.EnqueueOptions{Priority: 7}},
		},
	})
	if err != nil {
		t.Fatalf("EnqueueBatch() error = %v", err)
	}
	if resp.Count != 2 || len(resp.Jobs) != 2 {
		t.Fatalf("batch count/jobs = %d/%d", resp.Count, len(resp.Jobs))
	}
	for i, job := range resp.Jobs {
		if job.Queue != queue || job.MaxAttempts != 5 || job.Timeout.AsDuration() != 30*time.Second {
			t.Fatalf("job %d defaults = %+v", i, job)
		}
	}
	if resp.Jobs[1].Priority != 7 {
		t.Fatalf("per-job priority = %d, want 7", resp.Jobs[1].Priority)
	}
}

func TestGRPC_RejectsUnsupportedEnqueueOptions(t *testing.T) {
	s := newRPCServer(t)
	_, err := s.Enqueue(context.Background(), &ojsv1.EnqueueRequest{
		Type: "job.run",
		Options: &ojsv1.EnqueueOptions{
			Unique: &ojsv1.UniquePolicy{
				Key:      []string{"args"},
				ArgsKeys: []string{"customer_id"},
			},
		},
	})
	if status.Code(err) != codes.Unimplemented {
		t.Fatalf("Enqueue() code = %v, want Unimplemented (err=%v)", status.Code(err), err)
	}
}

func TestGRPC_HeartbeatExtendsRequestedJob(t *testing.T) {
	s := newRPCServer(t)
	ctx := context.Background()
	queue := "grpc-heartbeat-" + newSuffix()
	enqueued, err := s.Enqueue(ctx, &ojsv1.EnqueueRequest{Type: "job.run", Options: &ojsv1.EnqueueOptions{Queue: queue}})
	if err != nil {
		t.Fatalf("Enqueue() error = %v", err)
	}
	fetched, err := s.Fetch(ctx, &ojsv1.FetchRequest{Queues: []string{queue}, WorkerId: "worker-heartbeat", Count: 1})
	if err != nil || len(fetched.Jobs) != 1 {
		t.Fatalf("Fetch() error=%v jobs=%d", err, len(fetched.Jobs))
	}

	before := time.Now()
	hb, err := s.Heartbeat(ctx, &ojsv1.HeartbeatRequest{
		Id:       enqueued.Job.Id,
		WorkerId: "worker-heartbeat",
		ExtendBy: durationpb.New(2500 * time.Millisecond),
	})
	if err != nil {
		t.Fatalf("Heartbeat() error = %v", err)
	}
	if hb.NewDeadline == nil {
		t.Fatal("Heartbeat() returned no deadline for extended job")
	}
	deadline := hb.NewDeadline.AsTime()
	if deadline.Before(before.Add(2*time.Second)) || deadline.After(time.Now().Add(4*time.Second)) {
		t.Fatalf("heartbeat deadline %v does not reflect extend_by", deadline)
	}
}

func TestGRPC_HeartbeatRejectsInvalidDuration(t *testing.T) {
	s := newRPCServer(t)
	_, err := s.Heartbeat(context.Background(), &ojsv1.HeartbeatRequest{
		Id:       "job",
		WorkerId: "worker",
		ExtendBy: durationpb.New(-time.Second),
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("Heartbeat() code = %v, want InvalidArgument", status.Code(err))
	}
}

func TestGRPC_WorkflowRejectsUnsupportedDAGs(t *testing.T) {
	s := newRPCServer(t)
	tests := []*ojsv1.CreateWorkflowRequest{
		{},
		{Steps: []*ojsv1.WorkflowStep{{Id: "a", Type: "step.a", DependsOn: []string{"missing"}}}},
		{Steps: []*ojsv1.WorkflowStep{
			{Id: "a", Type: "step.a", DependsOn: []string{"b"}},
			{Id: "b", Type: "step.b", DependsOn: []string{"a"}},
		}},
		{Steps: []*ojsv1.WorkflowStep{
			{Id: "a", Type: "step.a"},
			{Id: "b", Type: "step.b", DependsOn: []string{"a"}},
			{Id: "c", Type: "step.c", DependsOn: []string{"a"}},
		}},
	}
	for i, req := range tests {
		if _, err := s.CreateWorkflow(context.Background(), req); status.Code(err) != codes.InvalidArgument {
			t.Errorf("case %d code = %v, want InvalidArgument (err=%v)", i, status.Code(err), err)
		}
	}
}
