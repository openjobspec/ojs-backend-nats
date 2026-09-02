package nats

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"sync/atomic"
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
	jobs       *kv.Store
	unique     *kv.UniqueStore
	cronStore  *kv.CronStore
	workers    *kv.Store
	workflows  *kv.Store
	queues     *kv.Store
	scheduled  *kv.Store
	retry      *kv.Store
	dead       *kv.Store
	active     *kv.Store
	stats      *kv.Store
	cronClaims *kv.Store

	// JetStream consumer manager
	consumers *ConsumerManager

	startTime        time.Time
	cpStore          *checkpointStore
	instanceID       string
	cronLegacyCursor atomic.Uint64

	publishJob func(context.Context, string, string, uint64) error
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

	buckets, err := openKVBuckets(ctx, js,
		BucketJobs, BucketUnique, BucketCron, BucketWorkers, BucketWorkflows,
		BucketQueues, BucketScheduled, BucketRetry, BucketDead, BucketActive, BucketStats,
		bucketCronClaims,
	)
	if err != nil {
		nc.Close()
		return nil, err
	}

	backend := &NATSBackend{
		nc:         nc,
		js:         js,
		jobs:       kv.NewStore(buckets[BucketJobs]),
		unique:     kv.NewUniqueStore(buckets[BucketUnique]),
		cronStore:  kv.NewCronStore(buckets[BucketCron]),
		workers:    kv.NewStore(buckets[BucketWorkers]),
		workflows:  kv.NewStore(buckets[BucketWorkflows]),
		queues:     kv.NewStore(buckets[BucketQueues]),
		scheduled:  kv.NewStore(buckets[BucketScheduled]),
		retry:      kv.NewStore(buckets[BucketRetry]),
		dead:       kv.NewStore(buckets[BucketDead]),
		active:     kv.NewStore(buckets[BucketActive]),
		stats:      kv.NewStore(buckets[BucketStats]),
		cronClaims: kv.NewStore(buckets[bucketCronClaims]),
		consumers:  NewConsumerManager(js),
		startTime:  time.Now(),
		cpStore:    newCheckpointStore(),
		instanceID: core.NewUUIDv7(),
	}
	backend.publishJob = func(ctx context.Context, queue, jobID string, dispatchSeq uint64) error {
		return PublishJobDispatch(ctx, backend.js, queue, jobID, dispatchSeq)
	}
	if err := backend.maintainLegacyCronClaims(ctx, cronLegacyMaintenanceLimit); err != nil {
		slog.Warn("legacy cron claim maintenance incomplete", "error", err)
	}
	return backend, nil
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
	if job == nil {
		return nil, core.NewInvalidRequestError("Job is required.", nil)
	}
	ctx, span := ojsotel.StartJobSpan(ctx, "push", job.ID, job.Type, job.Queue)
	defer span.End()

	now := time.Now()

	if job.ID == "" {
		job.ID = core.NewUUIDv7()
	}
	if job.Queue == "" {
		job.Queue = "default"
	}

	job.CreatedAt = core.FormatTime(now)
	job.Attempt = 0

	reservation, existing, err := b.reserveUnique(ctx, job, now)
	if err != nil {
		return nil, err
	}
	if existing != nil {
		return existing, nil
	}

	// Register queue
	if err := b.ensureQueue(ctx, job.Queue); err != nil {
		b.rollbackUnique(ctx, reservation)
		return nil, fmt.Errorf("register queue: %w", err)
	}

	// Store rate limit config if specified
	if job.RateLimit != nil && job.RateLimit.MaxPerSecond > 0 {
		if err := b.updateQueueRateLimit(ctx, job.Queue, job.RateLimit.MaxPerSecond); err != nil {
			b.rollbackUnique(ctx, reservation)
			return nil, fmt.Errorf("store queue rate limit: %w", err)
		}
	}

	// Determine initial state
	if job.ScheduledAt != "" {
		scheduledTime, err := time.Parse(time.RFC3339, job.ScheduledAt)
		if err == nil && scheduledTime.After(now) {
			job.State = core.StateScheduled
			job.EnqueuedAt = core.FormatTime(now)

			_, err := b.scheduled.Create(ctx, job.ID, []byte(job.ScheduledAt))
			if err != nil {
				if errors.Is(err, jetstream.ErrKeyExists) {
					b.rollbackUnique(ctx, reservation)
					return nil, core.NewDuplicateError(job.ID)
				}
				existingValue, _, readErr := b.scheduled.Get(ctx, job.ID)
				if readErr != nil || string(existingValue) != job.ScheduledAt {
					b.rollbackUnique(ctx, reservation)
					return nil, fmt.Errorf("index scheduled job: %w", err)
				}
			}
			if _, err := b.createJobRecord(ctx, &jobRecord{Job: job}); err != nil {
				if errors.Is(err, jetstream.ErrKeyExists) {
					b.rollbackUnique(ctx, reservation)
					return nil, core.NewDuplicateError(job.ID)
				}
				existing, readErr := b.getJobRecord(ctx, job.ID)
				if readErr != nil || !sameCreatedJob(existing, job, 0, "") {
					b.rollbackUnique(ctx, reservation)
					return nil, fmt.Errorf("store scheduled job: %w", err)
				}
			}
			b.cancelReplacedUnique(ctx, reservation)
			return job, nil
		}
	}

	job.State = core.StateAvailable
	job.EnqueuedAt = core.FormatTime(now)

	marker := handoffMarker{
		JobID:       job.ID,
		Queue:       job.Queue,
		Kind:        handoffPush,
		DispatchSeq: 1,
		CreatedAt:   core.FormatTime(now),
	}
	markerRevision, err := b.createHandoff(ctx, marker)
	if err != nil {
		if errors.Is(err, jetstream.ErrKeyExists) {
			b.rollbackUnique(ctx, reservation)
			return nil, core.NewDuplicateError(job.ID)
		}
		existing, existingRevision, readErr := b.getHandoff(ctx, handoffKey(job.ID))
		if readErr == nil && existing == marker {
			markerRevision = existingRevision
		} else {
			b.rollbackUnique(ctx, reservation)
			return nil, fmt.Errorf("create job dispatch handoff: %w", err)
		}
	}
	record := &jobRecord{
		Job:            job,
		DispatchSeq:    marker.DispatchSeq,
		DispatchSource: marker.Kind,
	}
	if _, err := b.createJobRecord(ctx, record); err != nil {
		if errors.Is(err, jetstream.ErrKeyExists) {
			_ = b.stats.DeleteRevision(ctx, handoffKey(job.ID), markerRevision)
			b.rollbackUnique(ctx, reservation)
			return nil, core.NewDuplicateError(job.ID)
		}
		existing, readErr := b.getJobRecord(ctx, job.ID)
		if readErr != nil || !sameCreatedJob(existing, job, marker.DispatchSeq, marker.Kind) {
			_ = b.stats.DeleteRevision(ctx, handoffKey(job.ID), markerRevision)
			b.rollbackUnique(ctx, reservation)
			return nil, fmt.Errorf("store job: %w", err)
		}
	}

	if err := b.reconcileHandoff(ctx, handoffKey(job.ID)); err != nil {
		return nil, fmt.Errorf("publish job: %w", err)
	}

	b.cancelReplacedUnique(ctx, reservation)
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

		fetchedMessages, err := b.consumers.FetchMessages(ctx, queue, remaining)
		if err != nil {
			continue
		}

		for _, fetched := range fetchedMessages {
			record, err := b.getJobRecord(ctx, fetched.JobID)
			if err != nil {
				if errors.Is(err, jetstream.ErrKeyNotFound) || errors.Is(err, jetstream.ErrKeyDeleted) {
					_ = b.consumers.AckFetched(ctx, fetched)
				} else {
					_ = b.consumers.NakFetched(fetched)
				}
				continue
			}
			job := record.Job

			// Check expiry
			if job.ExpiresAt != "" {
				expTime, err := time.Parse(time.RFC3339, job.ExpiresAt)
				if err == nil && now.After(expTime) {
					job.State = core.StateDiscarded
					job.CompletedAt = core.FormatTime(now)
					if _, err := b.updateJobRecord(ctx, record); err == nil {
						_ = b.consumers.AckFetched(ctx, fetched)
						b.advanceWorkflow(ctx, job.ID, core.StateDiscarded, nil)
					} else if errors.Is(err, jetstream.ErrKeyExists) {
						_ = b.consumers.AckFetched(ctx, fetched)
					} else {
						_ = b.consumers.NakFetched(fetched)
					}
					continue
				}
			}

			if job.State != core.StateAvailable {
				_ = b.consumers.AckFetched(ctx, fetched)
				continue
			}

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
				ClaimedAt:          core.FormatTime(now),
				JobRevision:        record.Revision,
				DispatchSeq:        record.DispatchSeq,
			}
			activeData, marshalErr := json.Marshal(activeInfo)
			if marshalErr != nil {
				slog.Warn("nats fetch: failed to marshal active info", "job_id", job.ID, "error", marshalErr)
				_ = b.consumers.AckFetched(ctx, fetched)
				continue
			}

			activeRevision, err := b.active.Create(ctx, job.ID, activeData)
			if errors.Is(err, jetstream.ErrKeyExists) {
				var existing activeJobInfo
				existingRevision, getErr := b.active.GetJSON(ctx, job.ID, &existing)
				if getErr != nil {
					continue
				}
				if existing.DispatchSeq == record.DispatchSeq && !activeClaimStale(existing, now) {
					_ = b.consumers.AckFetched(ctx, fetched)
					continue
				}
				activeRevision, err = b.active.Update(ctx, job.ID, activeData, existingRevision)
			}
			if err != nil {
				if errors.Is(err, jetstream.ErrKeyExists) {
					_ = b.consumers.AckFetched(ctx, fetched)
				}
				continue
			}

			job.State = core.StateActive
			job.StartedAt = core.FormatTime(now)
			job.WorkerID = workerID
			job.Attempt++
			if _, err := b.updateJobRecord(ctx, record); err != nil {
				if errors.Is(err, jetstream.ErrKeyExists) {
					_ = b.active.DeleteRevision(ctx, job.ID, activeRevision)
					_ = b.consumers.AckFetched(ctx, fetched)
				}
				continue
			}
			b.consumers.Track(job.ID, fetched.Msg, record.DispatchSeq)
			if err := b.recordFetchTime(ctx, queue, now); err != nil {
				slog.Warn("nats fetch: failed to record queue fetch time", "queue", queue, "error", err)
			}

			jobs = append(jobs, job)
		}
	}

	return jobs, nil
}

