package kv

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

type testCounter struct {
	Value int `json:"value"`
}

func newIntegrationStore(t *testing.T) *Store {
	t.Helper()
	natsURL := os.Getenv("NATS_URL")
	if natsURL == "" {
		natsURL = nats.DefaultURL
	}
	nc, err := nats.Connect(natsURL)
	if err != nil {
		t.Skipf("skipping KV integration test; NATS unavailable at %s: %v", natsURL, err)
	}
	t.Cleanup(nc.Close)

	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatalf("jetstream.New() error = %v", err)
	}
	bucket := fmt.Sprintf("ojs-kv-test-%d", time.Now().UnixNano())
	kv, err := js.CreateKeyValue(context.Background(), jetstream.KeyValueConfig{
		Bucket:  bucket,
		Storage: jetstream.MemoryStorage,
	})
	if err != nil {
		t.Fatalf("CreateKeyValue() error = %v", err)
	}
	t.Cleanup(func() {
		_ = js.DeleteKeyValue(context.Background(), bucket)
	})
	return NewStore(kv)
}

func TestUpdateJSON_ExhaustedConflictNeverFallsBackToPut(t *testing.T) {
	store := newIntegrationStore(t)
	ctx := context.Background()
	if _, err := store.PutJSON(ctx, "counter", &testCounter{}); err != nil {
		t.Fatalf("PutJSON() error = %v", err)
	}

	var target testCounter
	err := store.UpdateJSON(ctx, "counter", &target, func() {
		target.Value++
		if _, putErr := store.PutJSON(ctx, "counter", &testCounter{Value: target.Value + 1000}); putErr != nil {
			t.Errorf("competing PutJSON() error = %v", putErr)
		}
	})
	if err == nil {
		t.Fatal("UpdateJSON() error = nil, want conflict")
	}
	var conflict *ConflictError
	if !errors.As(err, &conflict) || !errors.Is(err, ErrConflict) {
		t.Fatalf("UpdateJSON() error = %T %v, want *ConflictError", err, err)
	}

	var stored testCounter
	if _, err := store.GetJSON(ctx, "counter", &stored); err != nil {
		t.Fatalf("GetJSON() error = %v", err)
	}
	if stored.Value == target.Value {
		t.Fatalf("stored stale target value %d after exhausted conflicts", stored.Value)
	}
}

func TestUpdateJSON_ConcurrentIncrements(t *testing.T) {
	store := newIntegrationStore(t)
	ctx := context.Background()
	if _, err := store.PutJSON(ctx, "counter", &testCounter{}); err != nil {
		t.Fatalf("PutJSON() error = %v", err)
	}

	const goroutines = 16
	const increments = 50
	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < increments; j++ {
				for {
					var value testCounter
					err := store.UpdateJSON(ctx, "counter", &value, func() {
						value.Value++
					})
					if err == nil {
						break
					}
					if !errors.Is(err, ErrConflict) {
						t.Errorf("UpdateJSON() error = %v", err)
						return
					}
				}
			}
		}()
	}
	wg.Wait()

	var stored testCounter
	if _, err := store.GetJSON(ctx, "counter", &stored); err != nil {
		t.Fatalf("GetJSON() error = %v", err)
	}
	if want := goroutines * increments; stored.Value != want {
		t.Fatalf("counter = %d, want %d", stored.Value, want)
	}
}

func TestUpdateJSON_ReturnsContextError(t *testing.T) {
	store := newIntegrationStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	var target testCounter
	err := store.UpdateJSON(ctx, "counter", &target, func() {
		target.Value++
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("UpdateJSON() error = %v, want context.Canceled", err)
	}
}
