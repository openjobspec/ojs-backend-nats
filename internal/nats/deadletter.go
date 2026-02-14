package nats

import (
	"context"
	"sort"
	"time"

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
	if !b.dead.Exists(ctx, jobID) {
		return nil, core.NewNotFoundError("Dead letter job", jobID)
	}

	now := time.Now()

	job, err := b.getJobState(ctx, jobID)
	if err != nil {
		return nil, core.NewNotFoundError("Dead letter job", jobID)
	}

	// Reset job state
	job.State = core.StateAvailable
	job.Attempt = 0
	job.EnqueuedAt = core.FormatTime(now)
	job.Error = nil
	job.Errors = nil
	job.CompletedAt = ""
	job.RetryDelayMs = nil

	b.putJobState(ctx, job)
	b.dead.Delete(ctx, jobID)

	// Re-publish to JetStream
	PublishJob(ctx, b.js, job.Queue, jobID)

	return b.Info(ctx, jobID)
}

// DeleteDeadLetter removes a job from the dead letter queue.
func (b *NATSBackend) DeleteDeadLetter(ctx context.Context, jobID string) error {
	if !b.dead.Exists(ctx, jobID) {
		return core.NewNotFoundError("Dead letter job", jobID)
	}
	return b.dead.Delete(ctx, jobID)
}
