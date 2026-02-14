package nats

import (
	"context"
	"fmt"
	"time"

	"github.com/nats-io/nats.go/jetstream"
)

// SetupJetStream creates the OJS JetStream stream and KV buckets.
func SetupJetStream(ctx context.Context, js jetstream.JetStream) error {
	// Create the main OJS stream for job messages.
	// Uses subject-based filtering: all OJS messages share one stream.
	_, err := js.CreateOrUpdateStream(ctx, jetstream.StreamConfig{
		Name:     StreamName,
		Subjects: []string{QueueAllSubject(), DeadAllSubject(), EventsAllSubject()},
		Storage:  jetstream.FileStorage,
		Retention: jetstream.WorkQueuePolicy,
		MaxAge:   24 * time.Hour, // Messages older than 24h are purged
		Discard:  jetstream.DiscardOld,
	})
	if err != nil {
		return fmt.Errorf("creating stream %s: %w", StreamName, err)
	}

	// Create KV buckets
	buckets := []struct {
		name string
		ttl  time.Duration
	}{
		{BucketJobs, 0},
		{BucketUnique, 0},       // Per-key TTL set on put
		{BucketCron, 0},
		{BucketWorkers, 5 * time.Minute},
		{BucketWorkflows, 0},
		{BucketQueues, 0},
		{BucketScheduled, 0},
		{BucketRetry, 0},
		{BucketDead, 0},
		{BucketActive, 0},
		{BucketStats, 0},
	}

	for _, b := range buckets {
		cfg := jetstream.KeyValueConfig{
			Bucket:  b.name,
			Storage: jetstream.FileStorage,
		}
		if b.ttl > 0 {
			cfg.TTL = b.ttl
		}
		if _, err := js.CreateOrUpdateKeyValue(ctx, cfg); err != nil {
			return fmt.Errorf("creating KV bucket %s: %w", b.name, err)
		}
	}

	return nil
}

// EnsureConsumer creates or updates a pull consumer for a queue.
func EnsureConsumer(ctx context.Context, js jetstream.JetStream, queue string) (jetstream.Consumer, error) {
	consumer, err := js.CreateOrUpdateConsumer(ctx, StreamName, jetstream.ConsumerConfig{
		Durable:       ConsumerName(queue),
		FilterSubject: QueueJobsSubject(queue),
		AckPolicy:     jetstream.AckExplicitPolicy,
		AckWait:       60 * time.Second,
		MaxDeliver:    1,  // We handle retries ourselves via KV state
		DeliverPolicy: jetstream.DeliverAllPolicy,
	})
	if err != nil {
		return nil, fmt.Errorf("creating consumer for queue %s: %w", queue, err)
	}
	return consumer, nil
}
