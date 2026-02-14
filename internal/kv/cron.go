package kv

import (
	"context"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/openjobspec/ojs-backend-nats/internal/core"
)

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
	var cron core.CronJob
	_, err := c.store.GetJSON(ctx, name, &cron)
	if err != nil {
		return nil, err
	}
	return &cron, nil
}

// Delete removes a cron job registration.
func (c *CronStore) Delete(ctx context.Context, name string) error {
	return c.store.Delete(ctx, name)
}

// List returns all registered cron jobs.
func (c *CronStore) List(ctx context.Context) ([]*core.CronJob, error) {
	keys, err := c.store.Keys(ctx)
	if err != nil {
		return nil, err
	}

	var crons []*core.CronJob
	for _, key := range keys {
		cron, err := c.Get(ctx, key)
		if err != nil {
			continue
		}
		crons = append(crons, cron)
	}
	return crons, nil
}
