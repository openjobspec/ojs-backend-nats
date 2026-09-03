package grpc

import (
	"encoding/json"
	"fmt"
	"math"
	"time"

	"github.com/openjobspec/ojs-backend-nats/internal/core"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/structpb"

	ojsv1 "github.com/openjobspec/ojs-proto/gen/go/ojs/v1"
)

// stateToProto maps core state strings to proto enum values.
var stateToProto = map[string]ojsv1.JobState{
	"scheduled": ojsv1.JobState_JOB_STATE_SCHEDULED,
	"available": ojsv1.JobState_JOB_STATE_AVAILABLE,
	"pending":   ojsv1.JobState_JOB_STATE_PENDING,
	"active":    ojsv1.JobState_JOB_STATE_ACTIVE,
	"completed": ojsv1.JobState_JOB_STATE_COMPLETED,
	"retryable": ojsv1.JobState_JOB_STATE_RETRYABLE,
	"cancelled": ojsv1.JobState_JOB_STATE_CANCELLED,
	"discarded": ojsv1.JobState_JOB_STATE_DISCARDED,
}

// jobToProto converts a core.Job to its protobuf representation.
func jobToProto(j *core.Job) *ojsv1.Job {
	if j == nil {
		return nil
	}

	pj := &ojsv1.Job{
		Id:      j.ID,
		Type:    j.Type,
		Queue:   j.Queue,
		State:   stateToProto[j.State],
		Attempt: int32(j.Attempt),
	}

	if j.Priority != nil {
		pj.Priority = int32(*j.Priority)
	}
	if j.MaxAttempts != nil {
		pj.MaxAttempts = int32(*j.MaxAttempts)
	}
	if j.TimeoutMs != nil {
		pj.Timeout = durationpb.New(time.Duration(*j.TimeoutMs) * time.Millisecond)
	}
	if j.VisibilityTimeoutMs != nil {
		pj.VisibilityTimeout = durationpb.New(time.Duration(*j.VisibilityTimeoutMs) * time.Millisecond)
	}
	pj.Tags = append([]string(nil), j.Tags...)

	if j.Args != nil {
		var args []any
		if err := json.Unmarshal(j.Args, &args); err == nil {
			for _, a := range args {
				if v, err := structpb.NewValue(a); err == nil {
					pj.Args = append(pj.Args, v)
				}
			}
		}
	}

	if j.Meta != nil {
		var meta map[string]any
		if err := json.Unmarshal(j.Meta, &meta); err == nil {
			if traceID, ok := meta["trace_id"].(string); ok {
				pj.TraceId = traceID
			}
			if s, err := structpb.NewStruct(meta); err == nil {
				pj.Meta = s
			}
		}
	}

	if j.Result != nil {
		var result map[string]any
		if err := json.Unmarshal(j.Result, &result); err == nil {
			if s, err := structpb.NewStruct(result); err == nil {
				pj.Result = s
			}
		}
	}

	pj.CreatedAt = parseRFC3339(j.CreatedAt)
	pj.EnqueuedAt = parseRFC3339(j.EnqueuedAt)
	pj.ScheduledAt = parseRFC3339(j.ScheduledAt)
	pj.StartedAt = parseRFC3339(j.StartedAt)
	pj.CompletedAt = parseRFC3339(j.CompletedAt)
	pj.ExpiresAt = parseRFC3339(j.ExpiresAt)

	if j.Retry != nil {
		pj.RetryPolicy = &ojsv1.RetryPolicy{
			MaxAttempts:        int32(j.Retry.MaxAttempts),
			BackoffCoefficient: j.Retry.BackoffCoefficient,
			Jitter:             j.Retry.Jitter,
			NonRetryableErrors: append([]string(nil), j.Retry.NonRetryableErrors...),
			OnExhaustion:       j.Retry.OnExhaustion,
		}
		if d, err := parseCoreDuration(j.Retry.InitialInterval); err == nil {
			pj.RetryPolicy.InitialInterval = durationpb.New(d)
		}
		if d, err := parseCoreDuration(j.Retry.MaxInterval); err == nil {
			pj.RetryPolicy.MaxInterval = durationpb.New(d)
		}
	}

	if j.Unique != nil {
		pj.UniquePolicy = &ojsv1.UniquePolicy{
			Key: j.Unique.Keys,
		}
		if d, err := parseCoreDuration(j.Unique.Period); err == nil {
			pj.UniquePolicy.Period = durationpb.New(d)
		}
		pj.UniquePolicy.OnConflict = uniqueConflictToProto(j.Unique.OnConflict)
		for _, state := range j.Unique.States {
			if protoState, ok := stateToProto[state]; ok {
				pj.UniquePolicy.States = append(pj.UniquePolicy.States, protoState)
			}
		}
	}

	return pj
}

