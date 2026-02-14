package nats

import (
	"context"
	"fmt"

	"github.com/nats-io/nats.go/jetstream"
)

// PublishJob publishes a job ID to the appropriate queue subject via JetStream.
func PublishJob(ctx context.Context, js jetstream.JetStream, queue, jobID string) error {
	subject := QueueJobsSubject(queue)
	_, err := js.Publish(ctx, subject, []byte(jobID))
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
