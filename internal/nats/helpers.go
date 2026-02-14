package nats

import (
	"context"
	"encoding/json"
	"regexp"
	"strconv"
	"sync"
	"time"

	"github.com/openjobspec/ojs-backend-nats/internal/core"
)

var regexCache sync.Map // pattern string -> *regexp.Regexp

func matchesPattern(s, pattern string) bool {
	key := "^" + pattern + "$"
	if cached, ok := regexCache.Load(key); ok {
		return cached.(*regexp.Regexp).MatchString(s)
	}
	re, err := regexp.Compile(key)
	if err != nil {
		return s == pattern
	}
	regexCache.Store(key, re)
	return re.MatchString(s)
}

func currentTime() time.Time {
	return time.Now()
}

func unmarshalJSON(data []byte, v any) error {
	return json.Unmarshal(data, v)
}

func (b *NATSBackend) getJobState(ctx context.Context, jobID string) (*core.Job, error) {
	data, _, err := b.jobs.Get(ctx, jobID)
	if err != nil {
		return nil, err
	}
	return unmarshalJobState(data)
}

func (b *NATSBackend) putJobState(ctx context.Context, job *core.Job) (uint64, error) {
	data, err := marshalJobState(job)
	if err != nil {
		return 0, err
	}
	return b.jobs.Put(ctx, job.ID, data)
}

func (b *NATSBackend) ensureQueue(ctx context.Context, name string) {
	if !b.queues.Exists(ctx, name) {
		meta := queueMeta{Name: name, Paused: false}
		b.queues.PutJSON(ctx, name, &meta)
	}
}

func (b *NATSBackend) isQueuePaused(ctx context.Context, name string) bool {
	var meta queueMeta
	_, err := b.queues.GetJSON(ctx, name, &meta)
	if err != nil {
		return false
	}
	return meta.Paused
}

func (b *NATSBackend) isRateLimited(ctx context.Context, queue string, now time.Time) bool {
	var meta queueMeta
	_, err := b.queues.GetJSON(ctx, queue, &meta)
	if err != nil || meta.RateLimitPerSec <= 0 {
		return false
	}

	windowMs := int64(1000 / meta.RateLimitPerSec)
	nowMs := now.UnixMilli()
	return nowMs-meta.LastFetchMs < windowMs
}

func (b *NATSBackend) updateQueueRateLimit(ctx context.Context, queue string, maxPerSec int) {
	var meta queueMeta
	b.queues.UpdateJSON(ctx, queue, &meta, func() {
		meta.Name = queue
		meta.RateLimitPerSec = maxPerSec
	})
}

func (b *NATSBackend) recordFetchTime(ctx context.Context, queue string, now time.Time) {
	var meta queueMeta
	b.queues.UpdateJSON(ctx, queue, &meta, func() {
		meta.LastFetchMs = now.UnixMilli()
	})
}

func (b *NATSBackend) incrementCompleted(ctx context.Context, queue string) {
	key := kvStatsKey(queue, "completed")
	for i := 0; i < 3; i++ {
		data, rev, err := b.stats.Get(ctx, key)
		if err != nil {
			b.stats.Create(ctx, key, []byte("1"))
			return
		}
		count, _ := strconv.Atoi(string(data))
		count++
		_, uErr := b.stats.Update(ctx, key, []byte(strconv.Itoa(count)), rev)
		if uErr == nil {
			return
		}
	}
}