// enqueueRequestToJob converts an EnqueueRequest to a core.Job.
func enqueueRequestToJob(req *ojsv1.EnqueueRequest) (*core.Job, error) {
	if req == nil {
		return nil, core.NewInvalidRequestError("enqueue request is required", nil)
	}
	return enqueueFieldsToJob(req.Type, req.Args, req.Options, time.Now())
}

// enqueueJobRequestToJob converts a batch job entry to a core.Job.
func enqueueJobRequestToJob(req *ojsv1.BatchJobEntry, defaults *ojsv1.EnqueueOptions, now time.Time) (*core.Job, error) {
	if req == nil {
		return nil, core.NewInvalidRequestError("batch job entry is required", nil)
	}
	return enqueueFieldsToJob(req.Type, req.Args, mergeEnqueueOptions(defaults, req.Options), now)
}

func enqueueFieldsToJob(jobType string, values []*structpb.Value, opts *ojsv1.EnqueueOptions, now time.Time) (*core.Job, error) {
	args, err := marshalProtoValues(values)
	if err != nil {
		return nil, core.NewInvalidRequestError("invalid job arguments", map[string]any{"error": err.Error()})
	}

	job := &core.Job{
		Type:  jobType,
		Args:  args,
		Queue: "default",
	}
	if err := applyEnqueueOptions(job, opts, now); err != nil {
		return nil, err
	}

	validationOptions := &core.EnqueueOptions{
		Queue:               job.Queue,
		Priority:            job.Priority,
		TimeoutMs:           job.TimeoutMs,
		ScheduledAt:         job.ScheduledAt,
		ExpiresAt:           job.ExpiresAt,
		Retry:               job.Retry,
		Unique:              job.Unique,
		Tags:                job.Tags,
		VisibilityTimeoutMs: job.VisibilityTimeoutMs,
	}
	if err := core.ValidateEnqueueRequest(&core.EnqueueRequest{
		Type:    job.Type,
		Args:    job.Args,
		Meta:    job.Meta,
		Options: validationOptions,
	}); err != nil {
		return nil, err
	}
	return job, nil
}

func applyEnqueueOptions(job *core.Job, opts *ojsv1.EnqueueOptions, now time.Time) error {
	if opts == nil {
		return nil
	}
	if opts.Queue != "" {
		job.Queue = opts.Queue
	}

	priority := int(opts.Priority)
	job.Priority = &priority
	job.Tags = append([]string(nil), opts.Tags...)

	meta := make(map[string]any)
	if opts.Meta != nil {
		meta = opts.Meta.AsMap()
	}
	if opts.TraceId != "" {
		meta["trace_id"] = opts.TraceId
	}
	if len(meta) > 0 {
		data, err := json.Marshal(meta)
		if err != nil {
			return core.NewInvalidRequestError("invalid enqueue metadata", map[string]any{"error": err.Error()})
		}
		job.Meta = data
	}

	if opts.DelayUntil != nil {
		if err := opts.DelayUntil.CheckValid(); err != nil {
			return invalidProtoField("options.delay_until", err)
		}
		job.ScheduledAt = core.FormatTime(opts.DelayUntil.AsTime())
	}
	if opts.Timeout != nil {
		ms, err := positiveDurationMillis("options.timeout", opts.Timeout)
		if err != nil {
			return err
		}
		job.TimeoutMs = &ms
	}
	if opts.VisibilityTimeout != nil {
		ms, err := positiveDurationMillis("options.visibility_timeout", opts.VisibilityTimeout)
		if err != nil {
			return err
		}
		job.VisibilityTimeoutMs = &ms
	}
	if opts.Ttl != nil {
		if err := opts.Ttl.CheckValid(); err != nil || opts.Ttl.AsDuration() <= 0 {
			if err == nil {
				err = fmt.Errorf("duration must be greater than zero")
			}
			return invalidProtoField("options.ttl", err)
		}
		job.ExpiresAt = core.FormatTime(now.Add(opts.Ttl.AsDuration()))
	}

	if opts.Retry != nil {
		retry, err := protoRetryToCore(opts.Retry)
		if err != nil {
			return err
		}
		job.Retry = retry
	}
	maxAttempts := int(opts.MaxAttempts)
	if job.Retry != nil && job.Retry.MaxAttempts > 0 {
		maxAttempts = job.Retry.MaxAttempts
	}
	if maxAttempts > 0 {
		job.MaxAttempts = intPtr(maxAttempts)
	}

	if opts.Unique != nil {
		unique, err := protoUniqueToCore(opts.Unique)
		if err != nil {
			return err
		}
		job.Unique = unique
	}
	return nil
}

