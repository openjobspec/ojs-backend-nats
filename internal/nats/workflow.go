package nats

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/openjobspec/ojs-backend-nats/internal/core"
)

// CreateWorkflow creates and starts a workflow.
func (b *NATSBackend) CreateWorkflow(ctx context.Context, req *core.WorkflowRequest) (*core.Workflow, error) {
	jobs, err := validateWorkflowRequest(req)
	if err != nil {
		return nil, err
	}

	now := currentTime()
	wfID := core.NewUUIDv7()

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
		ID:           wfID,
		Name:         req.Name,
		Type:         req.Type,
		State:        "running",
		Total:        total,
		Completed:    0,
		Failed:       0,
		CreatedAt:    core.FormatTime(now),
		JobDefs:      jobs,
		Results:      make(map[string]json.RawMessage),
		FinishedJobs: make(map[string]bool),
	}
	if req.Callbacks != nil {
		wfState.Callbacks = req.Callbacks
	}

	workflowData, err := json.Marshal(&wfState)
	if err != nil {
		return nil, core.NewInternalError(fmt.Sprintf("encoding workflow state: %v", err))
	}
	if _, err := b.workflows.Create(ctx, wfID, workflowData); err != nil {
		return nil, core.NewInternalError(fmt.Sprintf("storing workflow state: %v", err))
	}

	if req.Type == "chain" {
		// Chain: only enqueue the first step
		step := jobs[0]
		job := b.workflowStepToJob(step, wfID, 0)

		created, err := b.Push(ctx, job)
		if err != nil {
			return nil, err
		}

		if err := b.appendWorkflowJobID(ctx, wfID, created.ID); err != nil {
			return nil, err
		}
	} else {
		// Group/Batch: enqueue all jobs
		for i, step := range jobs {
			job := b.workflowStepToJob(step, wfID, i)

			created, err := b.Push(ctx, job)
			if err != nil {
				return nil, err
			}

			if err := b.appendWorkflowJobID(ctx, wfID, created.ID); err != nil {
				return nil, err
			}
		}
	}

	return wf, nil
}

