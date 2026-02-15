package nats

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/openjobspec/ojs-backend-nats/internal/core"
)

func TestBackendPushFetchAckFlow(t *testing.T) {
	backend := newIntegrationBackend(t)

	ctx := context.Background()
	queue := "it-backend-ack-" + core.NewUUIDv7()
	job := &core.Job{
		Type:  "email.send",
		Queue: queue,
		Args:  json.RawMessage(`["user@example.com"]`),
	}

	created, err := backend.Push(ctx, job)
	if err != nil {
		t.Fatalf("Push() error = %v", err)
	}
	if created.ID == "" {
		t.Fatal("Push() returned empty job ID")
	}

	fetched, err := backend.Fetch(ctx, []string{queue}, 1, "worker-1", 5000)
	if err != nil {
		t.Fatalf("Fetch() error = %v", err)
	}
	if len(fetched) != 1 {
		t.Fatalf("Fetch() returned %d jobs, want 1", len(fetched))
	}
	if fetched[0].ID != created.ID {
		t.Fatalf("Fetch() returned job ID %s, want %s", fetched[0].ID, created.ID)
	}

	if _, err := backend.Ack(ctx, created.ID, json.RawMessage(`{"ok":true}`)); err != nil {
		t.Fatalf("Ack() error = %v", err)
	}

	info, err := backend.Info(ctx, created.ID)
	if err != nil {
		t.Fatalf("Info() error = %v", err)
	}
	if info.State != core.StateCompleted {
		t.Fatalf("Info().State = %q, want %q", info.State, core.StateCompleted)
	}
}

func TestBackendNackRetryFlow(t *testing.T) {
	backend := newIntegrationBackend(t)

	maxAttempts := 5
	retryPolicy := &core.RetryPolicy{
		MaxAttempts:     maxAttempts,
		InitialInterval: "PT0.01S",
		BackoffType:     "constant",
		Jitter:          false,
	}

	ctx := context.Background()
	queue := "it-backend-retry-" + core.NewUUIDv7()
	job := &core.Job{
		Type:        "email.send",
		Queue:       queue,
		Args:        json.RawMessage(`["user@example.com"]`),
		Retry:       retryPolicy,
		MaxAttempts: &maxAttempts,
	}

	created, err := backend.Push(ctx, job)
	if err != nil {
		t.Fatalf("Push() error = %v", err)
	}

	fetched, err := backend.Fetch(ctx, []string{queue}, 1, "worker-1", 5000)
	if err != nil {
		t.Fatalf("Fetch() error = %v", err)
	}
	if len(fetched) != 1 || fetched[0].ID != created.ID {
		t.Fatalf("Fetch() did not return created job")
	}

	retryable := true
	nackResp, err := backend.Nack(ctx, created.ID, &core.JobError{
		Message:   "temporary failure",
		Retryable: &retryable,
	}, false)
	if err != nil {
		t.Fatalf("Nack() error = %v", err)
	}
	if nackResp.State != core.StateRetryable {
		t.Fatalf("Nack().State = %q, want %q", nackResp.State, core.StateRetryable)
	}

	time.Sleep(25 * time.Millisecond)
	if err := backend.PromoteRetries(ctx); err != nil {
		t.Fatalf("PromoteRetries() error = %v", err)
	}

	refetched, err := backend.Fetch(ctx, []string{queue}, 1, "worker-1", 5000)
	if err != nil {
		t.Fatalf("Fetch() after retry promotion error = %v", err)
	}
	if len(refetched) != 1 {
		t.Fatalf("Fetch() after retry promotion returned %d jobs, want 1", len(refetched))
	}
	if refetched[0].ID != created.ID {
		t.Fatalf("Retry fetch returned %s, want %s", refetched[0].ID, created.ID)
	}
}

