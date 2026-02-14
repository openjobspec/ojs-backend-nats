package kv

import (
	"context"
	"crypto/sha256"
	"fmt"
	"sort"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/openjobspec/ojs-backend-nats/internal/core"
)

// UniqueStore manages unique job locks via NATS KV.
type UniqueStore struct {
	store *Store
}

// NewUniqueStore creates a new UniqueStore.
func NewUniqueStore(kv jetstream.KeyValue) *UniqueStore {
	return &UniqueStore{store: NewStore(kv)}
}

// CheckAndSet attempts to acquire a unique lock for a job.
// Returns the existing job ID if the lock is held, empty string if acquired.
func (u *UniqueStore) CheckAndSet(ctx context.Context, fingerprint, jobID string) (string, error) {
	// Try to create (fails if key exists)
	_, err := u.store.Create(ctx, fingerprint, []byte(jobID))
	if err != nil {
		if err == jetstream.ErrKeyExists {
			// Key exists - read the existing value
			data, _, getErr := u.store.Get(ctx, fingerprint)
			if getErr != nil {
				return "", getErr
			}
			return string(data), nil
		}
		return "", err
	}
	return "", nil // Lock acquired
}

// Release removes a unique lock.
func (u *UniqueStore) Release(ctx context.Context, fingerprint string) error {
	return u.store.Delete(ctx, fingerprint)
}

// ComputeFingerprint computes a unique fingerprint for a job based on its unique policy.
func ComputeFingerprint(job *core.Job) string {
	h := sha256.New()
	keys := job.Unique.Keys
	if len(keys) == 0 {
		keys = []string{"type", "args"}
	}
	sort.Strings(keys)
	for _, key := range keys {
		switch key {
		case "type":
			h.Write([]byte("type:"))
			h.Write([]byte(job.Type))
		case "args":
			h.Write([]byte("args:"))
			if job.Args != nil {
				h.Write(job.Args)
			}
		case "queue":
			h.Write([]byte("queue:"))
			h.Write([]byte(job.Queue))
		}
	}
	return fmt.Sprintf("%x", h.Sum(nil))
}