func mergeEnqueueOptions(defaults, override *ojsv1.EnqueueOptions) *ojsv1.EnqueueOptions {
	if defaults == nil && override == nil {
		return nil
	}
	merged := &ojsv1.EnqueueOptions{}
	if defaults != nil {
		if cloned, ok := proto.Clone(defaults).(*ojsv1.EnqueueOptions); ok {
			merged = cloned
		}
	}
	if override == nil {
		return merged
	}
	if override.Queue != "" {
		merged.Queue = override.Queue
	}
	if override.Priority != 0 {
		merged.Priority = override.Priority
	}
	if override.DelayUntil != nil {
		merged.DelayUntil = override.DelayUntil
	}
	if override.Timeout != nil {
		merged.Timeout = override.Timeout
	}
	if override.Retry != nil {
		merged.Retry = override.Retry
	}
	if override.Unique != nil {
		merged.Unique = override.Unique
	}
	if override.Ttl != nil {
		merged.Ttl = override.Ttl
	}
	if len(override.Tags) > 0 {
		merged.Tags = append([]string(nil), override.Tags...)
	}
	if override.TraceId != "" {
		merged.TraceId = override.TraceId
	}
	if override.Meta != nil {
		merged.Meta = override.Meta
	}
	if override.MaxAttempts != 0 {
		merged.MaxAttempts = override.MaxAttempts
	}
	if override.VisibilityTimeout != nil {
		merged.VisibilityTimeout = override.VisibilityTimeout
	}
	return merged
}

func marshalProtoValues(values []*structpb.Value) (json.RawMessage, error) {
	args := make([]any, 0, len(values))
	for i, value := range values {
		if value == nil {
			return nil, fmt.Errorf("args[%d] is nil", i)
		}
		args = append(args, value.AsInterface())
	}
	return json.Marshal(args)
}

func positiveDurationMillis(field string, value *durationpb.Duration) (int, error) {
	if err := value.CheckValid(); err != nil {
		return 0, invalidProtoField(field, err)
	}
	duration := value.AsDuration()
	if duration <= 0 {
		return 0, invalidProtoField(field, fmt.Errorf("duration must be greater than zero"))
	}
	millis := duration.Milliseconds()
	if millis <= 0 || millis > int64(math.MaxInt) {
		return 0, invalidProtoField(field, fmt.Errorf("duration is outside the supported millisecond range"))
	}
	return int(millis), nil
}

func invalidProtoField(field string, err error) *core.OJSError {
	return core.NewInvalidRequestError(
		fmt.Sprintf("Invalid %s: %v.", field, err),
		map[string]any{"field": field, "error": err.Error()},
	)
}

// stateToWorkflowProto maps core workflow state strings to proto enum values.
var stateToWorkflowProto = map[string]ojsv1.WorkflowState{
	"running":   ojsv1.WorkflowState_WORKFLOW_STATE_RUNNING,
	"completed": ojsv1.WorkflowState_WORKFLOW_STATE_COMPLETED,
	"failed":    ojsv1.WorkflowState_WORKFLOW_STATE_FAILED,
	"cancelled": ojsv1.WorkflowState_WORKFLOW_STATE_CANCELLED,
}

