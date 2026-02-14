package nats

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/openjobspec/ojs-backend-nats/internal/core"
)

// CreateWorkflow creates and starts a workflow.
func (b *NATSBackend) CreateWorkflow(ctx context.Context, req *core.WorkflowRequest) (*core.Workflow, error) {
	now := currentTime()
	wfID := core.NewUUIDv7()

	jobs := req.Jobs
	if req.Type == "chain" {
		jobs = req.Steps
	}

	total := len(jobs)

	wf := &core.Workflow{
		ID:        wfID,
		Name:      req.Name,
		Type:      req.Type,
		State:     "running",
		CreatedAt: core.FormatTime(now),
	}

	if req.Type == "chain" {
		wf.StepsTotal = &total
		zero := 0
		wf.StepsCompleted = &zero
	} else {
		wf.JobsTotal = &total
		zero := 0
		wf.JobsCompleted = &zero
	}

	// Store workflow state
	wfState := workflowState{
		ID:        wfID,
		Name:      req.Name,
		Type:      req.Type,
		State:     "running",
		Total:     total,
		Completed: 0,
		Failed:    0,
		CreatedAt: core.FormatTime(now),
		JobDefs:   jobs,
		Results:   make(map[string]json.RawMessage),
	}
	if req.Callbacks != nil {
		wfState.Callbacks = req.Callbacks
	}

	b.workflows.PutJSON(ctx, wfID, &wfState)

	if req.Type == "chain" {
		// Chain: only enqueue the first step
		step := jobs[0]
		job := b.workflowStepToJob(step, wfID, 0)

		created, err := b.Push(ctx, job)
		if err != nil {
			return nil, err
		}

		wfState.JobIDs = append(wfState.JobIDs, created.ID)
		b.workflows.PutJSON(ctx, wfID, &wfState)
	} else {
		// Group/Batch: enqueue all jobs
		for i, step := range jobs {
			job := b.workflowStepToJob(step, wfID, i)

			created, err := b.Push(ctx, job)
			if err != nil {
				return nil, err
			}

			wfState.JobIDs = append(wfState.JobIDs, created.ID)
		}
		b.workflows.PutJSON(ctx, wfID, &wfState)
	}

	return wf, nil
}

// GetWorkflow retrieves a workflow by ID.
func (b *NATSBackend) GetWorkflow(ctx context.Context, id string) (*core.Workflow, error) {
	var wfState workflowState
	_, err := b.workflows.GetJSON(ctx, id, &wfState)
	if err != nil {
		return nil, core.NewNotFoundError("Workflow", id)
	}

	wf := &core.Workflow{
		ID:          wfState.ID,
		Name:        wfState.Name,
		Type:        wfState.Type,
		State:       wfState.State,
		CreatedAt:   wfState.CreatedAt,
		CompletedAt: wfState.CompletedAt,
	}

	if wf.Type == "chain" {
		wf.StepsTotal = &wfState.Total
		wf.StepsCompleted = &wfState.Completed
	} else {
		wf.JobsTotal = &wfState.Total
		wf.JobsCompleted = &wfState.Completed
	}

	return wf, nil
}

// CancelWorkflow cancels a workflow and its active/pending jobs.
func (b *NATSBackend) CancelWorkflow(ctx context.Context, id string) (*core.Workflow, error) {
	wf, err := b.GetWorkflow(ctx, id)
	if err != nil {
		return nil, err
	}

	if wf.State == "completed" || wf.State == "failed" || wf.State == "cancelled" {
		return nil, core.NewConflictError(
			fmt.Sprintf("Cannot cancel workflow in state '%s'.", wf.State),
			nil,
		)
	}

	// Cancel all jobs belonging to this workflow
	var wfState workflowState
	b.workflows.GetJSON(ctx, id, &wfState)

	for _, jobID := range wfState.JobIDs {
		job, err := b.getJobState(ctx, jobID)
		if err == nil && !core.IsTerminalState(job.State) {
			b.Cancel(ctx, jobID)
		}
	}

	wfState.State = "cancelled"
	wfState.CompletedAt = core.NowFormatted()
	b.workflows.PutJSON(ctx, id, &wfState)

	wf.State = "cancelled"
	wf.CompletedAt = wfState.CompletedAt
	return wf, nil
}

