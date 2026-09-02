package kv

import (
	"context"
	"encoding/json"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/openjobspec/ojs-backend-nats/internal/core"
)

// CronEntry couples a cron registration with its KV revision.
type CronEntry struct {
	Cron     *core.CronJob
	Revision uint64
}

// CronStore manages cron job registrations via NATS KV.
type CronStore struct {
	store *Store
}

// NewCronStore creates a new CronStore.
func NewCronStore(kv jetstream.KeyValue) *CronStore {
	return &CronStore{store: NewStore(kv)}
}

// Register stores a cron job registration.
func (c *CronStore) Register(ctx context.Context, cron *core.CronJob) error {
	_, err := c.store.PutJSON(ctx, cron.Name, cron)
	return err
}

// Get retrieves a cron job by name.
func (c *CronStore) Get(ctx context.Context, name string) (*core.CronJob, error) {
	entry, err := c.GetEntry(ctx, name)
	if err != nil {
		return nil, err
	}
	return entry.Cron, nil
}

// GetEntry retrieves a cron registration and its current KV revision.
func (c *CronStore) GetEntry(ctx context.Context, name string) (*CronEntry, error) {
	var cron core.CronJob
	revision, err := c.store.GetJSON(ctx, name, &cron)
	if err != nil {
		return nil, err
	}
	return &CronEntry{Cron: &cron, Revision: revision}, nil
}

// Update conditionally stores a cron registration.
func (c *CronStore) Update(ctx context.Context, cron *core.CronJob, revision uint64) (uint64, error) {
	data, err := json.Marshal(cron)
	if err != nil {
		return 0, err
	}
	return c.store.Update(ctx, cron.Name, data, revision)
}

// Delete removes a cron job registration.
func (c *CronStore) Delete(ctx context.Context, name string) error {
	return c.store.Delete(ctx, name)
}

// DeleteRevision removes a cron registration only if revision is current.
func (c *CronStore) DeleteRevision(ctx context.Context, name string, revision uint64) error {
	return c.store.DeleteRevision(ctx, name, revision)
}

// List returns all registered cron jobs.
func (c *CronStore) List(ctx context.Context) ([]*core.CronJob, error) {
	entries, err := c.ListEntries(ctx)
	if err != nil {
		return nil, err
	}
	crons := make([]*core.CronJob, 0, len(entries))
	for _, entry := range entries {
		crons = append(crons, entry.Cron)
	}
	return crons, nil
}

// ListEntries returns registrations with the revisions used for cursor CAS.
func (c *CronStore) ListEntries(ctx context.Context) ([]CronEntry, error) {
	keys, err := c.store.Keys(ctx)
	if err != nil {
		return nil, err
	}

	var entries []CronEntry
	for _, key := range keys {
		entry, err := c.GetEntry(ctx, key)
		if err != nil {
			continue
		}
		entries = append(entries, *entry)
	}
	return entries, nil
}
