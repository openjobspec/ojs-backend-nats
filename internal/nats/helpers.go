package nats

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strconv"
	"sync"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/openjobspec/ojs-backend-nats/internal/core"
	"github.com/openjobspec/ojs-backend-nats/internal/kv"
)

var regexCache sync.Map // pattern string -> *regexp.Regexp

func matchesPattern(s, pattern string) bool {
	key := "^" + pattern + "$"
	if cached, ok := regexCache.Load(key); ok {
		if compiled, valid := cached.(*regexp.Regexp); valid {
			return compiled.MatchString(s)
		}
		regexCache.Delete(key)
	}
	re, err := regexp.Compile(key)
	if err != nil {
		return s == pattern
	}
	regexCache.Store(key, re)
	return re.MatchString(s)
}

func currentTime() time.Time {
	return time.Now()
}

func unmarshalJSON(data []byte, v any) error {
	return json.Unmarshal(data, v)
}

// buildJobErrorPayload serializes a JobError into the JSON envelope stored on a
// job's error history. It returns (nil, nil) when jobErr is nil. The attempt
// argument records the 1-based attempt number that produced the error.
func buildJobErrorPayload(jobErr *core.JobError, attempt int) ([]byte, error) {
	if jobErr == nil {
		return nil, nil
	}
	errObj := map[string]any{
		"message": jobErr.Message,
		"attempt": attempt,
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
	return json.Marshal(errObj)
}

type jobRecord struct {
	Job            *core.Job
	Revision       uint64
	DispatchSeq    uint64
	DispatchSource string
}

func sameCreatedJob(record *jobRecord, job *core.Job, dispatchSeq uint64, dispatchSource string) bool {
	return record != nil &&
		record.Job.ID == job.ID &&
		record.Job.Type == job.Type &&
		record.Job.CreatedAt == job.CreatedAt &&
		record.DispatchSeq == dispatchSeq &&
		record.DispatchSource == dispatchSource
}

func (b *NATSBackend) getJobRecord(ctx context.Context, jobID string) (*jobRecord, error) {
	data, revision, err := b.jobs.Get(ctx, jobID)
	if err != nil {
		return nil, err
	}
	record, err := unmarshalJobRecord(data)
	if err != nil {
		return nil, err
	}
	record.Revision = revision
	return record, nil
}

func (b *NATSBackend) getJobState(ctx context.Context, jobID string) (*core.Job, error) {
	record, err := b.getJobRecord(ctx, jobID)
	if err != nil {
		return nil, err
	}
	return record.Job, nil
}

func (b *NATSBackend) createJobRecord(ctx context.Context, record *jobRecord) (uint64, error) {
	state := jobToState(record.Job)
	state.DispatchSeq = record.DispatchSeq
	state.DispatchSource = record.DispatchSource
	data, err := json.Marshal(state)
	if err != nil {
		return 0, err
	}
	return b.jobs.Create(ctx, record.Job.ID, data)
}

func (b *NATSBackend) updateJobRecord(ctx context.Context, record *jobRecord) (uint64, error) {
	state := jobToState(record.Job)
	state.DispatchSeq = record.DispatchSeq
	state.DispatchSource = record.DispatchSource
	data, err := json.Marshal(state)
	if err != nil {
		return 0, err
	}
	return b.jobs.Update(ctx, record.Job.ID, data, record.Revision)
}

func (b *NATSBackend) jobTransitionError(ctx context.Context, operation, jobID, expected string, err error) error {
	if errors.Is(err, jetstream.ErrKeyExists) {
		currentState := "unknown"
		if current, readErr := b.getJobState(ctx, jobID); readErr == nil {
			currentState = current.State
		}
		return core.NewConflictError(
			fmt.Sprintf("Cannot %s job from '%s'; current state is '%s'.", operation, expected, currentState),
			map[string]any{
				"job_id":         jobID,
				"expected_state": expected,
				"current_state":  currentState,
			},
		)
	}
	return core.NewInternalError(fmt.Sprintf("%s job state: %v", operation, err))
}

func deleteCurrentRevision(ctx context.Context, store *kv.Store, key string) error {
	for attempt := 0; attempt < 8; attempt++ {
		_, revision, err := store.Get(ctx, key)
		if err != nil {
			if errors.Is(err, jetstream.ErrKeyNotFound) || errors.Is(err, jetstream.ErrKeyDeleted) {
				return nil
			}
			return err
		}
		if err := store.DeleteRevision(ctx, key, revision); err == nil {
			return nil
		} else if !errors.Is(err, jetstream.ErrKeyExists) {
			return err
		}
	}
	return &kv.ConflictError{Key: key, Attempts: 8, Err: jetstream.ErrKeyExists}
}

func activeClaimStale(info activeJobInfo, now time.Time) bool {
	if info.ClaimedAt != "" {
		if claimedAt, err := time.Parse(time.RFC3339, info.ClaimedAt); err == nil {
			return now.Sub(claimedAt) >= 5*time.Second
		}
	}
	if deadline, err := time.Parse(time.RFC3339, info.VisibilityDeadline); err == nil {
		return !now.Before(deadline)
	}
	return true
}

func (b *NATSBackend) cleanupActiveSource(ctx context.Context, jobID string, dispatchSeq uint64) {
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := b.consumers.AckMessage(cleanupCtx, jobID, dispatchSeq); err != nil {
		slog.Warn("nats: failed to durably ack terminal source", "job_id", jobID, "error", err)
	}
	if err := b.deleteActiveDispatch(cleanupCtx, jobID, dispatchSeq); err != nil {
		slog.Warn("nats: failed to delete active state", "job_id", jobID, "error", err)
	}
}

func (b *NATSBackend) deleteActiveDispatch(ctx context.Context, jobID string, dispatchSeq uint64) error {
	for attempt := 0; attempt < 8; attempt++ {
		var info activeJobInfo
		revision, err := b.active.GetJSON(ctx, jobID, &info)
		if err != nil {
			if errors.Is(err, jetstream.ErrKeyNotFound) || errors.Is(err, jetstream.ErrKeyDeleted) {
				return nil
			}
			return err
		}
		if info.DispatchSeq != dispatchSeq {
			return nil
		}
		if err := b.active.DeleteRevision(ctx, jobID, revision); err == nil {
			return nil
		} else if !errors.Is(err, jetstream.ErrKeyExists) {
			return err
		}
	}
	return &kv.ConflictError{Key: jobID, Attempts: 8, Err: jetstream.ErrKeyExists}
}

func cleanupIndex(ctx context.Context, store *kv.Store, key, label string) {
	if err := deleteCurrentRevision(ctx, store, key); err != nil {
		slog.Warn("nats: failed to delete state index", "job_id", key, "index", label, "error", err)
	}
}

func (b *NATSBackend) ensureQueue(ctx context.Context, name string) error {
	var meta queueMeta
	return b.queues.UpdateJSON(ctx, name, &meta, func() {
		meta.Name = name
	})
}

func (b *NATSBackend) isQueuePaused(ctx context.Context, name string) bool {
	var meta queueMeta
	_, err := b.queues.GetJSON(ctx, name, &meta)
	if err != nil {
		return false
	}
	return meta.Paused
}

func (b *NATSBackend) isRateLimited(ctx context.Context, queue string, now time.Time) bool {
	var meta queueMeta
	_, err := b.queues.GetJSON(ctx, queue, &meta)
	if err != nil || meta.RateLimitPerSec <= 0 {
		return false
	}

	windowMs := int64(1000 / meta.RateLimitPerSec)
	nowMs := now.UnixMilli()
	return nowMs-meta.LastFetchMs < windowMs
}

func (b *NATSBackend) updateQueueRateLimit(ctx context.Context, queue string, maxPerSec int) error {
	var meta queueMeta
	return b.queues.UpdateJSON(ctx, queue, &meta, func() {
		meta.Name = queue
		meta.RateLimitPerSec = maxPerSec
	})
}

func (b *NATSBackend) recordFetchTime(ctx context.Context, queue string, now time.Time) error {
	var meta queueMeta
	return b.queues.UpdateJSON(ctx, queue, &meta, func() {
		meta.LastFetchMs = now.UnixMilli()
	})
}

func (b *NATSBackend) incrementCompleted(ctx context.Context, queue string) {
	key := kvStatsKey(queue, "completed")
	for i := 0; i < 32; i++ {
		data, rev, err := b.stats.Get(ctx, key)
		if err != nil {
			if errors.Is(err, jetstream.ErrKeyNotFound) || errors.Is(err, jetstream.ErrKeyDeleted) {
				if _, createErr := b.stats.Create(ctx, key, []byte("1")); createErr == nil {
					return
				} else if errors.Is(createErr, jetstream.ErrKeyExists) {
					continue
				} else {
					slog.Warn("nats: failed to create completed counter", "key", key, "error", createErr)
					return
				}
			}
			slog.Warn("nats: failed to read completed counter", "key", key, "error", err)
			return
		}
		count, parseErr := strconv.Atoi(string(data))
		if parseErr != nil {
			slog.Warn("nats: invalid counter value", "key", key, "value", string(data), "error", parseErr)
		}
		count++
		_, uErr := b.stats.Update(ctx, key, []byte(strconv.Itoa(count)), rev)
		if uErr == nil {
			return
		}
		if !errors.Is(uErr, jetstream.ErrKeyExists) {
			slog.Warn("nats: failed to update completed counter", "key", key, "error", uErr)
			return
		}
	}
	slog.Warn("nats: completed counter remained contended", "key", key)
}
