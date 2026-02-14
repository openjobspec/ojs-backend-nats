package scheduler

import (
	"context"
	"log/slog"
	"sync"
	"time"

	natsbackend "github.com/openjobspec/ojs-backend-nats/internal/nats"
)

// Scheduler runs background tasks for the OJS server.
type Scheduler struct {
	backend  *natsbackend.NATSBackend
	stop     chan struct{}
	stopOnce sync.Once
}

// New creates a new Scheduler.
func New(backend *natsbackend.NATSBackend) *Scheduler {
	return &Scheduler{
		backend: backend,
		stop:    make(chan struct{}),
	}
}

// Start begins all background scheduling goroutines.
func (s *Scheduler) Start() {
	go s.runLoop("scheduled-promoter", 1*time.Second, s.backend.PromoteScheduled)
	go s.runLoop("retry-promoter", 200*time.Millisecond, s.backend.PromoteRetries)
	go s.runLoop("stalled-reaper", 500*time.Millisecond, s.backend.RequeueStalled)
	go s.runLoop("cron-scheduler", 10*time.Second, s.backend.FireCronJobs)
}

// Stop signals all background goroutines to stop.
func (s *Scheduler) Stop() {
	s.stopOnce.Do(func() {
		close(s.stop)
	})
}

func (s *Scheduler) runLoop(name string, interval time.Duration, fn func(context.Context) error) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-s.stop:
			return
		case <-ticker.C:
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			if err := fn(ctx); err != nil {
				slog.Error("scheduler loop error", "scheduler", name, "error", err)
			}
			cancel()
		}
	}
}