// AdvanceWorkflow is called after ACK or NACK to update workflow state.
func (b *NATSBackend) AdvanceWorkflow(ctx context.Context, workflowID string, jobID string, result json.RawMessage, failed bool) error {
	var wfState workflowState
	_, err := b.workflows.GetJSON(ctx, workflowID, &wfState)
	if err != nil {
		return nil
	}

	if wfState.State != "running" {
		return nil
	}

	// Get the job's workflow step
	job, jobErr := b.getJobState(ctx, jobID)
	stepIdx := 0
	if jobErr == nil {
		stepIdx = job.WorkflowStep
	}

	// Store result for chain result passing
	if result != nil && len(result) > 0 {
		if wfState.Results == nil {
			wfState.Results = make(map[string]json.RawMessage)
		}
		wfState.Results[intToStr(stepIdx)] = result
	}

	if failed {
		wfState.Failed++
	} else {
		wfState.Completed++
	}

	totalFinished := wfState.Completed + wfState.Failed

	if wfState.Type == "chain" {
		if failed {
			wfState.State = "failed"
			wfState.CompletedAt = core.NowFormatted()
			b.workflows.PutJSON(ctx, workflowID, &wfState)
			return nil
		}

		if totalFinished >= wfState.Total {
			wfState.State = "completed"
			wfState.CompletedAt = core.NowFormatted()
			b.workflows.PutJSON(ctx, workflowID, &wfState)
			return nil
		}

		// Enqueue next step
		b.workflows.PutJSON(ctx, workflowID, &wfState)
		return b.enqueueChainStep(ctx, workflowID, &wfState, stepIdx+1)
	}

	// Group/Batch
	b.workflows.PutJSON(ctx, workflowID, &wfState)

	if totalFinished >= wfState.Total {
		finalState := "completed"
		if wfState.Failed > 0 {
			finalState = "failed"
		}
		wfState.State = finalState
		wfState.CompletedAt = core.NowFormatted()
		b.workflows.PutJSON(ctx, workflowID, &wfState)

		if wfState.Type == "batch" {
			b.fireBatchCallbacks(ctx, &wfState, wfState.Failed > 0)
		}
	}

	return nil
}

func (b *NATSBackend) enqueueChainStep(ctx context.Context, workflowID string, wfState *workflowState, stepIdx int) error {
	if stepIdx >= len(wfState.JobDefs) {
		return nil
	}

	step := wfState.JobDefs[stepIdx]

	// Collect parent results
	var parentResults []json.RawMessage
	for i := 0; i < stepIdx; i++ {
		if r, ok := wfState.Results[intToStr(i)]; ok {
			parentResults = append(parentResults, r)
		}
	}

	job := b.workflowStepToJob(step, workflowID, stepIdx)
	job.ParentResults = parentResults

	created, err := b.Push(ctx, job)
	if err != nil {
		return err
	}

	wfState.JobIDs = append(wfState.JobIDs, created.ID)
	b.workflows.PutJSON(ctx, workflowID, wfState)
	return nil
}

func (b *NATSBackend) workflowStepToJob(step core.WorkflowJobRequest, workflowID string, stepIdx int) *core.Job {
	queue := "default"
	if step.Options != nil && step.Options.Queue != "" {
		queue = step.Options.Queue
	}

	job := &core.Job{
		Type:         step.Type,
		Args:         step.Args,
		Queue:        queue,
		WorkflowID:   workflowID,
		WorkflowStep: stepIdx,
	}
	if step.Options != nil && step.Options.RetryPolicy != nil {
		job.Retry = step.Options.RetryPolicy
		job.MaxAttempts = &step.Options.RetryPolicy.MaxAttempts
	} else if step.Options != nil && step.Options.Retry != nil {
		job.Retry = step.Options.Retry
		job.MaxAttempts = &step.Options.Retry.MaxAttempts
	}
	return job
}

func (b *NATSBackend) fireBatchCallbacks(ctx context.Context, wfState *workflowState, hasFailure bool) {
	if wfState.Callbacks == nil {
		return
	}

	if wfState.Callbacks.OnComplete != nil {
		b.fireCallback(ctx, wfState.Callbacks.OnComplete)
	}
	if !hasFailure && wfState.Callbacks.OnSuccess != nil {
		b.fireCallback(ctx, wfState.Callbacks.OnSuccess)
	}
	if hasFailure && wfState.Callbacks.OnFailure != nil {
		b.fireCallback(ctx, wfState.Callbacks.OnFailure)
	}
}

func (b *NATSBackend) fireCallback(ctx context.Context, cb *core.WorkflowCallback) {
	queue := "default"
	if cb.Options != nil && cb.Options.Queue != "" {
		queue = cb.Options.Queue
	}
	b.Push(ctx, &core.Job{
		Type:  cb.Type,
		Args:  cb.Args,
		Queue: queue,
	})
}

func (b *NATSBackend) advanceWorkflow(ctx context.Context, jobID, state string, result []byte) {
	job, err := b.getJobState(ctx, jobID)
	if err != nil || job.WorkflowID == "" {
		return
	}

	failed := state == core.StateDiscarded || state == core.StateCancelled
	b.AdvanceWorkflow(ctx, job.WorkflowID, jobID, json.RawMessage(result), failed)
}
