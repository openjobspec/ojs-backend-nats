package nats

import (
	"context"
	"encoding/json"
	"sort"

	"github.com/openjobspec/ojs-backend-nats/internal/core"
)

// ListJobs returns a paginated, filtered list of jobs.
func (b *NATSBackend) ListJobs(ctx context.Context, filters core.JobListFilters, limit, offset int) ([]*core.Job, int, error) {
	keys, err := b.jobs.Keys(ctx)
	if err != nil {
		return nil, 0, err
	}

	var filtered []*core.Job
	for _, key := range keys {
		job, err := b.getJobState(ctx, key)
		if err != nil {
			continue
		}
		if filters.State != "" && job.State != filters.State {
			continue
		}
		if filters.Queue != "" && job.Queue != filters.Queue {
			continue
		}
		if filters.Type != "" && job.Type != filters.Type {
			continue
		}
		filtered = append(filtered, job)
	}

	total := len(filtered)

	// Sort by created_at descending
	sort.Slice(filtered, func(i, j int) bool {
		return filtered[i].CreatedAt > filtered[j].CreatedAt
	})

	// Apply pagination
	if offset >= len(filtered) {
		return []*core.Job{}, total, nil
	}
	end := offset + limit
	if end > len(filtered) {
		end = len(filtered)
	}

	return filtered[offset:end], total, nil
}

// ListWorkers returns a paginated list of workers and summary counts.
func (b *NATSBackend) ListWorkers(ctx context.Context, limit, offset int) ([]*core.WorkerInfo, core.WorkerSummary, error) {
	keys, err := b.workers.Keys(ctx)
	if err != nil {
		return nil, core.WorkerSummary{}, err
	}

	var workers []*core.WorkerInfo
	for _, key := range keys {
		data, _, err := b.workers.Get(ctx, key)
		if err != nil {
			continue
		}
		var info map[string]any
		if json.Unmarshal(data, &info) != nil {
			continue
		}

		w := &core.WorkerInfo{
			ID:    key,
			State: "running",
		}
		if d, ok := info["directive"].(string); ok && d != "" {
			w.Directive = d
			if d == "quiet" {
				w.State = "quiet"
			}
		}
		if aj, ok := info["active_jobs"].(float64); ok {
			w.ActiveJobs = int(aj)
		}
		if lh, ok := info["last_heartbeat"].(string); ok {
			w.LastHeartbeat = lh
		}
		workers = append(workers, w)
	}

	summary := core.WorkerSummary{Total: len(workers)}
	for _, w := range workers {
		switch w.State {
		case "running":
			summary.Running++
		case "quiet":
			summary.Quiet++
		default:
			summary.Stale++
		}
	}

	if offset >= len(workers) {
		return []*core.WorkerInfo{}, summary, nil
	}
	end := offset + limit
	if end > len(workers) {
		end = len(workers)
	}

	return workers[offset:end], summary, nil
}