func TestBackendDeadLetterLifecycle(t *testing.T) {
	backend := newIntegrationBackend(t)
	ctx := context.Background()
	queue := "it-backend-dead-" + core.NewUUIDv7()

	maxAttempts := 1
	created, err := backend.Push(ctx, &core.Job{
		Type:  "email.send",
		Queue: queue,
		Args:  json.RawMessage(`["dead@example.com"]`),
		Retry: &core.RetryPolicy{
			MaxAttempts:  maxAttempts,
			OnExhaustion: "dead_letter",
		},
		MaxAttempts: &maxAttempts,
	})
	if err != nil {
		t.Fatalf("Push() error = %v", err)
	}

	fetched, err := backend.Fetch(ctx, []string{queue}, 1, "worker-dead", 5000)
	if err != nil {
		t.Fatalf("Fetch() error = %v", err)
	}
	if len(fetched) != 1 {
		t.Fatalf("Fetch() returned %d jobs, want 1", len(fetched))
	}

	retryable := false
	nackResp, err := backend.Nack(ctx, created.ID, &core.JobError{
		Message:   "fatal",
		Retryable: &retryable,
	}, false)
	if err != nil {
		t.Fatalf("Nack() error = %v", err)
	}
	if nackResp.State != core.StateDiscarded {
		t.Fatalf("Nack().State = %q, want %q", nackResp.State, core.StateDiscarded)
	}

	deadJobs, total, err := backend.ListDeadLetter(ctx, 50, 0)
	if err != nil {
		t.Fatalf("ListDeadLetter() error = %v", err)
	}
	if total == 0 || !containsJobID(deadJobs, created.ID) {
		t.Fatalf("dead letter list does not include job %s", created.ID)
	}

	retried, err := backend.RetryDeadLetter(ctx, created.ID)
	if err != nil {
		t.Fatalf("RetryDeadLetter() error = %v", err)
	}
	if retried.State != core.StateAvailable {
		t.Fatalf("RetryDeadLetter().State = %q, want %q", retried.State, core.StateAvailable)
	}

	afterRetry, totalAfterRetry, err := backend.ListDeadLetter(ctx, 50, 0)
	if err != nil {
		t.Fatalf("ListDeadLetter() after retry error = %v", err)
	}
	if containsJobID(afterRetry, created.ID) || totalAfterRetry != 0 {
		t.Fatalf("job %s should no longer be in dead letter list", created.ID)
	}

	refetched, err := backend.Fetch(ctx, []string{queue}, 1, "worker-dead", 5000)
	if err != nil {
		t.Fatalf("Fetch() after dead-letter retry error = %v", err)
	}
	if len(refetched) != 1 || refetched[0].ID != created.ID {
		t.Fatalf("Fetch() after retry did not return retried job %s", created.ID)
	}
}

func TestBackendFireCronJobs_EnqueuesDueJob(t *testing.T) {
	backend := newIntegrationBackend(t)
	ctx := context.Background()
	queue := "it-backend-cron-" + core.NewUUIDv7()
	cronName := "cron-" + core.NewUUIDv7()

	registered, err := backend.RegisterCron(ctx, &core.CronJob{
		Name:       cronName,
		Expression: "* * * * *",
		JobTemplate: &core.CronJobTemplate{
			Type: "cron.process",
			Args: json.RawMessage(`["payload"]`),
			Options: &core.EnqueueOptions{
				Queue: queue,
			},
		},
	})
	if err != nil {
		t.Fatalf("RegisterCron() error = %v", err)
	}
	if registered.NextRunAt == "" {
		t.Fatal("RegisterCron() returned empty next_run_at")
	}

	stored, err := backend.cronStore.Get(ctx, cronName)
	if err != nil {
		t.Fatalf("cronStore.Get() error = %v", err)
	}
	stored.NextRunAt = core.FormatTime(time.Now().Add(-2 * time.Second))
	if err := backend.cronStore.Register(ctx, stored); err != nil {
		t.Fatalf("cronStore.Register() error = %v", err)
	}

	if err := backend.FireCronJobs(ctx); err != nil {
		t.Fatalf("FireCronJobs() error = %v", err)
	}

	fetched, err := backend.Fetch(ctx, []string{queue}, 1, "worker-cron", 5000)
	if err != nil {
		t.Fatalf("Fetch() after FireCronJobs error = %v", err)
	}
	if len(fetched) != 1 {
		t.Fatalf("Fetch() after FireCronJobs returned %d jobs, want 1", len(fetched))
	}
	if fetched[0].Type != "cron.process" {
		t.Fatalf("cron job type = %q, want %q", fetched[0].Type, "cron.process")
	}

	updated, err := backend.cronStore.Get(ctx, cronName)
	if err != nil {
		t.Fatalf("cronStore.Get() after fire error = %v", err)
	}
	if updated.LastRunAt == "" {
		t.Fatal("cron LastRunAt was not updated after firing")
	}
}

func containsJobID(jobs []*core.Job, jobID string) bool {
	for _, j := range jobs {
		if j != nil && j.ID == jobID {
			return true
		}
	}
	return false
}

func newIntegrationBackend(t *testing.T) *NATSBackend {
	t.Helper()

	natsURL := os.Getenv("NATS_URL")
	if natsURL == "" {
		natsURL = "nats://localhost:4222"
	}

	backend, err := New(natsURL)
	if err != nil {
		t.Skipf("skipping integration test; NATS unavailable at %s: %v", natsURL, err)
	}

	t.Cleanup(func() {
		_ = backend.Close()
	})

	return backend
}
