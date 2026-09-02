package kv

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/nats-io/nats.go/jetstream"
)

const updateJSONAttempts = 8

// ErrConflict identifies an exhausted optimistic-concurrency operation.
var ErrConflict = errors.New("kv revision conflict")

// ConflictError reports a key that could not be updated within the bounded
// compare-and-swap retry budget.
type ConflictError struct {
	Key      string
	Attempts int
	Err      error
}

func (e *ConflictError) Error() string {
	return fmt.Sprintf("update key %s after %d attempts: %v", e.Key, e.Attempts, e.Err)
}

func (e *ConflictError) Unwrap() error {
	return e.Err
}

func (e *ConflictError) Is(target error) bool {
	return target == ErrConflict || errors.Is(e.Err, target)
}

// Store provides typed access to a NATS KV bucket.
type Store struct {
	kv jetstream.KeyValue
}

// Entry is the latest observed revision of a KV key, including delete markers.
type Entry struct {
	Key       string
	Value     []byte
	Revision  uint64
	CreatedAt time.Time
	Operation jetstream.KeyValueOp
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

// DeleteRevision removes key only if revision is still current.
func (s *Store) DeleteRevision(ctx context.Context, key string, revision uint64) error {
	return s.kv.Delete(ctx, key, jetstream.LastRevision(revision))
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

// KeysFiltered returns current keys matching one or more NATS subject filters.
func (s *Store) KeysFiltered(ctx context.Context, filters ...string) ([]string, error) {
	lister, err := s.kv.ListKeysFiltered(ctx, filters...)
	if err != nil {
		if err == jetstream.ErrNoKeysFound {
			return nil, nil
		}
		return nil, err
	}
	defer func() {
		_ = lister.Stop()
	}()

	var keys []string
	for key := range lister.Keys() {
		keys = append(keys, key)
	}
	return keys, nil
}

// EntriesFiltered returns the latest entry for keys matching a NATS subject
// filter. Delete markers are included so callers can clean historical data.
func (s *Store) EntriesFiltered(ctx context.Context, filter string) ([]Entry, error) {
	watcher, err := s.kv.Watch(ctx, filter)
	if err != nil {
		if err == jetstream.ErrNoKeysFound {
			return nil, nil
		}
		return nil, err
	}
	defer func() {
		_ = watcher.Stop()
	}()

	var entries []Entry
	for entry := range watcher.Updates() {
		if entry == nil {
			break
		}
		entries = append(entries, Entry{
			Key:       entry.Key(),
			Value:     append([]byte(nil), entry.Value()...),
			Revision:  entry.Revision(),
			CreatedAt: entry.Created(),
			Operation: entry.Operation(),
		})
	}
	return entries, nil
}

// EntriesFilteredFrom returns at most limit matching revisions beginning at a
// stream revision. The next revision and exhausted flag support bounded scans.
func (s *Store) EntriesFilteredFrom(
	ctx context.Context,
	filter string,
	startRevision uint64,
	limit int,
) ([]Entry, uint64, bool, error) {
	watcher, err := s.kv.Watch(ctx, filter, jetstream.ResumeFromRevision(startRevision))
	if err != nil {
		if err == jetstream.ErrNoKeysFound {
			return nil, startRevision, true, nil
		}
		return nil, startRevision, false, err
	}
	defer func() {
		_ = watcher.Stop()
	}()

	entries := make([]Entry, 0, limit)
	nextRevision := startRevision
	for entry := range watcher.Updates() {
		if entry == nil {
			return entries, nextRevision, true, nil
		}
		entries = append(entries, Entry{
			Key:       entry.Key(),
			Value:     append([]byte(nil), entry.Value()...),
			Revision:  entry.Revision(),
			CreatedAt: entry.Created(),
			Operation: entry.Operation(),
		})
		nextRevision = entry.Revision() + 1
		if limit > 0 && len(entries) >= limit {
			return entries, nextRevision, false, nil
		}
	}
	return entries, nextRevision, true, nil
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
// Retries a bounded number of times on revision conflicts.
func (s *Store) UpdateJSON(ctx context.Context, key string, target any, mutate func()) error {
	var lastConflict error
	for i := 0; i < updateJSONAttempts; i++ {
		if err := ctx.Err(); err != nil {
			return err
		}

		rev, err := s.GetJSON(ctx, key, target)
		if err != nil {
			if !errors.Is(err, jetstream.ErrKeyNotFound) && !errors.Is(err, jetstream.ErrKeyDeleted) {
				return err
			}

			// Key doesn't exist yet — initialize via mutate and create.
			mutate()
			data, mErr := json.Marshal(target)
			if mErr != nil {
				return fmt.Errorf("marshal key %s: %w", key, mErr)
			}
			_, cErr := s.Create(ctx, key, data)
			if cErr == nil {
				return nil
			}
			if !errors.Is(cErr, jetstream.ErrKeyExists) {
				return cErr
			}
			lastConflict = cErr
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
		if !errors.Is(uErr, jetstream.ErrKeyExists) {
			return uErr
		}
		lastConflict = uErr
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return &ConflictError{
		Key:      key,
		Attempts: updateJSONAttempts,
		Err:      lastConflict,
	}
}

// Exists checks if a key exists.
func (s *Store) Exists(ctx context.Context, key string) bool {
	_, err := s.kv.Get(ctx, key)
	return err == nil
}