// workflowToProto converts a core.Workflow to its protobuf representation.
func workflowToProto(wf *core.Workflow) *ojsv1.Workflow {
	if wf == nil {
		return nil
	}

	pw := &ojsv1.Workflow{
		Id:        wf.ID,
		Name:      wf.Name,
		State:     stateToWorkflowProto[wf.State],
		CreatedAt: parseRFC3339(wf.CreatedAt),
	}

	if wf.CompletedAt != "" {
		pw.CompletedAt = parseRFC3339(wf.CompletedAt)
	}

	return pw
}

// protoToWorkflowRequest validates the protobuf DAG and maps the two workflow
// shapes supported by the core backend: dependency-free groups and strict
// linear chains.
func protoToWorkflowRequest(req *ojsv1.CreateWorkflowRequest) (*core.WorkflowRequest, error) {
	if req == nil || len(req.Steps) == 0 {
		return nil, core.NewInvalidRequestError(
			"A workflow requires at least one step.",
			map[string]any{"field": "steps", "validation": "non_empty"},
		)
	}

	stepsByID := make(map[string]*ojsv1.WorkflowStep, len(req.Steps))
	order := make([]string, 0, len(req.Steps))
	allIndependent := true
	for i, step := range req.Steps {
		if step == nil {
			return nil, core.NewInvalidRequestError(
				"Workflow steps must not be null.",
				map[string]any{"field": fmt.Sprintf("steps[%d]", i)},
			)
		}
		if step.Id == "" {
			return nil, core.NewInvalidRequestError(
				"Every workflow step requires an id.",
				map[string]any{"field": fmt.Sprintf("steps[%d].id", i)},
			)
		}
		if _, exists := stepsByID[step.Id]; exists {
			return nil, core.NewInvalidRequestError(
				fmt.Sprintf("Duplicate workflow step id %q.", step.Id),
				map[string]any{"field": fmt.Sprintf("steps[%d].id", i), "step_id": step.Id},
			)
		}
		stepsByID[step.Id] = step
		order = append(order, step.Id)
		if len(step.DependsOn) > 0 {
			allIndependent = false
		}
	}

	for _, step := range req.Steps {
		seenDeps := make(map[string]struct{}, len(step.DependsOn))
		for _, dependency := range step.DependsOn {
			if dependency == step.Id {
				return nil, core.NewInvalidRequestError(
					fmt.Sprintf("Workflow step %q cannot depend on itself.", step.Id),
					map[string]any{"step_id": step.Id, "dependency": dependency},
				)
			}
			if _, ok := stepsByID[dependency]; !ok {
				return nil, core.NewInvalidRequestError(
					fmt.Sprintf("Workflow step %q depends on unknown step %q.", step.Id, dependency),
					map[string]any{"step_id": step.Id, "dependency": dependency},
				)
			}
			if _, duplicate := seenDeps[dependency]; duplicate {
				return nil, core.NewInvalidRequestError(
					fmt.Sprintf("Workflow step %q repeats dependency %q.", step.Id, dependency),
					map[string]any{"step_id": step.Id, "dependency": dependency},
				)
			}
			seenDeps[dependency] = struct{}{}
		}
	}

	if !allIndependent {
		var err error
		order, err = linearWorkflowOrder(req.Steps)
		if err != nil {
			return nil, err
		}
	}

	wfReq := &core.WorkflowRequest{Name: req.Name}
	now := time.Now()
	for _, id := range order {
		step, err := protoToWorkflowStep(stepsByID[id], now)
		if err != nil {
			return nil, err
		}
		if allIndependent {
			wfReq.Jobs = append(wfReq.Jobs, step)
		} else {
			wfReq.Steps = append(wfReq.Steps, step)
		}
	}
	if allIndependent {
		wfReq.Type = "group"
	} else {
		wfReq.Type = "chain"
	}
	return wfReq, nil
}

