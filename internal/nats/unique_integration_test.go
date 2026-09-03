package nats

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/openjobspec/ojs-backend-nats/internal/core"
	"github.com/openjobspec/ojs-backend-nats/internal/kv"
)

func uniqueTestJob(queue, conflict, period string) *core.Job {
	return &core.Job{
		Type:  "unique.process",
		Queue: queue,
		Args:  json.RawMessage(`["same"]`),
		Unique: &core.UniquePolicy{
			Keys:       []string{"type", "queue", "args"},
			OnConflict: conflict,
			Period:     period,
		},
	}
}

func TestUniqueJobs_ConcurrentRejectHasOneOwner(t *testing.T) {
	backend := newIntegrationBackend(t)
	ctx := context.Background()
	queue := "it-unique-reject-" + core.NewUUIDv7()

	const callers = 32
	start := make(chan struct{})
	var wg sync.WaitGroup
	var mu sync.Mutex
	successes := make([]*core.Job, 0, 1)
	duplicates := 0
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			job, err := backend.Push(ctx, uniqueTestJob(queue, "reject", ""))
			mu.Lock()
			defer mu.Unlock()
			if err == nil {
				successes = append(successes, job)
				return
			}
			var ojsErr *core.OJSError
			if errors.As(err, &ojsErr) && ojsErr.Code == core.ErrCodeDuplicate {
				duplicates++
				return
			}
			t.Errorf("Push() error = %v", err)
		}()
	}
	close(start)
	wg.Wait()

	if len(successes) != 1 || duplicates != callers-1 {
		t.Fatalf("successes=%d duplicates=%d, want 1/%d", len(successes), duplicates, callers-1)
	}
	assertUniqueOwner(t, backend, uniqueTestJob(queue, "reject", ""), successes[0].ID)
}

func TestUniqueJobs_ConcurrentIgnoreReturnsOneOwner(t *testing.T) {
	backend := newIntegrationBackend(t)
	ctx := context.Background()
	queue := "it-unique-ignore-" + core.NewUUIDv7()

	const callers = 32
	start := make(chan struct{})
	var wg sync.WaitGroup
	ids := make(chan string, callers)
	errs := make(chan error, callers)
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			job, err := backend.Push(ctx, uniqueTestJob(queue, "ignore", ""))
			if err != nil {
				errs <- err
				return
			}
			ids <- job.ID
		}()
	}
	close(start)
	wg.Wait()
	close(ids)
	close(errs)
	for err := range errs {
		t.Errorf("Push() error = %v", err)
	}

	owners := make(map[string]struct{})
	for id := range ids {
		owners[id] = struct{}{}
	}
	if len(owners) != 1 {
		t.Fatalf("ignore returned %d owners: %v", len(owners), owners)
	}
	var owner string
	for id := range owners {
		owner = id
	}
	assertUniqueOwner(t, backend, uniqueTestJob(queue, "ignore", ""), owner)
}

func TestUniqueJobs_ConcurrentReplaceLeavesOneLiveOwner(t *testing.T) {
	backend := newIntegrationBackend(t)
	ctx := context.Background()
	queue := "it-unique-replace-" + core.NewUUIDv7()

	first, err := backend.Push(ctx, uniqueTestJob(queue, "replace", ""))
	if err != nil {
		t.Fatalf("initial Push() error = %v", err)
	}

	const callers = 24
	start := make(chan struct{})
	var wg sync.WaitGroup
	var mu sync.Mutex
	resultIDs := []string{first.ID}
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			job, err := backend.Push(ctx, uniqueTestJob(queue, "replace", ""))
			if err != nil {
				var ojsErr *core.OJSError
				if errors.As(err, &ojsErr) && ojsErr.Code == core.ErrCodeDuplicate {
					return
				}
				t.Errorf("Push() error = %v", err)
				return
			}
			mu.Lock()
			resultIDs = append(resultIDs, job.ID)
			mu.Unlock()
		}()
	}
	close(start)
	wg.Wait()

	template := uniqueTestJob(queue, "replace", "")
	claim, _, err := backend.unique.GetClaim(ctx, kv.ComputeFingerprint(template))
	if err != nil {
		t.Fatalf("GetClaim() error = %v", err)
	}
	live := 0
	for _, id := range resultIDs {
		job, infoErr := backend.Info(ctx, id)
		if infoErr == nil && !core.IsTerminalState(job.State) {
			live++
			if id != claim.JobID {
				t.Errorf("live job %s is not claim owner %s", id, claim.JobID)
			}
		}
	}
	if live != 1 {
		t.Fatalf("replace left %d live jobs, want 1 (claim=%s results=%v)", live, claim.JobID, resultIDs)
	}
}

func TestUniqueJobs_PeriodExpiryUsesExistingCreationTime(t *testing.T) {
	backend := newIntegrationBackend(t)
	ctx := context.Background()
	queue := "it-unique-period-" + core.NewUUIDv7()

	first, err := backend.Push(ctx, uniqueTestJob(queue, "reject", "PT0.05S"))
	if err != nil {
		t.Fatalf("first Push() error = %v", err)
	}
	time.Sleep(80 * time.Millisecond)
	second, err := backend.Push(ctx, uniqueTestJob(queue, "reject", "PT0.05S"))
	if err != nil {
		t.Fatalf("second Push() after period error = %v", err)
	}
	if second.ID == first.ID {
		t.Fatal("period expiry returned the original job")
	}
	assertUniqueOwner(t, backend, uniqueTestJob(queue, "reject", "PT0.05S"), second.ID)
}

func TestUniqueJobs_ReplaceActiveJobSafely(t *testing.T) {
	backend := newIntegrationBackend(t)
	ctx := context.Background()
	queue := "it-unique-active-" + core.NewUUIDv7()

	first, err := backend.Push(ctx, uniqueTestJob(queue, "replace", ""))
	if err != nil {
		t.Fatalf("first Push() error = %v", err)
	}
	fetched, err := backend.Fetch(ctx, []string{queue}, 1, "worker-unique", 5000)
	if err != nil || len(fetched) != 1 {
		t.Fatalf("Fetch() error=%v jobs=%d", err, len(fetched))
	}

	replacement, err := backend.Push(ctx, uniqueTestJob(queue, "replace", ""))
	if err != nil {
		t.Fatalf("replacement Push() error = %v", err)
	}
	old, err := backend.Info(ctx, first.ID)
	if err != nil {
		t.Fatalf("Info(old) error = %v", err)
	}
	if old.State != core.StateCancelled {
		t.Fatalf("active replaced job state = %q, want cancelled", old.State)
	}
	current, err := backend.Info(ctx, replacement.ID)
	if err != nil {
		t.Fatalf("Info(replacement) error = %v", err)
	}
	if current.State != core.StateAvailable {
		t.Fatalf("replacement state = %q, want available", current.State)
	}
	assertUniqueOwner(t, backend, uniqueTestJob(queue, "replace", ""), replacement.ID)
}

func assertUniqueOwner(t *testing.T, backend *NATSBackend, template *core.Job, want string) {
	t.Helper()
	claim, _, err := backend.unique.GetClaim(context.Background(), kv.ComputeFingerprint(template))
	if err != nil {
		t.Fatalf("GetClaim() error = %v", err)
	}
	if claim.JobID != want {
		t.Fatalf("unique owner = %q, want %q", claim.JobID, want)
	}
}
