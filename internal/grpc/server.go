package grpc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/openjobspec/ojs-backend-nats/internal/core"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	ojsv1 "github.com/openjobspec/ojs-proto/gen/go/ojs/v1"
)

// Server implements the OJSService gRPC service by delegating to a core.Backend.
type Server struct {
	ojsv1.UnimplementedOJSServiceServer
	backend    core.Backend
	subscriber core.EventSubscriber
}

// Register creates a new gRPC OJS server and registers it with the given gRPC server.
func Register(s *grpc.Server, backend core.Backend, opts ...ServerOption) {
	srv := &Server{backend: backend}
	for _, opt := range opts {
		opt(srv)
	}
	ojsv1.RegisterOJSServiceServer(s, srv)
}

// ServerOption configures the gRPC server.
type ServerOption func(*Server)

// WithEventSubscriber sets the event subscriber for streaming RPCs.
func WithEventSubscriber(sub core.EventSubscriber) ServerOption {
	return func(s *Server) {
		s.subscriber = sub
	}
}

// New returns a new gRPC OJS server wrapping the given backend.
func New(backend core.Backend) *Server {
	return &Server{backend: backend}
}

// --- System RPCs ---

func (s *Server) Manifest(ctx context.Context, req *ojsv1.ManifestRequest) (*ojsv1.ManifestResponse, error) {
	return &ojsv1.ManifestResponse{
		OjsVersion:       "1.0",
		ConformanceLevel: 4,
		Protocols:        []string{"http", "grpc"},
		Backend:          "nats",
		Implementation: &ojsv1.Implementation{
			Name:     "ojs-backend-nats",
			Version:  "1.0.0",
			Language: "go",
		},
	}, nil
}

func (s *Server) Health(ctx context.Context, req *ojsv1.HealthRequest) (*ojsv1.HealthResponse, error) {
	h, err := s.backend.Health(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "health check failed: %v", err)
	}

	st := ojsv1.HealthStatus_HEALTH_STATUS_OK
	if h.Status != "ok" {
		st = ojsv1.HealthStatus_HEALTH_STATUS_DEGRADED
	}

	return &ojsv1.HealthResponse{
		Status:    st,
		Timestamp: timestamppb.Now(),
		Details:   mustStruct(map[string]any{"backend": h.Backend.Type, "latency_ms": h.Backend.LatencyMs}),
	}, nil
}

// --- Job RPCs ---

func (s *Server) Enqueue(ctx context.Context, req *ojsv1.EnqueueRequest) (*ojsv1.EnqueueResponse, error) {
	job, err := enqueueRequestToJob(req)
	if err != nil {
		return nil, coreErrorToGRPC(err)
	}

	result, err := s.backend.Push(ctx, job)
	if err != nil {
		return nil, coreErrorToGRPC(err)
	}

	return &ojsv1.EnqueueResponse{
		Job: jobToProto(result),
	}, nil
}

func (s *Server) EnqueueBatch(ctx context.Context, req *ojsv1.EnqueueBatchRequest) (*ojsv1.EnqueueBatchResponse, error) {
	if req == nil || len(req.Jobs) == 0 {
		return nil, coreErrorToGRPC(core.NewInvalidRequestError(
			"At least one batch job is required.",
			map[string]any{"field": "jobs", "validation": "non_empty"},
		))
	}

	jobs := make([]*core.Job, 0, len(req.Jobs))
	now := time.Now()
	for _, j := range req.Jobs {
		job, err := enqueueJobRequestToJob(j, req.DefaultOptions, now)
		if err != nil {
			return nil, coreErrorToGRPC(err)
		}
		jobs = append(jobs, job)
	}

	results, err := s.backend.PushBatch(ctx, jobs)
	if err != nil {
		return nil, coreErrorToGRPC(err)
	}

	protoJobs := make([]*ojsv1.Job, 0, len(results))
	for _, r := range results {
		protoJobs = append(protoJobs, jobToProto(r))
	}

	return &ojsv1.EnqueueBatchResponse{
		Jobs:  protoJobs,
		Count: int32(len(protoJobs)),
	}, nil
}

