package nats

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"

	"github.com/nats-io/nats.go"

	"github.com/openjobspec/ojs-backend-nats/internal/core"
)

// ErrPubSubBrokerClosed is returned when work is attempted after shutdown.
var ErrPubSubBrokerClosed = errors.New("pub/sub broker is closed")

const (
	eventSubjectPrefix = "ojs.events."
	eventJobPrefix     = "ojs.events.job."
	eventQueuePrefix   = "ojs.events.queue."
	eventAllSubject    = "ojs.events.all"
)

func eventJobSubject(jobID string) string   { return eventJobPrefix + jobID }
func eventQueueSubject(queue string) string { return eventQueuePrefix + queue }

// PubSubBroker implements core.EventPublisher and core.EventSubscriber
// using NATS core pub/sub.
type PubSubBroker struct {
	nc     *nats.Conn
	mu     sync.Mutex
	subs   map[*subscription]struct{}
	closed bool
}

// subscription couples a NATS subscription with its delivery channel and
// serializes delivery and shutdown so a message callback can never send on a
// channel that has already been closed.
type subscription struct {
	sub     *nats.Subscription
	subject string
	ch      chan *core.JobEvent

	mu     sync.Mutex
	closed bool
}

// deliver forwards an event to the subscriber channel unless the subscription
// is closed. It never blocks: a full channel drops the event.
func (s *subscription) deliver(event *core.JobEvent) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	select {
	case s.ch <- event:
	default:
		slog.Warn("dropping event, subscriber channel full", "subject", s.subject)
	}
}

// close stops the NATS subscription and closes the delivery channel exactly
// once. Unsubscribe is called before taking the lock so no new callbacks start;
// any in-flight deliver completes under the lock before the channel is closed.
func (s *subscription) close() {
	if s.sub != nil {
		_ = s.sub.Unsubscribe()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.closed = true
	close(s.ch)
}

// NewPubSubBroker creates a new PubSubBroker using the given NATS connection.
func NewPubSubBroker(nc *nats.Conn) *PubSubBroker {
	return &PubSubBroker{
		nc:   nc,
		subs: make(map[*subscription]struct{}),
	}
}

// PublishJobEvent publishes a job event to all relevant NATS subjects.
func (b *PubSubBroker) PublishJobEvent(event *core.JobEvent) error {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return ErrPubSubBrokerClosed
	}
	b.mu.Unlock()

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
	s := &subscription{
		subject: subject,
		ch:      make(chan *core.JobEvent, 64),
	}

	// Holding the broker lock through NATS registration makes Subscribe and
	// Close linearizable: the subscription is either fully broker-owned or it
	// is rejected before any NATS resource is created.
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		close(s.ch)
		return nil, nil, ErrPubSubBrokerClosed
	}

	sub, err := b.nc.Subscribe(subject, func(msg *nats.Msg) {
		var event core.JobEvent
		if err := json.Unmarshal(msg.Data, &event); err != nil {
			slog.Error("failed to unmarshal event", "error", err, "subject", subject)
			return
		}
		s.deliver(&event)
	})
	if err != nil {
		b.mu.Unlock()
		close(s.ch)
		return nil, nil, fmt.Errorf("subscribe to %s: %w", subject, err)
	}
	s.sub = sub
	b.subs[s] = struct{}{}
	b.mu.Unlock()

	return s.ch, func() {
		b.unsubscribe(s)
	}, nil
}

// unsubscribe atomically removes broker ownership and closes the subscription.
func (b *PubSubBroker) unsubscribe(s *subscription) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.subs[s]; !ok {
		return
	}
	delete(b.subs, s)
	s.close()
}

// Close shuts down the broker and unsubscribes all subscriptions.
func (b *PubSubBroker) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return nil
	}
	b.closed = true
	for s := range b.subs {
		delete(b.subs, s)
		s.close()
	}
	return nil
}
