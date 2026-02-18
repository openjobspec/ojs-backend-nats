package nats

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	ojsotel "github.com/openjobspec/ojs-go-backend-common/otel"

	"github.com/openjobspec/ojs-backend-nats/internal/core"
	"github.com/openjobspec/ojs-backend-nats/internal/kv"
)

// NATSBackend implements core.Backend using NATS JetStream and KV.
type NATSBackend struct {
	nc *nats.Conn
	js jetstream.JetStream

	// KV stores
	jobs      *kv.Store
	unique    *kv.UniqueStore
	cronStore *kv.CronStore
	workers   *kv.Store
	workflows *kv.Store
	queues    *kv.Store
	scheduled *kv.Store
	retry     *kv.Store
	dead      *kv.Store
	active    *kv.Store
	stats     *kv.Store

	// JetStream consumer manager
	consumers *ConsumerManager

	startTime time.Time
}

// New creates a new NATSBackend, connecting to NATS and setting up JetStream resources.
func New(natsURL string) (*NATSBackend, error) {
	nc, err := nats.Connect(natsURL,
		nats.MaxReconnects(-1),
		nats.ReconnectWait(time.Second),
	)
	if err != nil {
		return nil, fmt.Errorf("connecting to NATS: %w", err)
	}

	js, err := jetstream.New(nc)
	if err != nil {
		nc.Close()
		return nil, fmt.Errorf("creating JetStream context: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Set up streams and KV buckets
	if err := SetupJetStream(ctx, js); err != nil {
		nc.Close()
		return nil, fmt.Errorf("setting up JetStream: %w", err)
	}

	// Open KV buckets
	openKV := func(name string) (jetstream.KeyValue, error) {
		bucket, err := js.KeyValue(ctx, name)
		if err != nil {
			return nil, fmt.Errorf("opening KV bucket %s: %w", name, err)
		}
		return bucket, nil
	}

	jobsKV, err := openKV(BucketJobs)
	if err != nil {
		nc.Close()
		return nil, err
	}
	uniqueKV, err := openKV(BucketUnique)
	if err != nil {
		nc.Close()
		return nil, err
	}
	cronKV, err := openKV(BucketCron)
	if err != nil {
		nc.Close()
		return nil, err
	}
	workersKV, err := openKV(BucketWorkers)
	if err != nil {
		nc.Close()
		return nil, err
	}
	workflowsKV, err := openKV(BucketWorkflows)
	if err != nil {
		nc.Close()
		return nil, err
	}
	queuesKV, err := openKV(BucketQueues)
	if err != nil {
		nc.Close()
		return nil, err
	}
	scheduledKV, err := openKV(BucketScheduled)
	if err != nil {
		nc.Close()
		return nil, err
	}
	retryKV, err := openKV(BucketRetry)
	if err != nil {
		nc.Close()
		return nil, err
	}
	deadKV, err := openKV(BucketDead)
	if err != nil {
		nc.Close()
		return nil, err
	}
	activeKV, err := openKV(BucketActive)
	if err != nil {
		nc.Close()
		return nil, err
	}
	statsKV, err := openKV(BucketStats)
	if err != nil {
		nc.Close()
		return nil, err
	}

	return &NATSBackend{
		nc:        nc,
		js:        js,
		jobs:      kv.NewStore(jobsKV),
		unique:    kv.NewUniqueStore(uniqueKV),
		cronStore: kv.NewCronStore(cronKV),
		workers:   kv.NewStore(workersKV),
		workflows: kv.NewStore(workflowsKV),
		queues:    kv.NewStore(queuesKV),
		scheduled: kv.NewStore(scheduledKV),
		retry:     kv.NewStore(retryKV),
		dead:      kv.NewStore(deadKV),
		active:    kv.NewStore(activeKV),
		stats:     kv.NewStore(statsKV),
		consumers: NewConsumerManager(js),
		startTime: time.Now(),
	}, nil
}

// Conn returns the underlying NATS connection for use by auxiliary services (e.g., pub/sub broker).
func (b *NATSBackend) Conn() *nats.Conn {
	return b.nc
}

func (b *NATSBackend) Close() error {
	b.nc.Close()
	return nil
}

// Push enqueues a single job.
func (b *NATSBackend) Push(ctx context.Context, job *core.Job) (*core.Job, error) {
	ctx, span := ojsotel.StartJobSpan(ctx, "push", job.ID, job.Type, job.Queue)
	defer span.End()

	now := time.Now()

	if job.ID == "" {
		job.ID = core.NewUUIDv7()
	}

	job.CreatedAt = core.FormatTime(now)
	job.Attempt = 0

	// Handle unique jobs
	if job.Unique != nil {
		fingerprint := kv.ComputeFingerprint(job)

		conflict := job.Unique.OnConflict
		if conflict == "" {
			conflict = "reject"
		}

		existingID, err := b.unique.CheckAndSet(ctx, fingerprint, job.ID)
		if err != nil {
			return nil, fmt.Errorf("unique check: %w", err)
		}
		if existingID != "" {
			// Check if existing job is in a relevant state
			existingJob, infoErr := b.Info(ctx, existingID)
			if infoErr == nil {
				isRelevant := false
				if len(job.Unique.States) > 0 {
					for _, s := range job.Unique.States {
						if s == existingJob.State {
							isRelevant = true
							break
						}
					}
				} else {
					isRelevant = !core.IsTerminalState(existingJob.State)
				}

				if isRelevant {
					switch conflict {
					case "reject":
						return nil, &core.OJSError{
							Code:    core.ErrCodeDuplicate,
							Message: "A job with the same unique key already exists.",
							Details: map[string]any{
								"existing_job_id": existingID,
								"unique_key":      fingerprint,
							},
						}
					case "ignore":
						existingJob.IsExisting = true
						return existingJob, nil
					case "replace":
						b.Cancel(ctx, existingID)
					}
				}
			}
			// Overwrite the unique key with new job ID
			b.unique.Release(ctx, fingerprint)
			b.unique.CheckAndSet(ctx, fingerprint, job.ID)
		}
	}

	// Register queue
	b.ensureQueue(ctx, job.Queue)

	// Store rate limit config if specified
	if job.RateLimit != nil && job.RateLimit.MaxPerSecond > 0 {
		b.updateQueueRateLimit(ctx, job.Queue, job.RateLimit.MaxPerSecond)
	}

	// Determine initial state
	if job.ScheduledAt != "" {
		scheduledTime, err := time.Parse(time.RFC3339, job.ScheduledAt)
		if err == nil && scheduledTime.After(now) {
			job.State = core.StateScheduled
			job.EnqueuedAt = core.FormatTime(now)

			if _, err := b.putJobState(ctx, job); err != nil {
				return nil, fmt.Errorf("store scheduled job: %w", err)
			}

			b.scheduled.Put(ctx, job.ID, []byte(job.ScheduledAt))
			return job, nil
		}
	}

	job.State = core.StateAvailable
	job.EnqueuedAt = core.FormatTime(now)

	if _, err := b.putJobState(ctx, job); err != nil {
		return nil, fmt.Errorf("store job: %w", err)
	}

	if err := PublishJob(ctx, b.js, job.Queue, job.ID); err != nil {
		return nil, fmt.Errorf("publish job: %w", err)
	}

	return job, nil
}

// Fetch claims jobs from the specified queues.
func (b *NATSBackend) Fetch(ctx context.Context, queues []string, count int, workerID string, visibilityTimeoutMs int) ([]*core.Job, error) {
	ctx, span := ojsotel.StartStorageSpan(ctx, "fetch", "nats")
	defer span.End()

	now := time.Now()
	var jobs []*core.Job

	for _, queue := range queues {
		if len(jobs) >= count {
			break
		}

		if b.isQueuePaused(ctx, queue) {
			continue
		}

		if b.isRateLimited(ctx, queue, now) {
			continue
		}

		remaining := count - len(jobs)

		jobIDs, err := b.consumers.FetchMessages(ctx, queue, remaining)
		if err != nil {
			continue
		}

		for _, jobID := range jobIDs {
			job, err := b.getJobState(ctx, jobID)
			if err != nil {
				b.consumers.AckMessage(jobID)
				continue
			}

			// Check expiry
			if job.ExpiresAt != "" {
				expTime, err := time.Parse(time.RFC3339, job.ExpiresAt)
				if err == nil && now.After(expTime) {
					job.State = core.StateDiscarded
					if _, err := b.putJobState(ctx, job); err != nil {
						b.consumers.AckMessage(jobID)
						continue
					}
					b.consumers.AckMessage(jobID)
					continue
				}
			}

			if job.State != core.StateAvailable {
				b.consumers.AckMessage(jobID)
				continue
			}

			job.State = core.StateActive
			job.StartedAt = core.FormatTime(now)

			effectiveVisTimeout := visibilityTimeoutMs
			if effectiveVisTimeout <= 0 && job.VisibilityTimeoutMs != nil {
				effectiveVisTimeout = *job.VisibilityTimeoutMs
			}
			if effectiveVisTimeout <= 0 {
				effectiveVisTimeout = core.DefaultVisibilityTimeoutMs
			}

			deadline := now.Add(time.Duration(effectiveVisTimeout) * time.Millisecond)

			activeInfo := activeJobInfo{
				Queue:              queue,
				VisibilityDeadline: core.FormatTime(deadline),
				WorkerID:           workerID,
			}
			activeData, _ := json.Marshal(activeInfo)
			if _, err := b.active.Put(ctx, jobID, activeData); err != nil {
				b.consumers.AckMessage(jobID)
				continue
			}

			if _, err := b.putJobState(ctx, job); err != nil {
				b.active.Delete(ctx, jobID)
				b.consumers.AckMessage(jobID)
				continue
			}
			b.recordFetchTime(ctx, queue, now)

			jobs = append(jobs, job)
		}
	}

	return jobs, nil
}

// Ack acknowledges a job as completed.
func (b *NATSBackend) Ack(ctx context.Context, jobID string, result []byte) (*core.AckResponse, error) {
	ctx, span := ojsotel.StartJobSpan(ctx, "ack", jobID, "", "")
	defer span.End()

	job, err := b.getJobState(ctx, jobID)
	if err != nil {
		return nil, core.NewNotFoundError("Job", jobID)
	}

	if job.State != core.StateActive {
		return nil, core.NewConflictError(
			fmt.Sprintf("Cannot acknowledge job not in 'active' state. Current state: '%s'.", job.State),
			map[string]any{
				"job_id":         jobID,
				"current_state":  job.State,
				"expected_state": "active",
			},
		)
	}

	now := core.NowFormatted()
	job.State = core.StateCompleted
	job.CompletedAt = now
	job.Error = nil

	if result != nil && len(result) > 0 {
		job.Result = json.RawMessage(result)
	}

	if _, err := b.putJobState(ctx, job); err != nil {
		return nil, core.NewInternalError(fmt.Sprintf("updating completed job state: %v", err))
	}
	if err := b.active.Delete(ctx, jobID); err != nil {
		return nil, core.NewInternalError(fmt.Sprintf("removing active job state: %v", err))
	}
	if err := b.consumers.AckMessage(jobID); err != nil {
		return nil, core.NewInternalError(fmt.Sprintf("acking job message: %v", err))
	}
	b.incrementCompleted(ctx, job.Queue)
	b.advanceWorkflow(ctx, jobID, core.StateCompleted, result)

	updatedJob, infoErr := b.Info(ctx, jobID)
	if infoErr != nil {
		updatedJob = job
	}

	return &core.AckResponse{
		Acknowledged: true,
		JobID:        jobID,
		State:        core.StateCompleted,
		CompletedAt:  now,
		Job:          updatedJob,
	}, nil
}

// Nack reports a job failure.
func (b *NATSBackend) Nack(ctx context.Context, jobID string, jobErr *core.JobError, requeue bool) (*core.NackResponse, error) {
	ctx, span := ojsotel.StartJobSpan(ctx, "nack", jobID, "", "")
	defer span.End()

	job, err := b.getJobState(ctx, jobID)
	if err != nil {
		return nil, core.NewNotFoundError("Job", jobID)
	}

	if job.State != core.StateActive {
		return nil, core.NewConflictError(
			fmt.Sprintf("Cannot fail job not in 'active' state. Current state: '%s'.", job.State),
			map[string]any{
				"job_id":         jobID,
				"current_state":  job.State,
				"expected_state": "active",
			},
		)
	}

	now := time.Now()
	maxAttempts := 3
	if job.MaxAttempts != nil {
		maxAttempts = *job.MaxAttempts
	}

	if requeue {
		job.State = core.StateAvailable
		job.StartedAt = ""
		job.EnqueuedAt = core.FormatTime(now)

		if _, err := b.putJobState(ctx, job); err != nil {
			return nil, core.NewInternalError(fmt.Sprintf("updating requeued job state: %v", err))
		}
		if err := b.active.Delete(ctx, jobID); err != nil {
			return nil, core.NewInternalError(fmt.Sprintf("removing active state for requeued job: %v", err))
		}
		if err := b.consumers.AckMessage(jobID); err != nil {
			return nil, core.NewInternalError(fmt.Sprintf("acking requeued job message: %v", err))
		}
		if err := PublishJob(ctx, b.js, job.Queue, jobID); err != nil {
			return nil, core.NewInternalError(fmt.Sprintf("republishing requeued job: %v", err))
		}

		retJob, _ := b.Info(ctx, jobID)
		return &core.NackResponse{
			JobID:       jobID,
			State:       core.StateAvailable,
			Attempt:     job.Attempt,
			MaxAttempts: maxAttempts,
			Job:         retJob,
		}, nil
	}

	newAttempt := job.Attempt + 1

	var errJSON []byte
	if jobErr != nil {
		errObj := map[string]any{
			"message": jobErr.Message,
			"attempt": job.Attempt,
		}
		if jobErr.Code != "" {
			errObj["type"] = jobErr.Code
		}
		if jobErr.Type != "" {
			errObj["type"] = jobErr.Type
		}
		if jobErr.Retryable != nil {
			errObj["retryable"] = *jobErr.Retryable
		}
		if jobErr.Details != nil {
			errObj["details"] = jobErr.Details
		}
		errJSON, err = json.Marshal(errObj)
		if err != nil {
			return nil, core.NewInternalError(fmt.Sprintf("encoding job error payload: %v", err))
		}
	}

	if errJSON != nil {
		job.Errors = append(job.Errors, json.RawMessage(errJSON))
	}

	isNonRetryable := false
	if jobErr != nil && jobErr.Retryable != nil && !*jobErr.Retryable {
		isNonRetryable = true
	}

	if !isNonRetryable && jobErr != nil && job.Retry != nil {
		for _, pattern := range job.Retry.NonRetryableErrors {
			errType := jobErr.Code
			if jobErr.Type != "" {
				errType = jobErr.Type
			}
			if matchesPattern(errType, pattern) || matchesPattern(jobErr.Message, pattern) {
				isNonRetryable = true
				break
			}
		}
	}

	onExhaustion := "discard"
	if job.Retry != nil && job.Retry.OnExhaustion != "" {
		onExhaustion = job.Retry.OnExhaustion
	}

	if isNonRetryable || newAttempt >= maxAttempts {
		discardedAt := core.FormatTime(now)
		job.State = core.StateDiscarded
		job.CompletedAt = discardedAt
		job.Attempt = newAttempt
		if errJSON != nil {
			job.Error = json.RawMessage(errJSON)
		}

		if _, err := b.putJobState(ctx, job); err != nil {
			return nil, core.NewInternalError(fmt.Sprintf("updating discarded job state: %v", err))
		}
		if err := b.active.Delete(ctx, jobID); err != nil {
			return nil, core.NewInternalError(fmt.Sprintf("removing active state for discarded job: %v", err))
		}
		if err := b.consumers.AckMessage(jobID); err != nil {
			return nil, core.NewInternalError(fmt.Sprintf("acking discarded job message: %v", err))
		}

		if onExhaustion == "dead_letter" {
			if _, err := b.dead.Put(ctx, jobID, []byte(core.FormatTime(now))); err != nil {
				return nil, core.NewInternalError(fmt.Sprintf("indexing dead letter job: %v", err))
			}
		}

		b.advanceWorkflow(ctx, jobID, core.StateDiscarded, nil)

		retJob, _ := b.Info(ctx, jobID)
		return &core.NackResponse{
			JobID:       jobID,
			State:       core.StateDiscarded,
			Attempt:     newAttempt,
			MaxAttempts: maxAttempts,
			DiscardedAt: discardedAt,
			Job:         retJob,
		}, nil
	}

	backoff := core.CalculateBackoff(job.Retry, newAttempt)
	backoffMs := backoff.Milliseconds()
	nextAttemptAt := now.Add(backoff)

	job.State = core.StateRetryable
	job.Attempt = newAttempt
	job.RetryDelayMs = &backoffMs
	if errJSON != nil {
		job.Error = json.RawMessage(errJSON)
	}

	if _, err := b.putJobState(ctx, job); err != nil {
		return nil, core.NewInternalError(fmt.Sprintf("updating retryable job state: %v", err))
	}
	if err := b.active.Delete(ctx, jobID); err != nil {
		return nil, core.NewInternalError(fmt.Sprintf("removing active state for retryable job: %v", err))
	}
	if err := b.consumers.AckMessage(jobID); err != nil {
		return nil, core.NewInternalError(fmt.Sprintf("acking retryable job message: %v", err))
	}
	if _, err := b.retry.Put(ctx, jobID, []byte(core.FormatTime(nextAttemptAt))); err != nil {
		return nil, core.NewInternalError(fmt.Sprintf("indexing retryable job: %v", err))
	}

	retJob, _ := b.Info(ctx, jobID)
	return &core.NackResponse{
		JobID:         jobID,
		State:         core.StateRetryable,
		Attempt:       newAttempt,
		MaxAttempts:   maxAttempts,
		NextAttemptAt: core.FormatTime(nextAttemptAt),
		Job:           retJob,
	}, nil
}

// Info retrieves job details.
func (b *NATSBackend) Info(ctx context.Context, jobID string) (*core.Job, error) {
	job, err := b.getJobState(ctx, jobID)
	if err != nil {
		return nil, core.NewNotFoundError("Job", jobID)
	}
	return job, nil
}

// Cancel cancels a job.
func (b *NATSBackend) Cancel(ctx context.Context, jobID string) (*core.Job, error) {
	job, err := b.getJobState(ctx, jobID)
	if err != nil {
		return nil, core.NewNotFoundError("Job", jobID)
	}

	if core.IsTerminalState(job.State) {
		return nil, core.NewConflictError(
			fmt.Sprintf("Cannot cancel job in terminal state '%s'.", job.State),
			map[string]any{
				"job_id":        jobID,
				"current_state": job.State,
			},
		)
	}

	now := core.NowFormatted()
	job.State = core.StateCancelled
	job.CancelledAt = now

	if _, err := b.putJobState(ctx, job); err != nil {
		return nil, core.NewInternalError(fmt.Sprintf("updating cancelled job state: %v", err))
	}
	if err := b.active.Delete(ctx, jobID); err != nil {
		return nil, core.NewInternalError(fmt.Sprintf("removing active state for cancelled job: %v", err))
	}
	if err := b.scheduled.Delete(ctx, jobID); err != nil {
		return nil, core.NewInternalError(fmt.Sprintf("removing scheduled index for cancelled job: %v", err))
	}
	if err := b.retry.Delete(ctx, jobID); err != nil {
		return nil, core.NewInternalError(fmt.Sprintf("removing retry index for cancelled job: %v", err))
	}
	if err := b.consumers.AckMessage(jobID); err != nil {
		return nil, core.NewInternalError(fmt.Sprintf("acking cancelled job message: %v", err))
	}

	return job, nil
}

// ListQueues returns all known queues.
func (b *NATSBackend) ListQueues(ctx context.Context) ([]core.QueueInfo, error) {
	keys, err := b.queues.Keys(ctx)
	if err != nil {
		return nil, err
	}

	sort.Strings(keys)
	var queues []core.QueueInfo
	for _, name := range keys {
		status := "active"
		if b.isQueuePaused(ctx, name) {
			status = "paused"
		}
		queues = append(queues, core.QueueInfo{
			Name:   name,
			Status: status,
		})
	}
	return queues, nil
}

// Health returns the health status.
func (b *NATSBackend) Health(ctx context.Context) (*core.HealthResponse, error) {
	resp := &core.HealthResponse{
		Version:       core.OJSVersion,
		UptimeSeconds: int64(time.Since(b.startTime).Seconds()),
	}

	status := b.nc.Status()
	if status != nats.CONNECTED {
		resp.Status = "degraded"
		resp.Backend = core.BackendHealth{
			Type:   "nats",
			Status: "disconnected",
			Error:  fmt.Sprintf("NATS status: %v", status),
		}
		return resp, fmt.Errorf("NATS not connected")
	}

	// Measure actual NATS RTT with a KV operation
	start := time.Now()
	b.stats.Exists(ctx, "_health_check")
	latency := time.Since(start).Milliseconds()

	resp.Status = "ok"
	resp.Backend = core.BackendHealth{
		Type:      "nats",
		Status:    "connected",
		LatencyMs: latency,
	}
	return resp, nil
}

// Heartbeat extends visibility and reports worker state.
func (b *NATSBackend) Heartbeat(ctx context.Context, workerID string, activeJobs []string, visibilityTimeoutMs int) (*core.HeartbeatResponse, error) {
	now := time.Now()
	extended := make([]string, 0)

	// Read existing worker state to preserve directive
	directive := "continue"
	existingData, _, err := b.workers.Get(ctx, workerID)
	if err == nil {
		var existingState map[string]any
		if json.Unmarshal(existingData, &existingState) == nil {
			if d, ok := existingState["directive"]; ok {
				if ds, ok := d.(string); ok && ds != "" {
					directive = ds
				}
			}
		}
	}

	// Update worker info, preserving directive
	workerInfo := map[string]any{
		"last_heartbeat": core.FormatTime(now),
		"active_jobs":    len(activeJobs),
	}
	if directive != "continue" {
		workerInfo["directive"] = directive
	}
	workerData, _ := json.Marshal(workerInfo)
	b.workers.Put(ctx, workerID, workerData)

	// Extend visibility for active jobs
	for _, jobID := range activeJobs {
		job, err := b.getJobState(ctx, jobID)
		if err != nil || job.State != core.StateActive {
			continue
		}

		timeout := time.Duration(visibilityTimeoutMs) * time.Millisecond
		deadline := now.Add(timeout)

		activeInfo := activeJobInfo{
			Queue:              job.Queue,
			VisibilityDeadline: core.FormatTime(deadline),
			WorkerID:           workerID,
		}
		activeData, _ := json.Marshal(activeInfo)
		b.active.Put(ctx, jobID, activeData)
		b.consumers.InProgress(jobID)

		extended = append(extended, jobID)
	}

	// Check job metadata for test_directive
	if directive == "continue" {
		for _, jobID := range activeJobs {
			job, err := b.getJobState(ctx, jobID)
			if err != nil {
				continue
			}
			if job.Meta != nil {
				var metaObj map[string]any
				if json.Unmarshal(job.Meta, &metaObj) == nil {
					if td, ok := metaObj["test_directive"]; ok {
						if tdStr, ok := td.(string); ok && tdStr != "" {
							directive = tdStr
							break
						}
					}
				}
			}
		}
	}

	return &core.HeartbeatResponse{
		State:        "active",
		Directive:    directive,
		JobsExtended: extended,
		ServerTime:   core.FormatTime(now),
	}, nil
}

// PushBatch enqueues multiple jobs.
func (b *NATSBackend) PushBatch(ctx context.Context, jobs []*core.Job) ([]*core.Job, error) {
	now := time.Now()

	for i, job := range jobs {
		if job.ID == "" {
			job.ID = core.NewUUIDv7()
		}
		job.State = core.StateAvailable
		job.Attempt = 0
		job.CreatedAt = core.FormatTime(now)
		job.EnqueuedAt = core.FormatTime(now)

		if _, err := b.putJobState(ctx, job); err != nil {
			return jobs[:i], fmt.Errorf("store batch job %d (%s): %w", i, job.ID, err)
		}
		b.ensureQueue(ctx, job.Queue)
		if err := PublishJob(ctx, b.js, job.Queue, job.ID); err != nil {
			return jobs[:i+1], fmt.Errorf("publish batch job %d (%s): %w", i, job.ID, err)
		}
	}

	return jobs, nil
}

// QueueStats returns statistics for a queue.
func (b *NATSBackend) QueueStats(ctx context.Context, name string) (*core.QueueStats, error) {
	status := "active"
	if b.isQueuePaused(ctx, name) {
		status = "paused"
	}

	available := 0
	activeCount := 0
	completed := 0

	activeKeys, _ := b.active.Keys(ctx)
	for _, key := range activeKeys {
		data, _, err := b.active.Get(ctx, key)
		if err != nil {
			continue
		}
		var info activeJobInfo
		if json.Unmarshal(data, &info) == nil && info.Queue == name {
			activeCount++
		}
	}

	completedData, _, err := b.stats.Get(ctx, kvStatsKey(name, "completed"))
	if err == nil {
		completed, _ = strconv.Atoi(string(completedData))
	}

	consumer, err := b.consumers.GetConsumer(ctx, name)
	if err == nil {
		info, err := consumer.Info(ctx)
		if err == nil {
			available = int(info.NumPending)
		}
	}

	return &core.QueueStats{
		Queue:  name,
		Status: status,
		Stats: core.Stats{
			Available: available,
			Active:    activeCount,
			Completed: completed,
		},
	}, nil
}

// PauseQueue pauses a queue.
func (b *NATSBackend) PauseQueue(ctx context.Context, name string) error {
	var meta queueMeta
	return b.queues.UpdateJSON(ctx, name, &meta, func() {
		meta.Name = name
		meta.Paused = true
	})
}

// ResumeQueue resumes a queue.
func (b *NATSBackend) ResumeQueue(ctx context.Context, name string) error {
	var meta queueMeta
	return b.queues.UpdateJSON(ctx, name, &meta, func() {
		meta.Name = name
		meta.Paused = false
	})
}

// SetWorkerState sets a directive for a worker.
func (b *NATSBackend) SetWorkerState(ctx context.Context, workerID string, state string) error {
	workerInfo := map[string]any{
		"directive": state,
	}
	data, _ := json.Marshal(workerInfo)
	_, err := b.workers.Put(ctx, workerID, data)
	return err
}
