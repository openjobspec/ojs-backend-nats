package nats

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/nats-io/nats.go/jetstream"
)

// ConsumerManager manages per-queue pull consumers and in-flight message tracking.
type ConsumerManager struct {
	js        jetstream.JetStream
	consumers sync.Map // map[string]jetstream.Consumer
	inflight  sync.Map // map[string]jetstream.Msg (job_id -> message)
}

// NewConsumerManager creates a new ConsumerManager.
func NewConsumerManager(js jetstream.JetStream) *ConsumerManager {
	return &ConsumerManager{js: js}
}

// GetConsumer returns the pull consumer for a queue, creating it if needed.
func (cm *ConsumerManager) GetConsumer(ctx context.Context, queue string) (jetstream.Consumer, error) {
	if c, ok := cm.consumers.Load(queue); ok {
		return c.(jetstream.Consumer), nil
	}

	consumer, err := EnsureConsumer(ctx, cm.js, queue)
	if err != nil {
		return nil, err
	}

	cm.consumers.Store(queue, consumer)
	return consumer, nil
}

// FetchMessages pulls messages from a queue's consumer.
// Returns up to count job IDs.
func (cm *ConsumerManager) FetchMessages(ctx context.Context, queue string, count int) ([]string, error) {
	consumer, err := cm.GetConsumer(ctx, queue)
	if err != nil {
		return nil, err
	}

	msgs, err := consumer.Fetch(count, jetstream.FetchMaxWait(100*time.Millisecond))
	if err != nil {
		// Timeout or no messages is not an error
		return nil, nil
	}

	var jobIDs []string
	for msg := range msgs.Messages() {
		jobID := string(msg.Data())
		if jobID == "" {
			msg.Ack()
			continue
		}

		// Track the in-flight message for later ack/nack
		cm.inflight.Store(jobID, msg)
		jobIDs = append(jobIDs, jobID)
	}

	if msgs.Error() != nil {
		// Partial results are fine; just log if needed
	}

	return jobIDs, nil
}

// AckMessage acknowledges the JetStream message for a job.
func (cm *ConsumerManager) AckMessage(jobID string) error {
	v, ok := cm.inflight.LoadAndDelete(jobID)
	if !ok {
		// Message not tracked (server restart or already acked)
		return nil
	}
	msg := v.(jetstream.Msg)
	return msg.Ack()
}

// TermMessage terminates the JetStream message (will not be redelivered).
func (cm *ConsumerManager) TermMessage(jobID string) error {
	v, ok := cm.inflight.LoadAndDelete(jobID)
	if !ok {
		return nil
	}
	msg := v.(jetstream.Msg)
	return msg.Term()
}

// NakMessage negatively acknowledges a message for immediate redelivery.
func (cm *ConsumerManager) NakMessage(jobID string) error {
	v, ok := cm.inflight.LoadAndDelete(jobID)
	if !ok {
		return nil
	}
	msg := v.(jetstream.Msg)
	return msg.Nak()
}

// NakWithDelay negatively acknowledges with a delay before redelivery.
func (cm *ConsumerManager) NakWithDelay(jobID string, delay time.Duration) error {
	v, ok := cm.inflight.LoadAndDelete(jobID)
	if !ok {
		return nil
	}
	msg := v.(jetstream.Msg)
	return msg.NakWithDelay(delay)
}

// InProgress signals that a message is still being processed (extends ack wait).
func (cm *ConsumerManager) InProgress(jobID string) error {
	v, ok := cm.inflight.Load(jobID)
	if !ok {
		return fmt.Errorf("no in-flight message for job %s", jobID)
	}
	msg := v.(jetstream.Msg)
	return msg.InProgress()
}

// RemoveInflight removes an in-flight message without acking.
func (cm *ConsumerManager) RemoveInflight(jobID string) {
	cm.inflight.Delete(jobID)
}