func (s *Server) GetJob(ctx context.Context, req *ojsv1.GetJobRequest) (*ojsv1.GetJobResponse, error) {
	job, err := s.backend.Info(ctx, req.JobId)
	if err != nil {
		return nil, coreErrorToGRPC(err)
	}

	return &ojsv1.GetJobResponse{
		Job: jobToProto(job),
	}, nil
}

func (s *Server) CancelJob(ctx context.Context, req *ojsv1.CancelJobRequest) (*ojsv1.CancelJobResponse, error) {
	job, err := s.backend.Cancel(ctx, req.JobId)
	if err != nil {
		return nil, coreErrorToGRPC(err)
	}

	return &ojsv1.CancelJobResponse{
		Job: jobToProto(job),
	}, nil
}

// --- Worker RPCs ---

func (s *Server) Fetch(ctx context.Context, req *ojsv1.FetchRequest) (*ojsv1.FetchResponse, error) {
	count := int(req.Count)
	if count <= 0 {
		count = 1
	}

	visibilityMs := core.DefaultVisibilityTimeoutMs

	jobs, err := s.backend.Fetch(ctx, req.Queues, count, req.WorkerId, visibilityMs)
	if err != nil {
		return nil, coreErrorToGRPC(err)
	}

	protoJobs := make([]*ojsv1.Job, 0, len(jobs))
	for _, j := range jobs {
		protoJobs = append(protoJobs, jobToProto(j))
	}

	return &ojsv1.FetchResponse{
		Jobs: protoJobs,
	}, nil
}

func (s *Server) Ack(ctx context.Context, req *ojsv1.AckRequest) (*ojsv1.AckResponse, error) {
	var result []byte
	if req.Result != nil {
		var err error
		result, err = json.Marshal(req.Result.AsMap())
		if err != nil {
			return nil, status.Errorf(codes.InvalidArgument, "invalid result: %v", err)
		}
	}

	ackResp, err := s.backend.Ack(ctx, req.JobId, result)
	if err != nil {
		return nil, coreErrorToGRPC(err)
	}

	return &ojsv1.AckResponse{
		Acknowledged: ackResp.Acknowledged,
	}, nil
}

func (s *Server) Nack(ctx context.Context, req *ojsv1.NackRequest) (*ojsv1.NackResponse, error) {
	jobErr := &core.JobError{
		Message: req.Error.GetMessage(),
		Type:    req.Error.GetCode(),
	}

	nackResp, err := s.backend.Nack(ctx, req.JobId, jobErr, false)
	if err != nil {
		return nil, coreErrorToGRPC(err)
	}

	response := &ojsv1.NackResponse{
		State: stateToProto[nackResp.State],
	}
	if nackResp.NextAttemptAt != "" {
		if nextAttempt, parseErr := time.Parse(time.RFC3339, nackResp.NextAttemptAt); parseErr == nil {
			response.NextAttemptAt = timestamppb.New(nextAttempt)
		}
	}
	return response, nil
}

