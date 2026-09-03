package nats

import (
	"errors"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/openjobspec/ojs-backend-nats/internal/core"
)

func newTestNATSConn(t *testing.T) *nats.Conn {
	t.Helper()
	natsURL := os.Getenv("NATS_URL")
	if natsURL == "" {
		natsURL = "nats://localhost:4222"
	}
	nc, err := nats.Connect(natsURL)
	if err != nil {
		t.Skipf("skipping pub/sub test; NATS unavailable at %s: %v", natsURL, err)
	}
	t.Cleanup(nc.Close)
	return nc
}

// TestPubSubBroker_DeliversEvents verifies a subscribed channel receives a
// published event.
func TestPubSubBroker_DeliversEvents(t *testing.T) {
	broker := NewPubSubBroker(newTestNATSConn(t))
	defer broker.Close()

	ch, unsub, err := broker.SubscribeAll()
	if err != nil {
		t.Fatalf("SubscribeAll() error = %v", err)
	}
	defer unsub()

	if err := broker.PublishJobEvent(&core.JobEvent{JobID: "job-1", EventType: "job.completed"}); err != nil {
		t.Fatalf("PublishJobEvent() error = %v", err)
	}

	select {
	case got, ok := <-ch:
		if !ok {
			t.Fatal("channel closed before delivering event")
		}
		if got.JobID != "job-1" {
			t.Fatalf("got JobID %q, want %q", got.JobID, "job-1")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for event")
	}
}

// TestPubSubBroker_UnsubscribeThenPublish ensures publishing after unsubscribe
// does not panic (regression: send on closed channel) and the channel is closed.
func TestPubSubBroker_UnsubscribeThenPublish(t *testing.T) {
	broker := NewPubSubBroker(newTestNATSConn(t))
	defer broker.Close()

	ch, unsub, err := broker.SubscribeAll()
	if err != nil {
		t.Fatalf("SubscribeAll() error = %v", err)
	}

	unsub()

	// Publishing after unsubscribe must not panic.
	for i := 0; i < 50; i++ {
		_ = broker.PublishJobEvent(&core.JobEvent{JobID: "job-x", EventType: "job.completed"})
	}
	time.Sleep(100 * time.Millisecond)

	// Draining a closed channel eventually yields ok == false.
	deadline := time.After(2 * time.Second)
	for {
		select {
		case _, ok := <-ch:
			if !ok {
				return // closed as expected
			}
		case <-deadline:
			t.Fatal("channel was not closed after unsubscribe")
		}
	}
}

// TestPubSubBroker_ConcurrentPublishUnsubscribe stresses the delivery/close
// coordination under the race detector.
func TestPubSubBroker_ConcurrentPublishUnsubscribe(t *testing.T) {
	broker := NewPubSubBroker(newTestNATSConn(t))
	defer broker.Close()

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		ch, unsub, err := broker.SubscribeAll()
		if err != nil {
			t.Fatalf("SubscribeAll() error = %v", err)
		}

		wg.Add(2)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				_ = broker.PublishJobEvent(&core.JobEvent{JobID: "job-c", EventType: "job.completed"})
			}
		}()
		go func() {
			defer wg.Done()
			// Drain until closed.
			for range ch {
			}
		}()

		time.Sleep(5 * time.Millisecond)
		unsub()
	}
	wg.Wait()
}

func TestPubSubBroker_SubscribeRacingClose(t *testing.T) {
	broker := NewPubSubBroker(newTestNATSConn(t))

	var (
		wg            sync.WaitGroup
		subscriptions atomic.Int64
	)
	start := make(chan struct{})
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			ch, unsub, err := broker.SubscribeAll()
			if err != nil {
				if !errors.Is(err, ErrPubSubBrokerClosed) {
					t.Errorf("SubscribeAll() error = %v, want broker closed", err)
				}
				return
			}
			subscriptions.Add(1)
			unsub()
			for range ch {
			}
		}()
	}

	close(start)
	if err := broker.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	wg.Wait()

	if _, _, err := broker.SubscribeAll(); !errors.Is(err, ErrPubSubBrokerClosed) {
		t.Fatalf("SubscribeAll() after Close error = %v, want %v", err, ErrPubSubBrokerClosed)
	}
	if err := broker.PublishJobEvent(&core.JobEvent{}); !errors.Is(err, ErrPubSubBrokerClosed) {
		t.Fatalf("PublishJobEvent() after Close error = %v, want %v", err, ErrPubSubBrokerClosed)
	}

	broker.mu.Lock()
	remaining := len(broker.subs)
	broker.mu.Unlock()
	if remaining != 0 {
		t.Fatalf("%d subscriptions survived broker shutdown (%d were created)", remaining, subscriptions.Load())
	}
}

func TestPubSubBroker_UnsubscribeIsBrokerOwned(t *testing.T) {
	broker := NewPubSubBroker(newTestNATSConn(t))
	defer broker.Close()

	ch, unsubscribe, err := broker.SubscribeAll()
	if err != nil {
		t.Fatalf("SubscribeAll() error = %v", err)
	}
	unsubscribe()
	unsubscribe()

	if _, ok := <-ch; ok {
		t.Fatal("subscription channel remained open after unsubscribe")
	}
	broker.mu.Lock()
	remaining := len(broker.subs)
	broker.mu.Unlock()
	if remaining != 0 {
		t.Fatalf("broker retained %d subscriptions after unsubscribe", remaining)
	}
}