func linearWorkflowOrder(steps []*ojsv1.WorkflowStep) ([]string, error) {
	children := make(map[string]string, len(steps))
	var root string
	for _, step := range steps {
		if len(step.DependsOn) > 1 {
			return nil, unsupportedWorkflowDAG()
		}
		if len(step.DependsOn) == 0 {
			if root != "" {
				return nil, unsupportedWorkflowDAG()
			}
			root = step.Id
			continue
		}
		parent := step.DependsOn[0]
		if _, exists := children[parent]; exists {
			return nil, unsupportedWorkflowDAG()
		}
		children[parent] = step.Id
	}
	if root == "" {
		return nil, core.NewInvalidRequestError("Workflow dependencies contain a cycle.", nil)
	}

	order := make([]string, 0, len(steps))
	seen := make(map[string]struct{}, len(steps))
	for current := root; current != ""; current = children[current] {
		if _, duplicate := seen[current]; duplicate {
			return nil, core.NewInvalidRequestError("Workflow dependencies contain a cycle.", nil)
		}
		seen[current] = struct{}{}
		order = append(order, current)
	}
	if len(order) != len(steps) {
		return nil, unsupportedWorkflowDAG()
	}
	return order, nil
}

func unsupportedWorkflowDAG() *core.OJSError {
	return core.NewInvalidRequestError(
		"Only dependency-free groups and strict linear workflow chains are supported.",
		map[string]any{"field": "steps.depends_on", "validation": "unsupported_dag"},
	)
}

func protoToWorkflowStep(req *ojsv1.WorkflowStep, now time.Time) (core.WorkflowJobRequest, error) {
	job, err := enqueueFieldsToJob(req.Type, req.Args, req.Options, now)
	if err != nil {
		return core.WorkflowJobRequest{}, err
	}

	options := &core.EnqueueOptions{
		Queue:               job.Queue,
		Priority:            job.Priority,
		TimeoutMs:           job.TimeoutMs,
		ScheduledAt:         job.ScheduledAt,
		ExpiresAt:           job.ExpiresAt,
		Retry:               job.Retry,
		Unique:              job.Unique,
		Tags:                append([]string(nil), job.Tags...),
		VisibilityTimeoutMs: job.VisibilityTimeoutMs,
		Metadata:            append(json.RawMessage(nil), job.Meta...),
	}
	if job.MaxAttempts != nil {
		if options.Retry == nil {
			options.Retry = &core.RetryPolicy{}
		}
		options.Retry.MaxAttempts = *job.MaxAttempts
	}
	return core.WorkflowJobRequest{
		Name:    req.Id,
		Type:    job.Type,
		Args:    job.Args,
		Options: options,
	}, nil
}

// protoRetryToCore converts a proto RetryPolicy to a core RetryPolicy.
func protoRetryToCore(r *ojsv1.RetryPolicy) (*core.RetryPolicy, error) {
	if r == nil {
		return nil, nil
	}
	cr := &core.RetryPolicy{
		MaxAttempts:        int(r.MaxAttempts),
		BackoffCoefficient: r.BackoffCoefficient,
		Jitter:             r.Jitter,
		NonRetryableErrors: append([]string(nil), r.NonRetryableErrors...),
		OnExhaustion:       r.OnExhaustion,
	}
	if r.InitialInterval != nil {
		if err := r.InitialInterval.CheckValid(); err != nil || r.InitialInterval.AsDuration() < 0 {
			if err == nil {
				err = fmt.Errorf("duration must be non-negative")
			}
			return nil, invalidProtoField("options.retry.initial_interval", err)
		}
		if r.InitialInterval.AsDuration() > 0 {
			cr.InitialInterval = core.FormatISO8601Duration(r.InitialInterval.AsDuration())
		}
	}
	if r.MaxInterval != nil {
		if err := r.MaxInterval.CheckValid(); err != nil || r.MaxInterval.AsDuration() < 0 {
			if err == nil {
				err = fmt.Errorf("duration must be non-negative")
			}
			return nil, invalidProtoField("options.retry.max_interval", err)
		}
		if r.MaxInterval.AsDuration() > 0 {
			cr.MaxInterval = core.FormatISO8601Duration(r.MaxInterval.AsDuration())
		}
	}
	if cr.OnExhaustion != "" && cr.OnExhaustion != "discard" && cr.OnExhaustion != "dead_letter" {
		return nil, invalidProtoField("options.retry.on_exhaustion", fmt.Errorf("must be discard or dead_letter"))
	}
	return cr, nil
}