func (s *Server) Heartbeat(ctx context.Context, req *ojsv1.HeartbeatRequest) (*ojsv1.HeartbeatResponse, error) {
	visibilityMs := core.DefaultVisibilityTimeoutMs
	if req == nil {
		return nil, coreErrorToGRPC(core.NewInvalidRequestError("heartbeat request is required", nil))
	}
	if req.WorkerId == "" {
		return nil, coreErrorToGRPC(core.NewInvalidRequestError(
			"worker_id is required",
			map[string]any{"field": "worker_id", "validation": "required"},
		))
	}
	if req.ExtendBy != nil {
		var err error
		visibilityMs, err = positiveDurationMillis("extend_by", req.ExtendBy)
		if err != nil {
			return nil, coreErrorToGRPC(err)
		}
	}

	var activeJobs []string
	if req.Id != "" {
		activeJobs = []string{req.Id}
	}

	hbResp, err := s.backend.Heartbeat(ctx, req.WorkerId, activeJobs, visibilityMs)
	if err != nil {
		return nil, coreErrorToGRPC(err)
	}

	resp := &ojsv1.HeartbeatResponse{
		DirectedState: directiveToWorkerState(hbResp.Directive),
	}
	if len(hbResp.JobsExtended) > 0 {
		serverTime, parseErr := time.Parse(time.RFC3339, hbResp.ServerTime)
		if parseErr == nil {
			resp.NewDeadline = timestamppb.New(serverTime.Add(time.Duration(visibilityMs) * time.Millisecond))
		}
	}
	return resp, nil
}

// --- Queue RPCs ---

func (s *Server) ListQueues(ctx context.Context, req *ojsv1.ListQueuesRequest) (*ojsv1.ListQueuesResponse, error) {
	queues, err := s.backend.ListQueues(ctx)
	if err != nil {
		return nil, coreErrorToGRPC(err)
	}

	protoQueues := make([]*ojsv1.QueueInfo, 0, len(queues))
	for _, q := range queues {
		protoQueues = append(protoQueues, &ojsv1.QueueInfo{
			Name:   q.Name,
			Paused: q.Status == "paused",
		})
	}

	return &ojsv1.ListQueuesResponse{
		Queues: protoQueues,
	}, nil
}

func (s *Server) QueueStats(ctx context.Context, req *ojsv1.QueueStatsRequest) (*ojsv1.QueueStatsResponse, error) {
	stats, err := s.backend.QueueStats(ctx, req.Queue)
	if err != nil {
		return nil, coreErrorToGRPC(err)
	}

	return &ojsv1.QueueStatsResponse{
		Queue: stats.Queue,
		Stats: &ojsv1.QueueStatistics{
			Available: int64(stats.Stats.Available),
			Active:    int64(stats.Stats.Active),
			Scheduled: int64(stats.Stats.Scheduled),
			Retryable: int64(stats.Stats.Retryable),
		},
	}, nil
}

func (s *Server) PauseQueue(ctx context.Context, req *ojsv1.PauseQueueRequest) (*ojsv1.PauseQueueResponse, error) {
	if err := s.backend.PauseQueue(ctx, req.Queue); err != nil {
		return nil, coreErrorToGRPC(err)
	}
	return &ojsv1.PauseQueueResponse{}, nil
}

func (s *Server) ResumeQueue(ctx context.Context, req *ojsv1.ResumeQueueRequest) (*ojsv1.ResumeQueueResponse, error) {
	if err := s.backend.ResumeQueue(ctx, req.Queue); err != nil {
		return nil, coreErrorToGRPC(err)
	}
	return &ojsv1.ResumeQueueResponse{}, nil
}

// --- Dead Letter RPCs ---

func (s *Server) ListDeadLetter(ctx context.Context, req *ojsv1.ListDeadLetterRequest) (*ojsv1.ListDeadLetterResponse, error) {
	limit := int(req.Limit)
	if limit <= 0 {
		limit = 25
	}

	jobs, total, err := s.backend.ListDeadLetter(ctx, limit, 0)
	if err != nil {
		return nil, coreErrorToGRPC(err)
	}

	protoJobs := make([]*ojsv1.Job, 0, len(jobs))
	for _, j := range jobs {
		protoJobs = append(protoJobs, jobToProto(j))
	}

	return &ojsv1.ListDeadLetterResponse{
		Jobs:       protoJobs,
		TotalCount: int64(total),
	}, nil
}

