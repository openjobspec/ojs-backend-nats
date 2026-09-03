package nats

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/openjobspec/ojs-backend-nats/internal/core"
)

// ListDeadLetter returns dead letter jobs.
func (b *NATSBackend) ListDeadLetter(ctx context.Context, limit, offset int) ([]*core.Job, int, error) {
	keys, err := b.dead.Keys(ctx)
	if err != nil {
		return nil, 0, err
	}

	total := len(keys)

	// Sort keys (job IDs) for consistent ordering
	sort.Strings(keys)

	// Apply offset and limit
	end := offset + limit
	if end > total {
		end = total
	}
	if offset >= total {
		return []*core.Job{}, total, nil
	}

	var jobs []*core.Job
	for _, key := range keys[offset:end] {
		job, err := b.Info(ctx, key)
		if err == nil {
			jobs = append(jobs, job)
		}
	}

	return jobs, total, nil
}

// RetryDeadLetter retries a dead letter job.
func (b *NATSBackend) RetryDeadLetter(ctx context.Context, jobID string) (*core.Job, error) {
	_, deadRevision, err := b.dead.Get(ctx, jobID)
	if err != nil {
		return nil, core.NewNotFoundError("Dead letter job", jobID)
	}

	now := time.Now()

	record, err := b.getJobRecord(ctx, jobID)
	if err != nil {
		return nil, core.NewNotFoundError("Dead letter job", jobID)
	}
	job := record.Job

	if job.State == core.StateDiscarded {
		job.State = core.StateAvailable
		job.Attempt = 0
		job.EnqueuedAt = core.FormatTime(now)
		job.Error = nil
		job.Errors = nil
		job.CompletedAt = ""
		job.RetryDelayMs = nil
		record.DispatchSeq++
		record.DispatchSource = "dead-retry"
		if _, err := b.updateJobRecord(ctx, record); err != nil {
			return nil, b.jobTransitionError(ctx, "retry dead-letter", jobID, core.StateDiscarded, err)
		}
	} else if job.State != core.StateAvailable || record.DispatchSource != "dead-retry" {
		return nil, core.NewConflictError(
			fmt.Sprintf("Cannot retry dead-letter job in state '%s'.", job.State),
			map[string]any{"job_id": jobID, "current_state": job.State},
		)
	}

	if err := b.publishDispatch(ctx, record); err != nil {
		return nil, core.NewInternalError(fmt.Sprintf("republishing dead-letter job: %v", err))
	}
	if err := b.dead.DeleteRevision(ctx, jobID, deadRevision); err != nil &&
		!errors.Is(err, jetstream.ErrKeyExists) {
		return nil, core.NewInternalError(fmt.Sprintf("removing dead-letter index: %v", err))
	}

	return b.Info(ctx, jobID)
}

// DeleteDeadLetter removes a job from the dead letter queue.
func (b *NATSBackend) DeleteDeadLetter(ctx context.Context, jobID string) error {
	_, revision, err := b.dead.Get(ctx, jobID)
	if err != nil {
		return core.NewNotFoundError("Dead letter job", jobID)
	}
	return b.dead.DeleteRevision(ctx, jobID, revision)
}
