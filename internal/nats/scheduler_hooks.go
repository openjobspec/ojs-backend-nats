package nats

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/openjobspec/ojs-backend-nats/internal/core"
	"github.com/openjobspec/ojs-backend-nats/internal/kv"
)

// parseIndexTimestamp parses a timestamp stored in a scheduling index, accepting
// both the OJS millisecond format and RFC3339. It reports false when neither
// layout matches.
func parseIndexTimestamp(s string) (time.Time, bool) {
	if t, err := time.Parse(core.TimeFormat, s); err == nil {
		return t, true
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, true
	}
	return time.Time{}, false
}

// promoteDueJobs scans an index bucket whose values are due-timestamps and, for
// every entry whose time has passed, transitions the job back to "available",
// deletes the index entry, and republishes it to its queue. label is used only
// for error context. Stale entries (missing job state) are pruned.
func (b *NATSBackend) promoteDueJobs(ctx context.Context, index *kv.Store, label, expectedState string) error {
	if err := b.reconcileHandoffs(ctx); err != nil {
		return err
	}
	keys, err := index.Keys(ctx)
	if err != nil {
		return err
	}

	now := time.Now()
	var firstErr error
	setErr := func(e error) {
		if firstErr == nil {
			firstErr = e
		}
	}

	for _, jobID := range keys {
		data, indexRevision, err := index.Get(ctx, jobID)
		if err != nil {
			continue
		}

		dueAt, ok := parseIndexTimestamp(string(data))
		if !ok {
			continue
		}
		if now.Before(dueAt) {
			continue
		}

		record, err := b.getJobRecord(ctx, jobID)
		if err != nil {
			_ = index.DeleteRevision(ctx, jobID, indexRevision)
			continue
		}

		switch {
		case record.Job.State == expectedState:
			record.Job.State = core.StateAvailable
			record.Job.EnqueuedAt = core.FormatTime(now)
			record.Job.StartedAt = ""
			record.Job.WorkerID = ""
			record.DispatchSeq++
			record.DispatchSource = label
			if _, err := b.updateJobRecord(ctx, record); err != nil {
				if !errors.Is(err, jetstream.ErrKeyExists) {
					setErr(fmt.Errorf("update %s job state for %s: %w", label, jobID, err))
				}
				continue
			}
		case record.Job.State == core.StateActive:
			// A retry index can be written immediately before the active→retry
			// CAS. Leave it durable until that transition resolves.
			continue
		case record.Job.State != core.StateAvailable || record.DispatchSource != label:
			_ = index.DeleteRevision(ctx, jobID, indexRevision)
			continue
		}

		if err := b.publishDispatch(ctx, record); err != nil {
			setErr(fmt.Errorf("publish %s job %s: %w", label, jobID, err))
			continue
		}
		if err := index.DeleteRevision(ctx, jobID, indexRevision); err != nil &&
			!errors.Is(err, jetstream.ErrKeyExists) {
			setErr(fmt.Errorf("delete %s index for %s: %w", label, jobID, err))
		}
	}

	return firstErr
}

// PromoteScheduled moves due scheduled jobs to their available queues.
func (b *NATSBackend) PromoteScheduled(ctx context.Context) error {
	return b.promoteDueJobs(ctx, b.scheduled, "scheduled", core.StateScheduled)
}

// PromoteRetries moves due retry jobs to their available queues.
func (b *NATSBackend) PromoteRetries(ctx context.Context) error {
	return b.promoteDueJobs(ctx, b.retry, "retry", core.StateRetryable)
}

// RequeueStalled finds and requeues jobs that exceeded their visibility timeout.
func (b *NATSBackend) RequeueStalled(ctx context.Context) error {
	if err := b.reconcileHandoffs(ctx); err != nil {
		return err
	}
	keys, err := b.active.Keys(ctx)
	if err != nil {
		return err
	}

	now := time.Now()
	var firstErr error

	for _, jobID := range keys {
		data, activeRevision, err := b.active.Get(ctx, jobID)
		if err != nil {
			continue
		}

		var info activeJobInfo
		if err := unmarshalJSON(data, &info); err != nil {
			continue
		}

		deadline, err := time.Parse(core.TimeFormat, info.VisibilityDeadline)
		if err != nil {
			continue
		}

		if now.After(deadline) {
			record, err := b.getJobRecord(ctx, jobID)
			if err != nil {
				_ = b.active.DeleteRevision(ctx, jobID, activeRevision)
				continue
			}

			if record.Job.State != core.StateActive {
				_ = b.active.DeleteRevision(ctx, jobID, activeRevision)
				continue
			}
			if info.DispatchSeq != record.DispatchSeq {
				continue
			}

			if err := b.prepareActiveHandoff(ctx, record, handoffStalled); err != nil && firstErr == nil {
				firstErr = fmt.Errorf("recover stalled job %s: %w", jobID, err)
			}
		}
	}

	return firstErr
}