func (s *Server) RetryDeadLetter(ctx context.Context, req *ojsv1.RetryDeadLetterRequest) (*ojsv1.RetryDeadLetterResponse, error) {
	job, err := s.backend.RetryDeadLetter(ctx, req.JobId)
	if err != nil {
		return nil, coreErrorToGRPC(err)
	}

	return &ojsv1.RetryDeadLetterResponse{
		Job: jobToProto(job),
	}, nil
}

func (s *Server) DeleteDeadLetter(ctx context.Context, req *ojsv1.DeleteDeadLetterRequest) (*ojsv1.DeleteDeadLetterResponse, error) {
	if err := s.backend.DeleteDeadLetter(ctx, req.JobId); err != nil {
		return nil, coreErrorToGRPC(err)
	}
	return &ojsv1.DeleteDeadLetterResponse{}, nil
}

// --- Cron RPCs ---

func (s *Server) RegisterCron(ctx context.Context, req *ojsv1.RegisterCronRequest) (*ojsv1.RegisterCronResponse, error) {
	argsJSON, err := json.Marshal(valuesToInterface(req.Args))
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "failed to marshal cron args: %v", err)
	}

	cronJob := &core.CronJob{
		Name:       req.Name,
		Expression: req.Cron,
		Timezone:   req.Timezone,
		Enabled:    true,
		JobTemplate: &core.CronJobTemplate{
			Type: req.Type,
			Args: argsJSON,
		},
	}

	result, err := s.backend.RegisterCron(ctx, cronJob)
	if err != nil {
		return nil, coreErrorToGRPC(err)
	}

	resp := &ojsv1.RegisterCronResponse{
		Name: result.Name,
	}
	if result.NextRunAt != "" {
		if t, err := time.Parse(time.RFC3339, result.NextRunAt); err == nil {
			resp.NextRunAt = timestamppb.New(t)
		}
	}
	return resp, nil
}

func (s *Server) UnregisterCron(ctx context.Context, req *ojsv1.UnregisterCronRequest) (*ojsv1.UnregisterCronResponse, error) {
	if _, err := s.backend.DeleteCron(ctx, req.Name); err != nil {
		return nil, coreErrorToGRPC(err)
	}
	return &ojsv1.UnregisterCronResponse{}, nil
}

func (s *Server) ListCron(ctx context.Context, req *ojsv1.ListCronRequest) (*ojsv1.ListCronResponse, error) {
	crons, err := s.backend.ListCron(ctx)
	if err != nil {
		return nil, coreErrorToGRPC(err)
	}

	entries := make([]*ojsv1.CronEntry, 0, len(crons))
	for _, c := range crons {
		entry := &ojsv1.CronEntry{
			Name:     c.Name,
			Cron:     c.Expression,
			Timezone: c.Timezone,
		}
		if c.JobTemplate != nil {
			entry.Type = c.JobTemplate.Type
		}
		if c.NextRunAt != "" {
			if t, err := time.Parse(time.RFC3339, c.NextRunAt); err == nil {
				entry.NextRunAt = timestamppb.New(t)
			}
		}
		if c.LastRunAt != "" {
			if t, err := time.Parse(time.RFC3339, c.LastRunAt); err == nil {
				entry.LastRunAt = timestamppb.New(t)
			}
		}
		entries = append(entries, entry)
	}

	return &ojsv1.ListCronResponse{
		Entries: entries,
	}, nil
}

// --- Workflow RPCs ---

func (s *Server) CreateWorkflow(ctx context.Context, req *ojsv1.CreateWorkflowRequest) (*ojsv1.CreateWorkflowResponse, error) {
	wfReq, err := protoToWorkflowRequest(req)
	if err != nil {
		return nil, coreErrorToGRPC(err)
	}

	wf, err := s.backend.CreateWorkflow(ctx, wfReq)
	if err != nil {
		return nil, coreErrorToGRPC(err)
	}

	return &ojsv1.CreateWorkflowResponse{
		Workflow: workflowToProto(wf),
	}, nil
}

