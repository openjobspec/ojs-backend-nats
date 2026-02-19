package nats

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"

	"github.com/nats-io/nats.go"

	"github.com/openjobspec/ojs-backend-nats/internal/core"
)

const (
	eventSubjectPrefix = "ojs.events."
	eventJobPrefix     = "ojs.events.job."
	eventQueuePrefix   = "ojs.events.queue."
	eventAllSubject    = "ojs.events.all"
)

func eventJobSubject(jobID string) string  { return eventJobPrefix + jobID }
func eventQueueSubject(queue string) string { return eventQueuePrefix + queue }

// PubSubBroker implements core.EventPublisher and core.EventSubscriber
// using NATS core pub/sub.
type PubSubBroker struct {
	nc   *nats.Conn
	mu   sync.Mutex
	subs []*nats.Subscription
	done chan struct{}
	wg   sync.WaitGroup
}

// NewPubSubBroker creates a new PubSubBroker using the given NATS connection.
func NewPubSubBroker(nc *nats.Conn) *PubSubBroker {
	return &PubSubBroker{
		nc:   nc,
		done: make(chan struct{}),
	}
}

// PublishJobEvent publishes a job event to all relevant NATS subjects.
func (b *PubSubBroker) PublishJobEvent(event *core.JobEvent) error {
	data, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("marshal event: %w", err)
	}

	// Publish to job-specific subject
	if err := b.nc.Publish(eventJobSubject(event.JobID), data); err != nil {
		slog.Error("failed to publish job event", "error", err, "job_id", event.JobID)
		return fmt.Errorf("publish event: %w", err)
	}

	// Publish to queue subject
	if event.Queue != "" {
		if err := b.nc.Publish(eventQueueSubject(event.Queue), data); err != nil {
			slog.Error("failed to publish queue event", "error", err, "queue", event.Queue)
		}
	}

	// Publish to global subject
	if err := b.nc.Publish(eventAllSubject, data); err != nil {
		slog.Error("failed to publish global event", "error", err)
	}

	return nil
}

// SubscribeJob subscribes to events for a specific job.
func (b *PubSubBroker) SubscribeJob(jobID string) (<-chan *core.JobEvent, func(), error) {
	return b.subscribe(eventJobSubject(jobID))
}

// SubscribeQueue subscribes to events for all jobs in a queue.
func (b *PubSubBroker) SubscribeQueue(queue string) (<-chan *core.JobEvent, func(), error) {
	return b.subscribe(eventQueueSubject(queue))
}

// SubscribeAll subscribes to all events.
func (b *PubSubBroker) SubscribeAll() (<-chan *core.JobEvent, func(), error) {
	return b.subscribe(eventAllSubject)
}

func (b *PubSubBroker) subscribe(subject string) (<-chan *core.JobEvent, func(), error) {
	ch := make(chan *core.JobEvent, 64)

	sub, err := b.nc.Subscribe(subject, func(msg *nats.Msg) {
		var event core.JobEvent
		if err := json.Unmarshal(msg.Data, &event); err != nil {
			slog.Error("failed to unmarshal event", "error", err)
			return
		}
		select {
		case ch <- &event:
		default:
			slog.Warn("dropping event, subscriber channel full", "subject", subject)
		}
	})
	if err != nil {
		close(ch)
		return nil, nil, fmt.Errorf("subscribe to %s: %w", subject, err)
	}

	b.mu.Lock()
	b.subs = append(b.subs, sub)
	b.mu.Unlock()

	unsubscribe := func() {
		_ = sub.Unsubscribe()
		// Drain remaining messages then close
		close(ch)
	}

	return ch, unsubscribe, nil
}

// Close shuts down the broker and unsubscribes all subscriptions.
func (b *PubSubBroker) Close() error {
	close(b.done)
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, sub := range b.subs {
		_ = sub.Unsubscribe()
	}
	b.subs = nil
	b.wg.Wait()
	return nil
}
