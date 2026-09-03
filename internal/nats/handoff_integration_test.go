package nats

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/openjobspec/ojs-backend-nats/internal/core"
)

func TestScheduledPromotion_AmbiguousPublishRecoversAfterRestart(t *testing.T) {
	backend := newIntegrationBackend(t)
	ctx := context.Background()
	queue := "it-handoff-scheduled-" + core.NewUUIDv7()
	created, err := backend.Push(ctx, &core.Job{
		Type:        "scheduled.run",
		Queue:       queue,
		Args:        json.RawMessage(`[]`),
		ScheduledAt: core.FormatTime(time.Now().Add(40 * time.Millisecond)),
	})
	if err != nil {
		t.Fatalf("Push() error = %v", err)
	}
	time.Sleep(60 * time.Millisecond)

	originalPublish := backend.publishJob
	var injected atomic.Bool
	backend.publishJob = func(ctx context.Context, queue, jobID string, seq uint64) error {
		if jobID != created.ID {
			return originalPublish(ctx, queue, jobID, seq)
		}
		if err := originalPublish(ctx, queue, jobID, seq); err != nil {
			return err
		}
		if !injected.Swap(true) {
			return context.DeadlineExceeded
		}
		return nil
	}
	if err := backend.PromoteScheduled(ctx); err == nil {
		t.Fatal("PromoteScheduled() error = nil, want ambiguous publish error")
	}
	if !backend.scheduled.Exists(ctx, created.ID) {
		t.Fatal("scheduled source was deleted before publish confirmation")
	}
	state, err := backend.Info(ctx, created.ID)
	if err != nil || state.State != core.StateAvailable {
		t.Fatalf("state after ambiguous promotion = %+v err=%v", state, err)
	}

	restarted := newIntegrationBackend(t)
	if err := restarted.PromoteScheduled(ctx); err != nil {
		t.Fatalf("PromoteScheduled() after restart error = %v", err)
	}
	if restarted.scheduled.Exists(ctx, created.ID) {
		t.Fatal("scheduled source survived confirmed reconciliation")
	}
	fetched := fetchJobEventually(t, restarted, queue, "scheduled-worker")
	if fetched.ID != created.ID {
		t.Fatalf("fetched ID = %q, want %q", fetched.ID, created.ID)
	}
	if _, err := restarted.Ack(ctx, fetched.ID, nil); err != nil {
		t.Fatalf("Ack() error = %v", err)
	}
}

func TestNackRequeue_RetainsSourceUntilReplacementConfirmed(t *testing.T) {
	backend := newIntegrationBackend(t)
	ctx := context.Background()
	queue := "it-handoff-nack-" + core.NewUUIDv7()
	created, err := backend.Push(ctx, &core.Job{Type: "nack.run", Queue: queue, Args: json.RawMessage(`[]`)})
	if err != nil {
		t.Fatalf("Push() error = %v", err)
	}

	fetched := fetchJobEventually(t, backend, queue, "nack-worker")

	originalPublish := backend.publishJob
	backend.publishJob = func(ctx context.Context, publishQueue, jobID string, seq uint64) error {
		if jobID != created.ID {
			return originalPublish(ctx, publishQueue, jobID, seq)
		}
		return context.DeadlineExceeded
	}
	if _, err := backend.Nack(ctx, fetched.ID, nil, true); err == nil {
		t.Fatal("Nack(requeue) error = nil, want publish failure")
	}
	if !backend.active.Exists(ctx, created.ID) {
		t.Fatal("active source was deleted before replacement publication")
	}
	if !backend.stats.Exists(ctx, handoffKey(created.ID)) {
		t.Fatal("durable handoff marker was not retained")
	}
	if _, ok := backend.consumers.inflight.Load(created.ID); !ok {
		t.Fatal("JetStream source was removed from inflight before replacement publication")
	}

	backend.publishJob = originalPublish
	if err := backend.reconcileHandoffs(ctx); err != nil {
		t.Fatalf("reconcileHandoffs() error = %v", err)
	}
	if backend.active.Exists(ctx, created.ID) || backend.stats.Exists(ctx, handoffKey(created.ID)) {
		t.Fatal("handoff source/marker remained after confirmed replacement")
	}
	refetched := fetchJobEventually(t, backend, queue, "nack-worker-2")
	if refetched.ID != created.ID {
		t.Fatalf("refetched ID = %q, want %q", refetched.ID, created.ID)
	}
}

