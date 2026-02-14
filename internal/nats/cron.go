package nats

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/robfig/cron/v3"

	"github.com/openjobspec/ojs-backend-nats/internal/core"
)

// RegisterCron registers a cron job.
func (b *NATSBackend) RegisterCron(ctx context.Context, cronJob *core.CronJob) (*core.CronJob, error) {
	expr := cronJob.Expression
	if expr == "" {
		expr = cronJob.Schedule
	}

	parser := cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor)

	var schedule cron.Schedule
	var err error

	if cronJob.Timezone != "" {
		loc, locErr := time.LoadLocation(cronJob.Timezone)
		if locErr != nil {
			return nil, core.NewInvalidRequestError(
				fmt.Sprintf("Invalid timezone: %s", cronJob.Timezone),
				map[string]any{"timezone": cronJob.Timezone},
			)
		}
		schedule, err = parser.Parse("CRON_TZ=" + loc.String() + " " + expr)
		if err != nil {
			schedule, err = parser.Parse(expr)
		}
	} else {
		schedule, err = parser.Parse(expr)
	}

	if err != nil {
		return nil, core.NewInvalidRequestError(
			fmt.Sprintf("Invalid cron expression: %s", expr),
			map[string]any{"expression": expr, "error": err.Error()},
		)
	}

	now := time.Now()
	cronJob.CreatedAt = core.FormatTime(now)
	cronJob.NextRunAt = core.FormatTime(schedule.Next(now))
	cronJob.Schedule = expr
	cronJob.Expression = expr

	if cronJob.Queue == "" {
		cronJob.Queue = "default"
	}
	if cronJob.OverlapPolicy == "" {
		cronJob.OverlapPolicy = "allow"
	}
	cronJob.Enabled = true

	if err := b.cronStore.Register(ctx, cronJob); err != nil {
		return nil, fmt.Errorf("register cron: %w", err)
	}

	return cronJob, nil
}

// ListCron lists all registered cron jobs.
func (b *NATSBackend) ListCron(ctx context.Context) ([]*core.CronJob, error) {
	return b.cronStore.List(ctx)
}

// DeleteCron removes a cron job.
func (b *NATSBackend) DeleteCron(ctx context.Context, name string) (*core.CronJob, error) {
	cronJob, err := b.cronStore.Get(ctx, name)
	if err != nil {
		return nil, core.NewNotFoundError("Cron job", name)
	}

	if err := b.cronStore.Delete(ctx, name); err != nil {
		return nil, fmt.Errorf("delete cron: %w", err)
	}

	return cronJob, nil
}

// FireCronJobs checks cron schedules and fires due jobs.
func (b *NATSBackend) FireCronJobs(ctx context.Context) error {
	crons, err := b.cronStore.List(ctx)
	if err != nil {
		return err
	}

	now := time.Now()
	parser := cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor)

	for _, cronJob := range crons {
		if cronJob.NextRunAt == "" {
			continue
		}

		nextRun, err := time.Parse(core.TimeFormat, cronJob.NextRunAt)
		if err != nil {
			continue
		}

		if now.Before(nextRun) {
			continue
		}

		// Extract job details from template
		var jobType string
		var args json.RawMessage
		var queue string
		if cronJob.JobTemplate != nil {
			jobType = cronJob.JobTemplate.Type
			args = cronJob.JobTemplate.Args
			if cronJob.JobTemplate.Options != nil {
				queue = cronJob.JobTemplate.Options.Queue
			}
		}
		if queue == "" {
			queue = "default"
		}

		// Check overlap policy
		if cronJob.OverlapPolicy == "skip" {
			if b.isCronInstanceRunning(ctx, cronJob.Name) {
				b.updateCronNextRun(ctx, cronJob, parser, now)
				continue
			}
		}

		// Fire the cron job
		cronVisTimeout := 600000 // 10 minutes
		job := &core.Job{
			Type:                jobType,
			Args:                args,
			Queue:               queue,
			VisibilityTimeoutMs: &cronVisTimeout,
		}

		created, pushErr := b.Push(ctx, job)
		if pushErr != nil {
			continue
		}

		// Track instance for overlap checking
		if cronJob.OverlapPolicy == "skip" {
			b.setCronInstance(ctx, cronJob.Name, created.ID)
		}

		b.updateCronNextRun(ctx, cronJob, parser, now)
	}

	return nil
}

func (b *NATSBackend) updateCronNextRun(ctx context.Context, cronJob *core.CronJob, parser cron.Parser, now time.Time) {
	expr := cronJob.Expression
	if expr == "" {
		expr = cronJob.Schedule
	}
	schedule, err := parser.Parse(expr)
	if err == nil {
		cronJob.LastRunAt = core.FormatTime(now)
		cronJob.NextRunAt = core.FormatTime(schedule.Next(now))
		b.cronStore.Register(ctx, cronJob)
	}
}

func (b *NATSBackend) isCronInstanceRunning(ctx context.Context, cronName string) bool {
	key := "cron-instance." + cronName
	data, _, err := b.stats.Get(ctx, key)
	if err != nil {
		return false
	}
	jobID := string(data)
	job, err := b.getJobState(ctx, jobID)
	if err != nil {
		return false
	}
	return !core.IsTerminalState(job.State)
}

func (b *NATSBackend) setCronInstance(ctx context.Context, cronName, jobID string) {
	key := "cron-instance." + cronName
	b.stats.Put(ctx, key, []byte(jobID))
}