func (s *Server) GetWorkflow(ctx context.Context, req *ojsv1.GetWorkflowRequest) (*ojsv1.GetWorkflowResponse, error) {
	wf, err := s.backend.GetWorkflow(ctx, req.WorkflowId)
	if err != nil {
		return nil, coreErrorToGRPC(err)
	}

	return &ojsv1.GetWorkflowResponse{
		Workflow: workflowToProto(wf),
	}, nil
}

func (s *Server) CancelWorkflow(ctx context.Context, req *ojsv1.CancelWorkflowRequest) (*ojsv1.CancelWorkflowResponse, error) {
	wf, err := s.backend.CancelWorkflow(ctx, req.WorkflowId)
	if err != nil {
		return nil, coreErrorToGRPC(err)
	}

	return &ojsv1.CancelWorkflowResponse{
		Workflow: workflowToProto(wf),
	}, nil
}

// --- Streaming RPCs ---

func (s *Server) StreamJobs(req *ojsv1.StreamJobsRequest, stream ojsv1.OJSService_StreamJobsServer) error {
	if len(req.Queues) == 0 {
		return status.Errorf(codes.InvalidArgument, "at least one queue is required")
	}
	workerID := req.WorkerId
	if workerID == "" {
		return status.Errorf(codes.InvalidArgument, "worker_id is required")
	}

	maxConcurrent := int(req.MaxConcurrent)
	if maxConcurrent <= 0 {
		maxConcurrent = 1
	}

	ctx := stream.Context()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()

	outstanding := make(map[string]struct{}, maxConcurrent)
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		for jobID := range outstanding {
			_, _ = s.backend.Nack(cleanupCtx, jobID, nil, true)
		}
	}()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			for jobID := range outstanding {
				job, err := s.backend.Info(ctx, jobID)
				if err != nil || job.State != core.StateActive {
					delete(outstanding, jobID)
				}
			}
			if len(outstanding) >= maxConcurrent {
				continue
			}

			count := maxConcurrent - len(outstanding)
			jobs, err := s.backend.Fetch(ctx, req.Queues, count, workerID, core.DefaultVisibilityTimeoutMs)
			if err != nil {
				continue
			}

			for _, j := range jobs {
				outstanding[j.ID] = struct{}{}
				if err := stream.Send(jobToProto(j)); err != nil {
					return err
				}
			}
		}
	}
}

func (s *Server) StreamEvents(req *ojsv1.StreamEventsRequest, stream ojsv1.OJSService_StreamEventsServer) error {
	if s.subscriber == nil {
		return status.Errorf(codes.Unavailable, "event streaming is not configured")
	}

	ctx := stream.Context()

	var (
		ch    <-chan *core.JobEvent
		unsub func()
		err   error
	)

	switch {
	case req.JobId != "":
		ch, unsub, err = s.subscriber.SubscribeJob(req.JobId)
	case len(req.Queues) == 1:
		ch, unsub, err = s.subscriber.SubscribeQueue(req.Queues[0])
	default:
		ch, unsub, err = s.subscriber.SubscribeAll()
	}
	if err != nil {
		return status.Errorf(codes.Internal, "failed to subscribe: %v", err)
	}
	defer unsub()

	queueFilter := make(map[string]bool, len(req.Queues))
	for _, q := range req.Queues {
		queueFilter[q] = true
	}
	typeFilter := make(map[string]bool, len(req.EventTypes))
	for _, t := range req.EventTypes {
		typeFilter[t] = true
	}

	keepalive := time.NewTicker(30 * time.Second)
	defer keepalive.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case event, ok := <-ch:
			if !ok {
				return nil
			}
			if len(queueFilter) > 0 && !queueFilter[event.Queue] {
				continue
			}
			if len(typeFilter) > 0 && !typeFilter[event.EventType] {
				continue
			}

			protoEvent := jobEventToProto(event)
			if err := stream.Send(protoEvent); err != nil {
				return err
			}
		case <-keepalive.C:
			ka := &ojsv1.Event{
				Id:        "evt_keepalive",
				Type:      "stream.keepalive",
				Timestamp: timestamppb.Now(),
			}
			if err := stream.Send(ka); err != nil {
				return err
			}
		}
	}
}

