package nats

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/openjobspec/ojs-backend-nats/internal/core"
)

const (
	handoffPrefix = "handoff."
	handoffLease  = 30 * time.Second

	handoffPush        = "push"
	handoffNackRequeue = "nack-requeue"
	handoffStalled     = "stalled"
)

type handoffMarker struct {
	JobID       string `json:"job_id"`
	Queue       string `json:"queue"`
	Kind        string `json:"kind"`
	FromState   string `json:"from_state,omitempty"`
	DispatchSeq uint64 `json:"dispatch_seq"`
	CreatedAt   string `json:"created_at"`
}

func handoffKey(jobID string) string {
	return handoffPrefix + jobID
}

func (b *NATSBackend) createHandoff(ctx context.Context, marker handoffMarker) (uint64, error) {
	data, err := json.Marshal(marker)
	if err != nil {
		return 0, err
	}
	return b.stats.Create(ctx, handoffKey(marker.JobID), data)
}

func (b *NATSBackend) getHandoff(ctx context.Context, key string) (handoffMarker, uint64, error) {
	data, revision, err := b.stats.Get(ctx, key)
	if err != nil {
		return handoffMarker{}, 0, err
	}
	var marker handoffMarker
	if err := json.Unmarshal(data, &marker); err != nil {
		return handoffMarker{}, 0, err
	}
	return marker, revision, nil
}

func (b *NATSBackend) reconcileHandoffs(ctx context.Context) error {
	keys, err := b.stats.KeysFiltered(ctx, handoffPrefix+">")
	if err != nil {
		return err
	}
	var firstErr error
	for _, key := range keys {
		if !strings.HasPrefix(key, handoffPrefix) {
			continue
		}
		if err := b.reconcileHandoff(ctx, key); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func (b *NATSBackend) reconcileHandoff(ctx context.Context, key string) error {
	for attempt := 0; attempt < 8; attempt++ {
		marker, markerRevision, err := b.getHandoff(ctx, key)
		if err != nil {
			if errors.Is(err, jetstream.ErrKeyNotFound) || errors.Is(err, jetstream.ErrKeyDeleted) {
				return nil
			}
			return err
		}

		record, err := b.getJobRecord(ctx, marker.JobID)
		if err != nil {
			if errors.Is(err, jetstream.ErrKeyNotFound) || errors.Is(err, jetstream.ErrKeyDeleted) {
				if handoffExpired(marker) {
					return b.stats.DeleteRevision(ctx, key, markerRevision)
				}
				return nil
			}
			return err
		}

		switch marker.Kind {
		case handoffPush:
			if record.Job.State != core.StateAvailable ||
				record.DispatchSeq != marker.DispatchSeq ||
				record.DispatchSource != marker.Kind {
				return b.stats.DeleteRevision(ctx, key, markerRevision)
			}
		case handoffNackRequeue, handoffStalled:
			if record.Job.State == marker.FromState {
				if record.DispatchSeq+1 != marker.DispatchSeq {
					return b.stats.DeleteRevision(ctx, key, markerRevision)
				}
				record.Job.State = core.StateAvailable
				record.Job.StartedAt = ""
				record.Job.WorkerID = ""
				record.Job.EnqueuedAt = core.NowFormatted()
				record.DispatchSeq = marker.DispatchSeq
				record.DispatchSource = marker.Kind
				if _, err := b.updateJobRecord(ctx, record); err != nil {
					if errors.Is(err, jetstream.ErrKeyExists) {
						continue
					}
					return fmt.Errorf("update %s handoff job %s: %w", marker.Kind, marker.JobID, err)
				}
				continue
			}
			if record.Job.State != core.StateAvailable ||
				record.DispatchSeq != marker.DispatchSeq ||
				record.DispatchSource != marker.Kind {
				return b.stats.DeleteRevision(ctx, key, markerRevision)
			}
		default:
			return b.stats.DeleteRevision(ctx, key, markerRevision)
		}

		if err := b.publishDispatch(ctx, record); err != nil {
			return fmt.Errorf("publish %s handoff for %s: %w", marker.Kind, marker.JobID, err)
		}
		if marker.Kind != handoffPush {
			sourceDispatchSeq := marker.DispatchSeq - 1
			if err := b.consumers.AckMessage(ctx, marker.JobID, sourceDispatchSeq); err != nil {
				return fmt.Errorf("ack %s source for %s: %w", marker.Kind, marker.JobID, err)
			}
			if err := b.deleteActiveDispatch(ctx, marker.JobID, sourceDispatchSeq); err != nil {
				return fmt.Errorf("delete active source for %s: %w", marker.JobID, err)
			}
		}
		if err := b.stats.DeleteRevision(ctx, key, markerRevision); err != nil {
			if errors.Is(err, jetstream.ErrKeyExists) {
				continue
			}
			return err
		}
		return nil
	}
	return &core.OJSError{
		Code:      core.ErrCodeConflict,
		Message:   "Handoff changed concurrently; reconciliation will retry.",
		Retryable: true,
		Details:   map[string]any{"handoff_key": key},
	}
}

func handoffExpired(marker handoffMarker) bool {
	createdAt, err := time.Parse(time.RFC3339, marker.CreatedAt)
	return err != nil || time.Since(createdAt) >= handoffLease
}

func (b *NATSBackend) prepareActiveHandoff(
	ctx context.Context,
	record *jobRecord,
	kind string,
) error {
	marker := handoffMarker{
		JobID:       record.Job.ID,
		Queue:       record.Job.Queue,
		Kind:        kind,
		FromState:   core.StateActive,
		DispatchSeq: record.DispatchSeq + 1,
		CreatedAt:   core.NowFormatted(),
	}
	if _, err := b.createHandoff(ctx, marker); err != nil && !errors.Is(err, jetstream.ErrKeyExists) {
		return err
	}
	return b.reconcileHandoff(ctx, handoffKey(record.Job.ID))
}