func validateWorkflowRequest(req *core.WorkflowRequest) ([]core.WorkflowJobRequest, error) {
	if req == nil {
		return nil, core.NewInvalidRequestError("A workflow request is required.", nil)
	}

	var jobs []core.WorkflowJobRequest
	switch req.Type {
	case "chain":
		if len(req.Jobs) != 0 {
			return nil, core.NewInvalidRequestError(
				"A chain workflow must use 'steps', not 'jobs'.",
				map[string]any{"field": "jobs", "workflow_type": req.Type},
			)
		}
		jobs = req.Steps
	case "group", "batch":
		if len(req.Steps) != 0 {
			return nil, core.NewInvalidRequestError(
				"A group or batch workflow must use 'jobs', not 'steps'.",
				map[string]any{"field": "steps", "workflow_type": req.Type},
			)
		}
		jobs = req.Jobs
	default:
		return nil, core.NewInvalidRequestError(
			"Invalid workflow type. Must be 'chain', 'group', or 'batch'.",
			map[string]any{"field": "type", "received": req.Type},
		)
	}

	if len(jobs) == 0 {
		field := "jobs"
		if req.Type == "chain" {
			field = "steps"
		}
		return nil, core.NewInvalidRequestError(
			fmt.Sprintf("The '%s' field must contain at least one job.", field),
			map[string]any{"field": field, "validation": "non_empty"},
		)
	}

	for i := range jobs {
		if err := core.ValidateEnqueueRequest(&core.EnqueueRequest{
			Type:    jobs[i].Type,
			Args:    jobs[i].Args,
			Options: jobs[i].Options,
		}); err != nil {
			if err.Details == nil {
				err.Details = make(map[string]any)
			}
			err.Details["workflow_job_index"] = i
			return nil, err
		}
	}
	return jobs, nil
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
	var wfState workflowState
	updated := false
	for attempt := 0; attempt < 64; attempt++ {
		revision, err := b.workflows.GetJSON(ctx, id, &wfState)
		if err != nil {
			return nil, core.NewNotFoundError("Workflow", id)
		}
		if wfState.State == "completed" || wfState.State == "failed" || wfState.State == "cancelled" {
			return nil, core.NewConflictError(
				fmt.Sprintf("Cannot cancel workflow in state '%s'.", wfState.State),
				nil,
			)
		}
		wfState.State = "cancelled"
		wfState.CompletedAt = core.NowFormatted()
		data, err := json.Marshal(&wfState)
		if err != nil {
			return nil, err
		}
		if _, err := b.workflows.Update(ctx, id, data, revision); err == nil {
			updated = true
			break
		} else if !errors.Is(err, jetstream.ErrKeyExists) {
			return nil, err
		}
	}
	if !updated {
		return nil, core.NewConflictError("Workflow state changed concurrently.", map[string]any{"workflow_id": id})
	}

	// The workflow CAS winner is the only caller that cancels member jobs.
	for _, jobID := range wfState.JobIDs {
		job, err := b.getJobState(ctx, jobID)
		if err == nil && !core.IsTerminalState(job.State) {
			if _, err := b.Cancel(ctx, jobID); err != nil {
				return nil, err
			}
		}
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

// AdvanceWorkflow is called after ACK or NACK to update workflow state.
func (b *NATSBackend) AdvanceWorkflow(ctx context.Context, workflowID string, jobID string, result json.RawMessage, failed bool) error {
	// Get the job's workflow step
	job, jobErr := b.getJobState(ctx, jobID)
	stepIdx := 0
	if jobErr == nil {
		stepIdx = job.WorkflowStep
	}

	for attempt := 0; attempt < 64; attempt++ {
		var wfState workflowState
		revision, err := b.workflows.GetJSON(ctx, workflowID, &wfState)
		if err != nil || wfState.State != "running" {
			return nil
		}
		if wfState.FinishedJobs == nil {
			wfState.FinishedJobs = make(map[string]bool)
		}
		if wfState.FinishedJobs[jobID] {
			return nil
		}
		wfState.FinishedJobs[jobID] = true

		if len(result) > 0 {
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
		nextStep := -1
		fireCallbacks := false
		if wfState.Type == "chain" {
			switch {
			case failed:
				wfState.State = "failed"
				wfState.CompletedAt = core.NowFormatted()
			case totalFinished >= wfState.Total:
				wfState.State = "completed"
				wfState.CompletedAt = core.NowFormatted()
			default:
				nextStep = stepIdx + 1
			}
		} else if totalFinished >= wfState.Total {
			wfState.State = "completed"
			if wfState.Failed > 0 {
				wfState.State = "failed"
			}
			wfState.CompletedAt = core.NowFormatted()
			fireCallbacks = wfState.Type == "batch"
		}

		data, err := json.Marshal(&wfState)
		if err != nil {
			return err
		}
		if _, err := b.workflows.Update(ctx, workflowID, data, revision); err != nil {
			if errors.Is(err, jetstream.ErrKeyExists) {
				continue
			}
			return err
		}

		if nextStep >= 0 {
			return b.enqueueChainStep(ctx, workflowID, &wfState, nextStep)
		}
		if fireCallbacks {
			b.fireBatchCallbacks(ctx, &wfState, wfState.Failed > 0)
		}
		return nil
	}
	return core.NewConflictError(
		"Workflow state changed concurrently; advancement will be retried.",
		map[string]any{"workflow_id": workflowID, "job_id": jobID},
	)
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

	return b.appendWorkflowJobID(ctx, workflowID, created.ID)
}

func (b *NATSBackend) appendWorkflowJobID(ctx context.Context, workflowID, jobID string) error {
	for attempt := 0; attempt < 8; attempt++ {
		var current workflowState
		revision, err := b.workflows.GetJSON(ctx, workflowID, &current)
		if err != nil {
			return err
		}
		for _, existingID := range current.JobIDs {
			if existingID == jobID {
				return nil
			}
		}
		current.JobIDs = append(current.JobIDs, jobID)
		data, err := json.Marshal(&current)
		if err != nil {
			return err
		}
		if _, err := b.workflows.Update(ctx, workflowID, data, revision); err == nil {
			return nil
		} else if !errors.Is(err, jetstream.ErrKeyExists) {
			return err
		}
	}
	return core.NewConflictError(
		"Workflow job list changed concurrently.",
		map[string]any{"workflow_id": workflowID, "job_id": jobID},
	)
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
	if step.Options != nil {
		job.Priority = step.Options.Priority
		job.TimeoutMs = step.Options.TimeoutMs
		job.ScheduledAt = step.Options.ScheduledAt
		if job.ScheduledAt == "" {
			job.ScheduledAt = step.Options.DelayUntil
		}
		job.ExpiresAt = step.Options.ExpiresAt
		job.Unique = step.Options.Unique
		job.Tags = append([]string(nil), step.Options.Tags...)
		job.VisibilityTimeoutMs = step.Options.VisibilityTimeoutMs
		job.Meta = append(json.RawMessage(nil), step.Options.Metadata...)
		job.RateLimit = step.Options.RateLimit
		if step.Options.RetryPolicy != nil {
			job.Retry = step.Options.RetryPolicy
			job.MaxAttempts = &step.Options.RetryPolicy.MaxAttempts
		} else if step.Options.Retry != nil {
			job.Retry = step.Options.Retry
			job.MaxAttempts = &step.Options.Retry.MaxAttempts
		}
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
	if _, err := b.Push(ctx, &core.Job{
		Type:  cb.Type,
		Args:  cb.Args,
		Queue: queue,
	}); err != nil {
		slog.Error("workflow: error firing callback", "type", cb.Type, "error", err)
	}
}

func (b *NATSBackend) advanceWorkflow(ctx context.Context, jobID, state string, result []byte) {
	job, err := b.getJobState(ctx, jobID)
	if err != nil || job.WorkflowID == "" {
		return
	}

	failed := state == core.StateDiscarded || state == core.StateCancelled
	if err := b.AdvanceWorkflow(ctx, job.WorkflowID, jobID, json.RawMessage(result), failed); err != nil {
		slog.Error("workflow: failed to advance workflow",
			"workflow_id", job.WorkflowID,
			"job_id", jobID,
			"error", err,
		)
	}
}
