package nats

import (
	"context"
	"fmt"
	"log/slog"
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

// FetchedMessage keeps the durable JetStream source coupled to its job ID
// until the job-state CAS decides whether this exact delivery should be kept.
type FetchedMessage struct {
	JobID string
	Msg   jetstream.Msg
}

type inflightDelivery struct {
	msg         jetstream.Msg
	dispatchSeq uint64
}

// NewConsumerManager creates a new ConsumerManager.
func NewConsumerManager(js jetstream.JetStream) *ConsumerManager {
	return &ConsumerManager{js: js}
}

// GetConsumer returns the pull consumer for a queue, creating it if needed.
func (cm *ConsumerManager) GetConsumer(ctx context.Context, queue string) (jetstream.Consumer, error) {
	if c, ok := cm.consumers.Load(queue); ok {
		consumer, valid := c.(jetstream.Consumer)
		if !valid {
			return nil, fmt.Errorf("cached consumer for queue %q has unexpected type", queue)
		}
		return consumer, nil
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
func (cm *ConsumerManager) FetchMessages(ctx context.Context, queue string, count int) ([]FetchedMessage, error) {
	consumer, err := cm.GetConsumer(ctx, queue)
	if err != nil {
		return nil, err
	}

	msgs, err := consumer.Fetch(count, jetstream.FetchMaxWait(100*time.Millisecond))
	if err != nil {
		// Timeout or no messages is not an error
		return nil, nil
	}

	var fetched []FetchedMessage
	for msg := range msgs.Messages() {
		jobID := string(msg.Data())
		if jobID == "" {
			_ = msg.DoubleAck(ctx)
			continue
		}
		fetched = append(fetched, FetchedMessage{JobID: jobID, Msg: msg})
	}

	if err := msgs.Error(); err != nil {
		slog.Warn("nats consumer returned partial results", "error", err)
	}

	return fetched, nil
}

// Track associates the CAS-winning delivery with its active job.
func (cm *ConsumerManager) Track(jobID string, msg jetstream.Msg, dispatchSeq uint64) {
	cm.inflight.Store(jobID, inflightDelivery{msg: msg, dispatchSeq: dispatchSeq})
}

// AckFetched durably acknowledges an exact delivery that never became active.
func (cm *ConsumerManager) AckFetched(ctx context.Context, fetched FetchedMessage) error {
	return fetched.Msg.DoubleAck(ctx)
}

// NakFetched releases an exact delivery for redelivery after a transient
// storage failure prevented a state decision.
func (cm *ConsumerManager) NakFetched(fetched FetchedMessage) error {
	return fetched.Msg.Nak()
}

// AckMessage acknowledges the JetStream message for a job.
func (cm *ConsumerManager) AckMessage(ctx context.Context, jobID string, dispatchSeq uint64) error {
	v, ok := cm.inflight.Load(jobID)
	if !ok {
		// Message not tracked (server restart or already acked)
		return nil
	}
	delivery, valid := v.(inflightDelivery)
	if !valid {
		return fmt.Errorf("tracked delivery for job %q has unexpected type", jobID)
	}
	if delivery.dispatchSeq != dispatchSeq {
		return nil
	}
	if err := delivery.msg.DoubleAck(ctx); err != nil {
		return err
	}
	cm.inflight.CompareAndDelete(jobID, v)
	return nil
}

// TermMessage terminates the JetStream message (will not be redelivered).
func (cm *ConsumerManager) TermMessage(jobID string) error {
	v, ok := cm.inflight.LoadAndDelete(jobID)
	if !ok {
		return nil
	}
	delivery, err := checkedDelivery(v, jobID)
	if err != nil {
		return err
	}
	return delivery.msg.Term()
}

// NakMessage negatively acknowledges a message for immediate redelivery.
func (cm *ConsumerManager) NakMessage(jobID string) error {
	v, ok := cm.inflight.LoadAndDelete(jobID)
	if !ok {
		return nil
	}
	delivery, err := checkedDelivery(v, jobID)
	if err != nil {
		return err
	}
	return delivery.msg.Nak()
}

// NakWithDelay negatively acknowledges with a delay before redelivery.
func (cm *ConsumerManager) NakWithDelay(jobID string, delay time.Duration) error {
	v, ok := cm.inflight.LoadAndDelete(jobID)
	if !ok {
		return nil
	}
	delivery, err := checkedDelivery(v, jobID)
	if err != nil {
		return err
	}
	return delivery.msg.NakWithDelay(delay)
}

// InProgress signals that a message is still being processed (extends ack wait).
func (cm *ConsumerManager) InProgress(jobID string) error {
	v, ok := cm.inflight.Load(jobID)
	if !ok {
		return fmt.Errorf("no in-flight message for job %s", jobID)
	}
	delivery, err := checkedDelivery(v, jobID)
	if err != nil {
		return err
	}
	return delivery.msg.InProgress()
}

func checkedDelivery(value any, jobID string) (inflightDelivery, error) {
	delivery, ok := value.(inflightDelivery)
	if !ok {
		return inflightDelivery{}, fmt.Errorf("tracked delivery for job %q has unexpected type", jobID)
	}
	return delivery, nil
}

// RemoveInflight removes an in-flight message without acking.
func (cm *ConsumerManager) RemoveInflight(jobID string) {
	cm.inflight.Delete(jobID)
}
