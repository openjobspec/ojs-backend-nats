package kv

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/nats-io/nats.go/jetstream"
)

// Store provides typed access to a NATS KV bucket.
type Store struct {
	kv jetstream.KeyValue
}

// NewStore wraps a NATS KV bucket.
func NewStore(kv jetstream.KeyValue) *Store {
	return &Store{kv: kv}
}

// Get retrieves a value by key.
func (s *Store) Get(ctx context.Context, key string) ([]byte, uint64, error) {
	entry, err := s.kv.Get(ctx, key)
	if err != nil {
		return nil, 0, err
	}
	return entry.Value(), entry.Revision(), nil
}

// Put stores a value at key.
func (s *Store) Put(ctx context.Context, key string, value []byte) (uint64, error) {
	return s.kv.Put(ctx, key, value)
}

// Create stores a value at key only if it doesn't already exist.
// Returns jetstream.ErrKeyExists if the key already exists.
func (s *Store) Create(ctx context.Context, key string, value []byte) (uint64, error) {
	return s.kv.Create(ctx, key, value)
}

// Update stores a value at key only if the revision matches.
func (s *Store) Update(ctx context.Context, key string, value []byte, revision uint64) (uint64, error) {
	return s.kv.Update(ctx, key, value, revision)
}

// Delete removes a key.
func (s *Store) Delete(ctx context.Context, key string) error {
	return s.kv.Delete(ctx, key)
}

// Keys returns all keys in the bucket.
func (s *Store) Keys(ctx context.Context) ([]string, error) {
	keys, err := s.kv.Keys(ctx)
	if err != nil {
		// If no keys exist, NATS returns an error
		if err == jetstream.ErrNoKeysFound {
			return nil, nil
		}
		return nil, err
	}
	return keys, nil
}

// GetJSON retrieves and unmarshals a JSON value.
func (s *Store) GetJSON(ctx context.Context, key string, v any) (uint64, error) {
	data, rev, err := s.Get(ctx, key)
	if err != nil {
		return 0, err
	}
	if err := json.Unmarshal(data, v); err != nil {
		return 0, fmt.Errorf("unmarshal key %s: %w", key, err)
	}
	return rev, nil
}

// PutJSON marshals and stores a JSON value.
func (s *Store) PutJSON(ctx context.Context, key string, v any) (uint64, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return 0, fmt.Errorf("marshal key %s: %w", key, err)
	}
	return s.Put(ctx, key, data)
}

// UpdateJSON performs a CAS (compare-and-swap) update on a JSON value.
// The mutate function receives the current value and should modify it in place.
// Retries up to 3 times on revision conflicts.
func (s *Store) UpdateJSON(ctx context.Context, key string, target any, mutate func()) error {
	for i := 0; i < 3; i++ {
		rev, err := s.GetJSON(ctx, key, target)
		if err != nil {
			// Key doesn't exist yet — initialize via mutate and create
			mutate()
			data, mErr := json.Marshal(target)
			if mErr != nil {
				return fmt.Errorf("marshal key %s: %w", key, mErr)
			}
			_, cErr := s.Create(ctx, key, data)
			if cErr == nil {
				return nil
			}
			// Key was created concurrently — retry
			continue
		}

		mutate()
		data, mErr := json.Marshal(target)
		if mErr != nil {
			return fmt.Errorf("marshal key %s: %w", key, mErr)
		}
		_, uErr := s.Update(ctx, key, data, rev)
		if uErr == nil {
			return nil
		}
		// Revision conflict — retry
	}
	// Fall back to unconditional put after retries exhausted
	return s.putJSON(ctx, key, target)
}

func (s *Store) putJSON(ctx context.Context, key string, v any) error {
	_, err := s.PutJSON(ctx, key, v)
	return err
}

// Exists checks if a key exists.
func (s *Store) Exists(ctx context.Context, key string) bool {
	_, err := s.kv.Get(ctx, key)
	return err == nil
}