func jobEventToProto(e *core.JobEvent) *ojsv1.Event {
	pe := &ojsv1.Event{
		Id:      "evt_" + core.NewUUIDv7(),
		Type:    e.EventType,
		JobId:   e.JobID,
		JobType: e.JobType,
		Queue:   e.Queue,
	}

	if e.Timestamp != "" {
		if t, err := time.Parse(time.RFC3339, e.Timestamp); err == nil {
			pe.Timestamp = timestamppb.New(t)
		}
	}

	data := map[string]any{}
	if e.From != "" {
		data["from"] = e.From
	}
	if e.To != "" {
		data["to"] = e.To
	}
	if e.Progress > 0 {
		data["progress"] = e.Progress
	}
	if e.Message != "" {
		data["message"] = e.Message
	}
	if len(data) > 0 {
		pe.Data, _ = structpb.NewStruct(data)
	}

	return pe
}

// --- Helpers ---

func mustStruct(m map[string]any) *structpb.Struct {
	s, _ := structpb.NewStruct(m)
	return s
}

func coreErrorToGRPC(err error) error {
	if err == nil {
		return nil
	}

	var ojsErr *core.OJSError
	if !errors.As(err, &ojsErr) {
		return status.Error(codes.Internal, err.Error())
	}

	code := codes.Internal
	switch ojsErr.Code {
	case core.ErrCodeInvalidRequest, core.ErrCodeInvalidPayload, core.ErrCodeValidationError:
		code = codes.InvalidArgument
	case core.ErrCodeNotFound:
		code = codes.NotFound
	case core.ErrCodeConflict, core.ErrCodeQueuePaused:
		code = codes.FailedPrecondition
	case core.ErrCodeDuplicate:
		code = codes.AlreadyExists
	case core.ErrCodeUnsupported:
		code = codes.Unimplemented
	case core.ErrCodeRateLimited:
		code = codes.ResourceExhausted
	case core.ErrCodeVisibilityTimeout:
		code = codes.DeadlineExceeded
	case core.ErrCodeInternalError:
		code = codes.Internal
	}

	metadata := map[string]string{
		"code":      ojsErr.Code,
		"retryable": fmt.Sprintf("%t", ojsErr.Retryable),
	}
	if ojsErr.Type != "" {
		metadata["type"] = ojsErr.Type
	}
	for key, value := range ojsErr.Details {
		metadata[key] = fmt.Sprint(value)
	}
	reason := "OJS_" + strings.ToUpper(ojsErr.Code)
	detail := &errdetails.ErrorInfo{
		Reason:   reason,
		Domain:   "openjobspec.org",
		Metadata: metadata,
	}
	st := status.New(code, ojsErr.Message)
	withDetails, detailErr := st.WithDetails(detail)
	if detailErr != nil {
		return st.Err()
	}
	return withDetails.Err()
}

func valuesToInterface(vals []*structpb.Value) []any {
	result := make([]any, 0, len(vals))
	for _, v := range vals {
		result = append(result, v.AsInterface())
	}
	return result
}

func parseRFC3339(s string) *timestamppb.Timestamp {
	if s == "" {
		return nil
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return nil
	}
	return timestamppb.New(t)
}

func intPtr(v int) *int { return &v }

// directiveToWorkerState maps a backend heartbeat directive string to the
// protobuf WorkerState enum.
func directiveToWorkerState(directive string) ojsv1.WorkerState {
	switch directive {
	case "quiet":
		return ojsv1.WorkerState_WORKER_STATE_QUIET
	case "terminate":
		return ojsv1.WorkerState_WORKER_STATE_TERMINATE
	default:
		return ojsv1.WorkerState_WORKER_STATE_RUNNING
	}
}