func TestNackRequeue_DoesNotAckOrDeleteReplacementConsumedDuringHandoff(t *testing.T) {
	backend := newIntegrationBackend(t)
	replacementBackend := newIntegrationBackend(t)
	ctx := context.Background()
	queue := "it-handoff-consumed-" + core.NewUUIDv7()
	created, err := backend.Push(ctx, &core.Job{Type: "nack.consume", Queue: queue, Args: json.RawMessage(`[]`)})
	if err != nil {
		t.Fatalf("Push() error = %v", err)
	}
	original := fetchJobEventually(t, backend, queue, "original-worker")
	originalRecord, err := backend.getJobRecord(ctx, original.ID)
	if err != nil {
		t.Fatalf("getJobRecord() error = %v", err)
	}

	originalPublish := backend.publishJob
	var replacement *core.Job
	backend.publishJob = func(ctx context.Context, publishQueue, jobID string, seq uint64) error {
		if jobID != created.ID {
			return originalPublish(ctx, publishQueue, jobID, seq)
		}
		if err := originalPublish(ctx, publishQueue, jobID, seq); err != nil {
			return err
		}
		jobs, err := replacementBackend.Fetch(ctx, []string{queue}, 1, "replacement-worker", 5000)
		if err != nil {
			return err
		}
		if len(jobs) != 1 {
			return errors.New("replacement was not consumable after confirmed publish")
		}
		replacement = jobs[0]
		return nil
	}

	response, err := backend.Nack(ctx, created.ID, nil, true)
	if err != nil {
		t.Fatalf("Nack(requeue) error = %v", err)
	}
	if replacement == nil || response.State != core.StateActive {
		t.Fatalf("replacement/response = %+v/%+v", replacement, response)
	}
	var active activeJobInfo
	if _, err := backend.active.GetJSON(ctx, created.ID, &active); err != nil {
		t.Fatalf("replacement active entry was deleted: %v", err)
	}
	if active.DispatchSeq != originalRecord.DispatchSeq+1 {
		t.Fatalf("active dispatch sequence = %d, want %d", active.DispatchSeq, originalRecord.DispatchSeq+1)
	}
	if _, err := replacementBackend.Ack(ctx, replacement.ID, nil); err != nil {
		t.Fatalf("replacement Ack() error = %v", err)
	}
}

func TestStalledRecovery_AmbiguousPublishDoesNotLoseJob(t *testing.T) {
	backend := newIntegrationBackend(t)
	ctx := context.Background()
	queue := "it-handoff-stalled-" + core.NewUUIDv7()
	created, err := backend.Push(ctx, &core.Job{Type: "stalled.run", Queue: queue, Args: json.RawMessage(`[]`)})
	if err != nil {
		t.Fatalf("Push() error = %v", err)
	}
	if jobs, err := backend.Fetch(ctx, []string{queue}, 1, "stalled-worker", 15); err != nil || len(jobs) != 1 {
		t.Fatalf("Fetch() error=%v jobs=%d", err, len(jobs))
	}
	time.Sleep(30 * time.Millisecond)

	originalPublish := backend.publishJob
	var injected atomic.Bool
	backend.publishJob = func(ctx context.Context, queue, jobID string, seq uint64) error {
		if jobID != created.ID {
			return originalPublish(ctx, queue, jobID, seq)
		}
		if err := originalPublish(ctx, queue, jobID, seq); err != nil {
			return err
		}
		if !injected.Swap(true) {
			return context.DeadlineExceeded
		}
		return nil
	}
	if err := backend.RequeueStalled(ctx); err == nil {
		t.Fatal("RequeueStalled() error = nil, want ambiguous publish error")
	}
	if !backend.active.Exists(ctx, created.ID) || !backend.stats.Exists(ctx, handoffKey(created.ID)) {
		t.Fatal("stalled source was not retained across ambiguous publish")
	}

	backend.publishJob = originalPublish
	if err := backend.RequeueStalled(ctx); err != nil {
		t.Fatalf("RequeueStalled() reconciliation error = %v", err)
	}
	refetched := fetchJobEventually(t, backend, queue, "stalled-worker-2")
	if refetched.ID != created.ID {
		t.Fatalf("refetched ID = %q, want %q", refetched.ID, created.ID)
	}
}