// protoUniqueToCore converts a proto UniquePolicy to a core UniquePolicy.
func protoUniqueToCore(u *ojsv1.UniquePolicy) (*core.UniquePolicy, error) {
	if u == nil {
		return nil, nil
	}
	if len(u.ArgsKeys) > 0 || len(u.MetaKeys) > 0 {
		return nil, core.NewUnsupportedError("unique args_keys and meta_keys are not supported by this backend")
	}
	cu := &core.UniquePolicy{
		Keys: append([]string(nil), u.Key...),
	}
	for _, key := range cu.Keys {
		switch key {
		case "type", "queue", "args":
		case "meta":
			return nil, core.NewUnsupportedError("unique key 'meta' is not supported by this backend")
		default:
			return nil, invalidProtoField("options.unique.key", fmt.Errorf("unsupported key %q", key))
		}
	}
	if u.Period != nil {
		if err := u.Period.CheckValid(); err != nil || u.Period.AsDuration() <= 0 {
			if err == nil {
				err = fmt.Errorf("duration must be greater than zero")
			}
			return nil, invalidProtoField("options.unique.period", err)
		}
		cu.Period = core.FormatISO8601Duration(u.Period.AsDuration())
	}
	switch u.OnConflict {
	case ojsv1.UniqueConflictAction_UNIQUE_CONFLICT_ACTION_UNSPECIFIED,
		ojsv1.UniqueConflictAction_UNIQUE_CONFLICT_ACTION_REJECT:
		cu.OnConflict = "reject"
	case ojsv1.UniqueConflictAction_UNIQUE_CONFLICT_ACTION_IGNORE:
		cu.OnConflict = "ignore"
	case ojsv1.UniqueConflictAction_UNIQUE_CONFLICT_ACTION_REPLACE:
		cu.OnConflict = "replace"
	case ojsv1.UniqueConflictAction_UNIQUE_CONFLICT_ACTION_REPLACE_EXCEPT_SCHEDULE:
		return nil, core.NewUnsupportedError("unique replace_except_schedule is not supported by this backend")
	default:
		return nil, invalidProtoField("options.unique.on_conflict", fmt.Errorf("unknown value %d", u.OnConflict))
	}
	for _, protoState := range u.States {
		state, ok := protoToState[protoState]
		if !ok {
			return nil, invalidProtoField("options.unique.states", fmt.Errorf("unsupported state %s", protoState))
		}
		cu.States = append(cu.States, state)
	}
	return cu, nil
}

var protoToState = map[ojsv1.JobState]string{
	ojsv1.JobState_JOB_STATE_SCHEDULED: core.StateScheduled,
	ojsv1.JobState_JOB_STATE_AVAILABLE: core.StateAvailable,
	ojsv1.JobState_JOB_STATE_PENDING:   core.StatePending,
	ojsv1.JobState_JOB_STATE_ACTIVE:    core.StateActive,
	ojsv1.JobState_JOB_STATE_COMPLETED: core.StateCompleted,
	ojsv1.JobState_JOB_STATE_RETRYABLE: core.StateRetryable,
	ojsv1.JobState_JOB_STATE_CANCELLED: core.StateCancelled,
	ojsv1.JobState_JOB_STATE_DISCARDED: core.StateDiscarded,
}

func uniqueConflictToProto(conflict string) ojsv1.UniqueConflictAction {
	switch conflict {
	case "ignore":
		return ojsv1.UniqueConflictAction_UNIQUE_CONFLICT_ACTION_IGNORE
	case "replace":
		return ojsv1.UniqueConflictAction_UNIQUE_CONFLICT_ACTION_REPLACE
	default:
		return ojsv1.UniqueConflictAction_UNIQUE_CONFLICT_ACTION_REJECT
	}
}

func parseCoreDuration(value string) (time.Duration, error) {
	if value == "" {
		return 0, fmt.Errorf("empty duration")
	}
	if d, err := time.ParseDuration(value); err == nil {
		return d, nil
	}
	return core.ParseISO8601Duration(value)
}
