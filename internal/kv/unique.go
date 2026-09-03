package kv

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/openjobspec/ojs-backend-nats/internal/core"
)

// UniqueClaim is the revisioned owner record stored for a fingerprint.
type UniqueClaim struct {
	JobID     string `json:"job_id"`
	ClaimedAt string `json:"claimed_at,omitempty"`
}

// UniqueStore manages unique job locks via NATS KV.
type UniqueStore struct {
	store *Store
}

// NewUniqueStore creates a new UniqueStore.
func NewUniqueStore(kv jetstream.KeyValue) *UniqueStore {
	return &UniqueStore{store: NewStore(kv)}
}

// CreateClaim acquires an unowned fingerprint.
func (u *UniqueStore) CreateClaim(ctx context.Context, fingerprint, jobID string, now time.Time) (uint64, error) {
	data, err := marshalUniqueClaim(UniqueClaim{JobID: jobID, ClaimedAt: core.FormatTime(now)})
	if err != nil {
		return 0, err
	}
	return u.store.Create(ctx, fingerprint, data)
}

// GetClaim returns the current fingerprint owner and KV revision.
func (u *UniqueStore) GetClaim(ctx context.Context, fingerprint string) (UniqueClaim, uint64, error) {
	data, revision, err := u.store.Get(ctx, fingerprint)
	if err != nil {
		return UniqueClaim{}, 0, err
	}
	claim, err := unmarshalUniqueClaim(data)
	return claim, revision, err
}

// ReplaceClaim transfers ownership only if expectedRevision is still current.
func (u *UniqueStore) ReplaceClaim(
	ctx context.Context,
	fingerprint string,
	claim UniqueClaim,
	expectedRevision uint64,
) (uint64, error) {
	data, err := marshalUniqueClaim(claim)
	if err != nil {
		return 0, err
	}
	return u.store.Update(ctx, fingerprint, data, expectedRevision)
}

// ReleaseClaim removes a claim only if expectedRevision is still current.
func (u *UniqueStore) ReleaseClaim(ctx context.Context, fingerprint string, expectedRevision uint64) error {
	return u.store.DeleteRevision(ctx, fingerprint, expectedRevision)
}

func marshalUniqueClaim(claim UniqueClaim) ([]byte, error) {
	return json.Marshal(claim)
}

func unmarshalUniqueClaim(data []byte) (UniqueClaim, error) {
	var claim UniqueClaim
	if err := json.Unmarshal(data, &claim); err == nil && claim.JobID != "" {
		return claim, nil
	}
	// Backward compatibility for existing buckets that stored only the job ID.
	if len(data) == 0 {
		return UniqueClaim{}, fmt.Errorf("empty unique claim")
	}
	return UniqueClaim{JobID: string(data)}, nil
}

// ComputeFingerprint computes a unique fingerprint for a job based on its unique policy.
func ComputeFingerprint(job *core.Job) string {
	h := sha256.New()
	keys := append([]string(nil), job.Unique.Keys...)
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
