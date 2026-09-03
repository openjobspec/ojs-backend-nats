package nats

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/openjobspec/ojs-backend-nats/internal/core"
)

func TestJobCAS_ConcurrentTerminalOperationsHaveOneWinner(t *testing.T) {
	backend := newIntegrationBackend(t)
	ctx := context.Background()
	queue := "it-cas-terminal-" + core.NewUUIDv7()
	created, err := backend.Push(ctx, &core.Job{Type: "cas.run", Queue: queue, Args: json.RawMessage(`[]`)})
	if err != nil {
		t.Fatalf("Push() error = %v", err)
	}
	fetched, err := backend.Fetch(ctx, []string{queue}, 1, "worker-cas", 5000)
	if err != nil || len(fetched) != 1 {
		t.Fatalf("Fetch() error=%v jobs=%d", err, len(fetched))
	}

	const callers = 96
	start := make(chan struct{})
	var wg sync.WaitGroup
	var winners atomic.Int64
	var conflicts atomic.Int64
	nonRetryable := false
	for i := 0; i < callers; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			var opErr error
			switch i % 3 {
			case 0:
				_, opErr = backend.Ack(ctx, created.ID, json.RawMessage(`{"ok":true}`))
			case 1:
				_, opErr = backend.Nack(ctx, created.ID, &core.JobError{
					Message:   "fatal",
					Retryable: &nonRetryable,
				}, false)
			default:
				_, opErr = backend.Cancel(ctx, created.ID)
			}
			if opErr == nil {
				winners.Add(1)
				return
			}
			var ojsErr *core.OJSError
			if errors.As(opErr, &ojsErr) && ojsErr.Code == core.ErrCodeConflict {
				conflicts.Add(1)
				return
			}
			t.Errorf("terminal operation error = %T %v", opErr, opErr)
		}()
	}
	close(start)
	wg.Wait()

	if winners.Load() != 1 || conflicts.Load() != callers-1 {
		t.Fatalf("winners=%d conflicts=%d, want 1/%d", winners.Load(), conflicts.Load(), callers-1)
	}
	job, err := backend.Info(ctx, created.ID)
	if err != nil {
		t.Fatalf("Info() error = %v", err)
	}
	if !core.IsTerminalState(job.State) {
		t.Fatalf("final state = %q, want terminal", job.State)
	}
}

func TestJobCAS_ConcurrentAckAdvancesChainOnce(t *testing.T) {
	backend := newIntegrationBackend(t)
	ctx := context.Background()
	queue := "it-cas-chain-" + core.NewUUIDv7()
	workflow, err := backend.CreateWorkflow(ctx, &core.WorkflowRequest{
		Type: "chain",
		Steps: []core.WorkflowJobRequest{
			{Name: "one", Type: "chain.one", Args: json.RawMessage(`[]`), Options: &core.EnqueueOptions{Queue: queue}},
			{Name: "two", Type: "chain.two", Args: json.RawMessage(`[]`), Options: &core.EnqueueOptions{Queue: queue}},
		},
	})
	if err != nil {
		t.Fatalf("CreateWorkflow() error = %v", err)
	}
	first := fetchJobEventually(t, backend, queue, "chain-worker")

	const callers = 64
	start := make(chan struct{})
	var wg sync.WaitGroup
	var successes atomic.Int64
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if _, err := backend.Ack(ctx, first.ID, nil); err == nil {
				successes.Add(1)
			}
		}()
	}
	close(start)
	wg.Wait()
	if successes.Load() != 1 {
		t.Fatalf("Ack successes = %d, want 1", successes.Load())
	}

	second := fetchJobEventually(t, backend, queue, "chain-worker")
	if second.Type != "chain.two" {
		t.Fatalf("second chain type = %q, want chain.two", second.Type)
	}
	var state workflowState
	if _, err := backend.workflows.GetJSON(ctx, workflow.ID, &state); err != nil {
		t.Fatalf("workflow state read error = %v", err)
	}
	if len(state.JobIDs) != 2 {
		t.Fatalf("chain JobIDs = %v, want exactly two steps", state.JobIDs)
	}

	data, _, err := backend.stats.Get(ctx, kvStatsKey(queue, "completed"))
	if err != nil {
		t.Fatalf("completed counter read error = %v", err)
	}
	if completed, _ := strconv.Atoi(string(data)); completed != 1 {
		t.Fatalf("completed metric = %d, want 1 before second ACK", completed)
	}
}

