package nats

import (
	"context"
	"fmt"
	"strconv"

	"github.com/nats-io/nats.go/jetstream"
)

// PublishJob publishes a job ID to the appropriate queue subject via JetStream.
func PublishJob(ctx context.Context, js jetstream.JetStream, queue, jobID string) error {
	return PublishJobDispatch(ctx, js, queue, jobID, 1)
}

// PublishJobDispatch publishes one logical dispatch using a stable JetStream
// message ID so retrying an ambiguous publish is idempotent.
func PublishJobDispatch(ctx context.Context, js jetstream.JetStream, queue, jobID string, dispatchSeq uint64) error {
	subject := QueueJobsSubject(queue)
	messageID := jobID + ":" + strconv.FormatUint(dispatchSeq, 10)
	_, err := js.Publish(ctx, subject, []byte(jobID), jetstream.WithMsgID(messageID))
	if err != nil {
		return fmt.Errorf("publish job %s to %s: %w", jobID, subject, err)
	}
	return nil
}

// PublishJobWithPriority publishes a job ID to a priority-segmented subject.
func PublishJobWithPriority(ctx context.Context, js jetstream.JetStream, queue, jobID string, priority int) error {
	// For now, publish to the main queue subject.
	// Priority ordering is handled at the application level during fetch
	// by storing priority in KV and sorting fetched results.
	return PublishJob(ctx, js, queue, jobID)
}

func (b *NATSBackend) publishDispatch(ctx context.Context, record *jobRecord) error {
	return b.publishJob(ctx, record.Job.Queue, record.Job.ID, record.DispatchSeq)
}
