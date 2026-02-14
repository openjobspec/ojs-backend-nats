package nats

import (
	"context"
	"fmt"
	"time"

	"github.com/openjobspec/ojs-backend-nats/internal/core"
)

// PromoteScheduled moves due scheduled jobs to their available queues.
func (b *NATSBackend) PromoteScheduled(ctx context.Context) error {
	keys, err := b.scheduled.Keys(ctx)
	if err != nil {
		return err
	}

	now := time.Now()
	var firstErr error

	for _, jobID := range keys {
		data, _, err := b.scheduled.Get(ctx, jobID)
		if err != nil {
			continue
		}

		scheduledAt, err := time.Parse(time.RFC3339, string(data))
		if err != nil {
			// Try the OJS time format
			scheduledAt, err = time.Parse(core.TimeFormat, string(data))
			if err != nil {
				continue
			}
		}

		if now.Before(scheduledAt) {
			continue
		}

		job, err := b.getJobState(ctx, jobID)
		if err != nil {
			b.scheduled.Delete(ctx, jobID)
			continue
		}

		job.State = core.StateAvailable
		job.EnqueuedAt = core.FormatTime(now)

		if _, err := b.putJobState(ctx, job); err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("update scheduled job state for %s: %w", jobID, err)
			}
			continue
		}
		if err := b.scheduled.Delete(ctx, jobID); err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("delete scheduled index for %s: %w", jobID, err)
			}
			continue
		}

		if err := PublishJob(ctx, b.js, job.Queue, jobID); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("publish scheduled job %s: %w", jobID, err)
		}
	}

	return firstErr
}

// PromoteRetries moves due retry jobs to their available queues.
func (b *NATSBackend) PromoteRetries(ctx context.Context) error {
	keys, err := b.retry.Keys(ctx)
	if err != nil {
		return err
	}

	now := time.Now()
	var firstErr error

	for _, jobID := range keys {
		data, _, err := b.retry.Get(ctx, jobID)
		if err != nil {
			continue
		}

		nextAttemptAt, err := time.Parse(core.TimeFormat, string(data))
		if err != nil {
			nextAttemptAt, err = time.Parse(time.RFC3339, string(data))
			if err != nil {
				continue
			}
		}

		if now.Before(nextAttemptAt) {
			continue
		}

		job, err := b.getJobState(ctx, jobID)
		if err != nil {
			b.retry.Delete(ctx, jobID)
			continue
		}

		job.State = core.StateAvailable
		job.EnqueuedAt = core.FormatTime(now)

		if _, err := b.putJobState(ctx, job); err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("update retryable job state for %s: %w", jobID, err)
			}
			continue
		}
		if err := b.retry.Delete(ctx, jobID); err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("delete retry index for %s: %w", jobID, err)
			}
			continue
		}

		if err := PublishJob(ctx, b.js, job.Queue, jobID); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("publish retryable job %s: %w", jobID, err)
		}
	}

	return firstErr
}

// RequeueStalled finds and requeues jobs that exceeded their visibility timeout.
func (b *NATSBackend) RequeueStalled(ctx context.Context) error {
	keys, err := b.active.Keys(ctx)
	if err != nil {
		return err
	}

	now := time.Now()
	var firstErr error

	for _, jobID := range keys {
		data, _, err := b.active.Get(ctx, jobID)
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
			job, err := b.getJobState(ctx, jobID)
			if err != nil {
				b.active.Delete(ctx, jobID)
				continue
			}

			if job.State != core.StateActive {
				b.active.Delete(ctx, jobID)
				continue
			}

			// Requeue
			job.State = core.StateAvailable
			job.StartedAt = ""
			job.EnqueuedAt = core.FormatTime(now)

			if _, err := b.putJobState(ctx, job); err != nil {
				if firstErr == nil {
					firstErr = fmt.Errorf("update stalled job state for %s: %w", jobID, err)
				}
				continue
			}
			if err := b.active.Delete(ctx, jobID); err != nil {
				if firstErr == nil {
					firstErr = fmt.Errorf("delete active state for stalled job %s: %w", jobID, err)
				}
				continue
			}
			if err := b.consumers.AckMessage(jobID); err != nil {
				if firstErr == nil {
					firstErr = fmt.Errorf("ack stale message for stalled job %s: %w", jobID, err)
				}
				continue
			}

			if err := PublishJob(ctx, b.js, job.Queue, jobID); err != nil && firstErr == nil {
				firstErr = fmt.Errorf("republish stalled job %s: %w", jobID, err)
			}
		}
	}

	return firstErr
}