func TestJobCAS_ConcurrentFetchClaimsOneDelivery(t *testing.T) {
	backend := newIntegrationBackend(t)
	backend2 := newIntegrationBackend(t)
	ctx := context.Background()
	queue := "it-cas-fetch-" + core.NewUUIDv7()
	created, err := backend.Push(ctx, &core.Job{Type: "fetch.once", Queue: queue, Args: json.RawMessage(`[]`)})
	if err != nil {
		t.Fatalf("Push() error = %v", err)
	}
	for seq := uint64(100); seq < 120; seq++ {
		if err := PublishJobDispatch(ctx, backend.js, queue, created.ID, seq); err != nil {
			t.Fatalf("duplicate PublishJobDispatch() error = %v", err)
		}
	}

	const callers = 32
	start := make(chan struct{})
	var wg sync.WaitGroup
	var claimed atomic.Int64
	for i := 0; i < callers; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			selected := backend
			if i%2 == 1 {
				selected = backend2
			}
			jobs, err := selected.Fetch(ctx, []string{queue}, 1, "fetch-worker", 5000)
			if err != nil {
				t.Errorf("Fetch() error = %v", err)
				return
			}
			claimed.Add(int64(len(jobs)))
		}()
	}
	close(start)
	wg.Wait()
	if claimed.Load() != 1 {
		t.Fatalf("concurrent Fetch claimed %d jobs, want 1", claimed.Load())
	}
	if _, err := backend.Cancel(ctx, created.ID); err != nil {
		t.Fatalf("Cancel() error = %v", err)
	}
}

func TestWorkflowCAS_ConcurrentGroupCompletionsAreNotLost(t *testing.T) {
	backend := newIntegrationBackend(t)
	ctx := context.Background()
	queue := "it-cas-group-" + core.NewUUIDv7()
	const size = 16
	jobs := make([]core.WorkflowJobRequest, 0, size)
	for i := 0; i < size; i++ {
		jobs = append(jobs, core.WorkflowJobRequest{
			Name:    strconv.Itoa(i),
			Type:    "group.run",
			Args:    json.RawMessage(`[]`),
			Options: &core.EnqueueOptions{Queue: queue},
		})
	}
	workflow, err := backend.CreateWorkflow(ctx, &core.WorkflowRequest{Type: "group", Jobs: jobs})
	if err != nil {
		t.Fatalf("CreateWorkflow() error = %v", err)
	}

	fetched := make([]*core.Job, 0, size)
	deadline := time.Now().Add(3 * time.Second)
	for len(fetched) < size && time.Now().Before(deadline) {
		batch, err := backend.Fetch(ctx, []string{queue}, size-len(fetched), "group-worker", 5000)
		if err != nil {
			t.Fatalf("Fetch() error = %v", err)
		}
		fetched = append(fetched, batch...)
	}
	if len(fetched) != size {
		t.Fatalf("fetched %d group jobs, want %d", len(fetched), size)
	}

	var wg sync.WaitGroup
	for _, job := range fetched {
		job := job
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := backend.Ack(ctx, job.ID, nil); err != nil {
				t.Errorf("Ack(%s) error = %v", job.ID, err)
			}
		}()
	}
	wg.Wait()

	got, err := backend.GetWorkflow(ctx, workflow.ID)
	if err != nil {
		t.Fatalf("GetWorkflow() error = %v", err)
	}
	if got.State != "completed" || got.JobsCompleted == nil || *got.JobsCompleted != size {
		t.Fatalf("workflow state/completed = %q/%v, want completed/%d", got.State, got.JobsCompleted, size)
	}
}

func fetchJobEventually(t *testing.T, backend *NATSBackend, queue, worker string) *core.Job {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		jobs, err := backend.Fetch(context.Background(), []string{queue}, 1, worker, 5000)
		if err != nil {
			t.Fatalf("Fetch() error = %v", err)
		}
		if len(jobs) == 1 {
			return jobs[0]
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out fetching from %s", queue)
	return nil
}
