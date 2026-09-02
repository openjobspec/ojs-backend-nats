package nats

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/openjobspec/ojs-backend-nats/internal/core"
	"github.com/openjobspec/ojs-backend-nats/internal/kv"
)

func TestQueueMetadataCAS_PauseResumeRateLimitRaces(t *testing.T) {
	backend := newIntegrationBackend(t)
	ctx := context.Background()
	queue := "it-queue-cas-" + core.NewUUIDv7()
	if err := backend.ensureQueue(ctx, queue); err != nil {
		t.Fatalf("ensureQueue() error = %v", err)
	}

	const goroutines = 24
	const iterations = 30
	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			for iteration := 0; iteration < iterations; iteration++ {
				var operation func() error
				switch i % 4 {
				case 0:
					operation = func() error { return backend.PauseQueue(ctx, queue) }
				case 1:
					operation = func() error { return backend.ResumeQueue(ctx, queue) }
				case 2:
					value := (i + iteration) % 100
					operation = func() error { return backend.updateQueueRateLimit(ctx, queue, value+1) }
				default:
					now := time.Now().Add(time.Duration(iteration) * time.Millisecond)
					operation = func() error { return backend.recordFetchTime(ctx, queue, now) }
				}
				if err := retryKVConflict(operation); err != nil {
					t.Errorf("queue metadata operation error = %v", err)
					return
				}
			}
		}()
	}
	wg.Wait()

	// Apply independent final writes concurrently; a stale unconditional Put
	// would lose one of these fields.
	finalTime := time.Now().Add(time.Second).Truncate(time.Millisecond)
	operations := []func() error{
		func() error { return backend.PauseQueue(ctx, queue) },
		func() error { return backend.updateQueueRateLimit(ctx, queue, 99) },
		func() error { return backend.recordFetchTime(ctx, queue, finalTime) },
	}
	for _, operation := range operations {
		operation := operation
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := retryKVConflict(operation); err != nil {
				t.Errorf("final queue metadata operation error = %v", err)
			}
		}()
	}
	wg.Wait()

	var meta queueMeta
	if _, err := backend.queues.GetJSON(ctx, queue, &meta); err != nil {
		t.Fatalf("queues.GetJSON() error = %v", err)
	}
	if meta.Name != queue || !meta.Paused || meta.RateLimitPerSec != 99 ||
		meta.LastFetchMs != finalTime.UnixMilli() {
		t.Fatalf("queue metadata lost a concurrent field: %+v", meta)
	}
}

func retryKVConflict(operation func() error) error {
	for attempt := 0; attempt < 64; attempt++ {
		err := operation()
		if err == nil {
			return nil
		}
		if !errors.Is(err, kv.ErrConflict) {
			return err
		}
	}
	return &kv.ConflictError{Key: "queue", Attempts: 64, Err: kv.ErrConflict}
}