// Ack acknowledges a job as completed.
func (b *NATSBackend) Ack(ctx context.Context, jobID string, result []byte) (*core.AckResponse, error) {
	ctx, span := ojsotel.StartJobSpan(ctx, "ack", jobID, "", "")
	defer span.End()

	record, err := b.getJobRecord(ctx, jobID)
	if err != nil {
		return nil, core.NewNotFoundError("Job", jobID)
	}
	job := record.Job

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

	if len(result) > 0 {
		job.Result = json.RawMessage(result)
	}

	if _, err := b.updateJobRecord(ctx, record); err != nil {
		return nil, b.jobTransitionError(ctx, "acknowledge", jobID, core.StateActive, err)
	}
	b.cleanupActiveSource(ctx, jobID, record.DispatchSeq)
	b.incrementCompleted(ctx, job.Queue)
	b.advanceWorkflow(ctx, jobID, core.StateCompleted, result)

	return &core.AckResponse{
		Acknowledged: true,
		ID:           jobID,
		State:        core.StateCompleted,
		CompletedAt:  now,
		Job:          job,
	}, nil
}

// Nack reports a job failure.
func (b *NATSBackend) Nack(ctx context.Context, jobID string, jobErr *core.JobError, requeue bool) (*core.NackResponse, error) {
	ctx, span := ojsotel.StartJobSpan(ctx, "nack", jobID, "", "")
	defer span.End()

	record, err := b.getJobRecord(ctx, jobID)
	if err != nil {
		return nil, core.NewNotFoundError("Job", jobID)
	}
	job := record.Job

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
		targetDispatchSeq := record.DispatchSeq + 1
		if err := b.prepareActiveHandoff(ctx, record, handoffNackRequeue); err != nil {
			return nil, core.NewInternalError(fmt.Sprintf("durably requeueing job: %v", err))
		}

		retRecord, infoErr := b.getJobRecord(ctx, jobID)
		if infoErr != nil {
			return nil, core.NewNotFoundError("Job", jobID)
		}
		retJob := retRecord.Job
		requeued := retRecord.DispatchSeq == targetDispatchSeq &&
			retRecord.DispatchSource == handoffNackRequeue &&
			(retJob.State == core.StateAvailable || retJob.State == core.StateActive)
		if !requeued {
			return nil, core.NewConflictError(
				fmt.Sprintf("Cannot requeue job because its state changed to '%s'.", retJob.State),
				map[string]any{"job_id": jobID, "current_state": retJob.State},
			)
		}
		return &core.NackResponse{
			ID:          jobID,
			State:       retJob.State,
			Attempt:     job.Attempt,
			MaxAttempts: maxAttempts,
			Job:         retJob,
		}, nil
	}

	currentAttempt := job.Attempt

	errJSON, err := buildJobErrorPayload(jobErr, job.Attempt)
	if err != nil {
		return nil, core.NewInternalError(fmt.Sprintf("encoding job error payload: %v", err))
	}

	if errJSON != nil {
		job.Errors = append(job.Errors, json.RawMessage(errJSON))
	}

	isNonRetryable := jobErr != nil && jobErr.Retryable != nil && !*jobErr.Retryable

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

	if isNonRetryable || currentAttempt >= maxAttempts {
		discardedAt := core.FormatTime(now)
		job.State = core.StateDiscarded
		job.CompletedAt = discardedAt
		if errJSON != nil {
			job.Error = json.RawMessage(errJSON)
		}

		if _, err := b.updateJobRecord(ctx, record); err != nil {
			return nil, b.jobTransitionError(ctx, "discard", jobID, core.StateActive, err)
		}
		b.cleanupActiveSource(ctx, jobID, record.DispatchSeq)

		if onExhaustion == "dead_letter" {
			if _, err := b.dead.Put(ctx, jobID, []byte(core.FormatTime(now))); err != nil {
				slog.Warn("nats: failed to index discarded job in dead letter", "job_id", jobID, "error", err)
			}
		}

		b.advanceWorkflow(ctx, jobID, core.StateDiscarded, nil)

		retJob, _ := b.Info(ctx, jobID)
		return &core.NackResponse{
			ID:          jobID,
			State:       core.StateDiscarded,
			Attempt:     currentAttempt,
			MaxAttempts: maxAttempts,
			CompletedAt: discardedAt,
			DiscardedAt: discardedAt,
			Job:         retJob,
		}, nil
	}

	backoff := core.CalculateBackoff(job.Retry, currentAttempt)
	backoffMs := backoff.Milliseconds()
	nextAttemptAt := now.Add(backoff)

	job.State = core.StateRetryable
	job.RetryDelayMs = &backoffMs
	if errJSON != nil {
		job.Error = json.RawMessage(errJSON)
	}

	if _, err := b.retry.Put(ctx, jobID, []byte(core.FormatTime(nextAttemptAt))); err != nil {
		return nil, core.NewInternalError(fmt.Sprintf("indexing retryable job: %v", err))
	}
	if _, err := b.updateJobRecord(ctx, record); err != nil {
		return nil, b.jobTransitionError(ctx, "retry", jobID, core.StateActive, err)
	}
	b.cleanupActiveSource(ctx, jobID, record.DispatchSeq)

	retJob, _ := b.Info(ctx, jobID)
	return &core.NackResponse{
		ID:            jobID,
		State:         core.StateRetryable,
		Attempt:       currentAttempt,
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
	record, err := b.getJobRecord(ctx, jobID)
	if err != nil {
		return nil, core.NewNotFoundError("Job", jobID)
	}
	job := record.Job

	if core.IsTerminalState(job.State) {
		return nil, core.NewConflictError(
			fmt.Sprintf("Cannot cancel job in terminal state '%s'.", job.State),
			map[string]any{
				"job_id":        jobID,
				"current_state": job.State,
			},
		)
	}

	previousState := job.State
	now := core.NowFormatted()
	job.State = core.StateCancelled
	job.CancelledAt = now

	if _, err := b.updateJobRecord(ctx, record); err != nil {
		return nil, b.jobTransitionError(ctx, "cancel", jobID, previousState, err)
	}
	b.cleanupActiveSource(ctx, jobID, record.DispatchSeq)
	cleanupIndex(ctx, b.scheduled, jobID, "scheduled")
	cleanupIndex(ctx, b.retry, jobID, "retry")
	b.advanceWorkflow(ctx, jobID, core.StateCancelled, nil)

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
	workerData, marshalErr := json.Marshal(workerInfo)
	if marshalErr != nil {
		return nil, core.NewInternalError(fmt.Sprintf("encode worker heartbeat: %v", marshalErr))
	}
	if _, err := b.workers.Put(ctx, workerID, workerData); err != nil {
		return nil, core.NewInternalError(fmt.Sprintf("store worker heartbeat: %v", err))
	}

	// Extend visibility for active jobs
	for _, jobID := range activeJobs {
		job, err := b.getJobState(ctx, jobID)
		if err != nil || job.State != core.StateActive {
			continue
		}
		var current activeJobInfo
		activeRevision, err := b.active.GetJSON(ctx, jobID, &current)
		if err != nil {
			continue
		}

		timeout := time.Duration(visibilityTimeoutMs) * time.Millisecond
		deadline := now.Add(timeout)

		activeInfo := activeJobInfo{
			Queue:              job.Queue,
			VisibilityDeadline: core.FormatTime(deadline),
			WorkerID:           workerID,
			ClaimedAt:          current.ClaimedAt,
			JobRevision:        current.JobRevision,
			DispatchSeq:        current.DispatchSeq,
		}
		activeData, marshalErr := json.Marshal(activeInfo)
		if marshalErr != nil {
			slog.Warn("nats heartbeat: failed to marshal active info", "job_id", jobID, "error", marshalErr)
			continue
		}
		if _, err := b.active.Update(ctx, jobID, activeData, activeRevision); err != nil {
			continue
		}
		if err := b.consumers.InProgress(jobID); err != nil {
			continue
		}

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
	// Pre-validate all jobs before any writes to avoid partial batch failures
	for _, job := range jobs {
		if err := core.ValidateEnqueueRequest(&core.EnqueueRequest{
			Type: job.Type,
			Args: job.Args,
		}); err != nil {
			return nil, err
		}
	}

	results := make([]*core.Job, 0, len(jobs))
	for i, job := range jobs {
		created, err := b.Push(ctx, job)
		if err != nil {
			return results, fmt.Errorf("push batch job %d (%s): %w", i, job.ID, err)
		}
		results = append(results, created)
	}

	return results, nil
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
		n, parseErr := strconv.Atoi(string(completedData))
		if parseErr != nil {
			slog.Warn("nats: invalid completed count", "queue", name, "value", string(completedData), "error", parseErr)
		}
		completed = n
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
	data, marshalErr := json.Marshal(workerInfo)
	if marshalErr != nil {
		return fmt.Errorf("marshal worker info: %w", marshalErr)
	}
	_, err := b.workers.Put(ctx, workerID, data)
	return err
}