func TestJetStreamSource_RedeliveryAfterUncommittedFetch(t *testing.T) {
	backend := newIntegrationBackend(t)
	ctx := context.Background()
	queue := "it-handoff-redelivery-" + core.NewUUIDv7()
	created, err := backend.Push(ctx, &core.Job{Type: "redeliver.run", Queue: queue, Args: json.RawMessage(`[]`)})
	if err != nil {
		t.Fatalf("Push() error = %v", err)
	}

	deliveries, err := backend.consumers.FetchMessages(ctx, queue, 1)
	if err != nil || len(deliveries) != 1 {
		t.Fatalf("FetchMessages() error=%v deliveries=%d", err, len(deliveries))
	}
	if err := deliveries[0].Msg.Nak(); err != nil {
		t.Fatalf("Nak() source error = %v", err)
	}

	fetched := fetchJobEventually(t, backend, queue, "redelivery-worker")
	if fetched.ID != created.ID {
		t.Fatalf("redelivered ID = %q, want %q", fetched.ID, created.ID)
	}
	if _, err := backend.Ack(ctx, fetched.ID, nil); err != nil {
		t.Fatalf("Ack() error = %v", err)
	}
	if job, err := backend.Info(ctx, created.ID); err != nil || job.State != core.StateCompleted {
		t.Fatalf("redelivered job state = %+v err=%v", job, err)
	}
}

func TestImmediatePush_AmbiguousPublishLeavesRecoverableMarker(t *testing.T) {
	backend := newIntegrationBackend(t)
	ctx := context.Background()
	queue := "it-handoff-push-" + core.NewUUIDv7()
	job := &core.Job{
		ID:    core.NewUUIDv7(),
		Type:  "push.run",
		Queue: queue,
		Args:  json.RawMessage(`[]`),
	}
	originalPublish := backend.publishJob
	var injected atomic.Bool
	backend.publishJob = func(ctx context.Context, queue, jobID string, seq uint64) error {
		if jobID != job.ID {
			return originalPublish(ctx, queue, jobID, seq)
		}
		if err := originalPublish(ctx, queue, jobID, seq); err != nil {
			return err
		}
		if !injected.Swap(true) {
			return context.DeadlineExceeded
		}
		return nil
	}
	if _, err := backend.Push(ctx, job); err == nil {
		t.Fatal("Push() error = nil, want ambiguous publish error")
	}
	if !backend.stats.Exists(ctx, handoffKey(job.ID)) {
		t.Fatal("Push() did not retain a durable dispatch marker")
	}

	backend.publishJob = originalPublish
	if err := backend.reconcileHandoffs(ctx); err != nil {
		t.Fatalf("reconcileHandoffs() error = %v", err)
	}
	if backend.stats.Exists(ctx, handoffKey(job.ID)) {
		t.Fatal("push marker remained after confirmed reconciliation")
	}
	fetched := fetchJobEventually(t, backend, queue, "push-worker")
	if fetched.ID != job.ID {
		t.Fatalf("fetched ID = %q, want %q", fetched.ID, job.ID)
	}
}
