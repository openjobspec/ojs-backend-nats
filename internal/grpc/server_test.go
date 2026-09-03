package grpc

import (
	"errors"
	"testing"

	"github.com/openjobspec/ojs-backend-nats/internal/core"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestCoreErrorToGRPC(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want codes.Code
	}{
		{"nil", nil, codes.OK},
		{"not found", core.NewNotFoundError("Job", "job-1"), codes.NotFound},
		{"conflict", core.NewConflictError("invalid state transition", map[string]any{"job_id": "job-1"}), codes.FailedPrecondition},
		{"duplicate", &core.OJSError{Code: core.ErrCodeDuplicate, Message: "duplicate"}, codes.AlreadyExists},
		{"invalid request", core.NewInvalidRequestError("invalid", nil), codes.InvalidArgument},
		{"validation", core.NewValidationError("invalid", nil), codes.InvalidArgument},
		{"unsupported", core.NewUnsupportedError("unsupported"), codes.Unimplemented},
		{"rate limited", &core.OJSError{Code: core.ErrCodeRateLimited, Message: "slow down", Retryable: true}, codes.ResourceExhausted},
		{"visibility", &core.OJSError{Code: core.ErrCodeVisibilityTimeout, Message: "expired", Retryable: true}, codes.DeadlineExceeded},
		{"internal", core.NewInternalError("exploded"), codes.Internal},
		{"untyped internal", errors.New("something exploded"), codes.Internal},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := coreErrorToGRPC(tt.err)
			if tt.err == nil {
				if got != nil {
					t.Fatalf("coreErrorToGRPC(nil) = %v, want nil", got)
				}
				return
			}
			if status.Code(got) != tt.want {
				t.Errorf("coreErrorToGRPC() code = %v, want %v", status.Code(got), tt.want)
			}
		})
	}
}

func TestCoreErrorToGRPC_AttachesErrorInfo(t *testing.T) {
	err := core.NewConflictError("cannot acknowledge", map[string]any{
		"job_id":        "job-123",
		"current_state": "completed",
	})
	grpcErr := coreErrorToGRPC(err)
	st, ok := status.FromError(grpcErr)
	if !ok {
		t.Fatalf("status.FromError() failed for %v", grpcErr)
	}

	var info *errdetails.ErrorInfo
	for _, detail := range st.Details() {
		if value, ok := detail.(*errdetails.ErrorInfo); ok {
			info = value
			break
		}
	}
	if info == nil {
		t.Fatal("gRPC status has no google.rpc.ErrorInfo detail")
	}
	if info.Reason != "OJS_CONFLICT" {
		t.Errorf("ErrorInfo reason = %q, want OJS_CONFLICT", info.Reason)
	}
	if info.Metadata["code"] != core.ErrCodeConflict || info.Metadata["job_id"] != "job-123" {
		t.Errorf("ErrorInfo metadata = %#v", info.Metadata)
	}
}

func TestCoreErrorToGRPC_DoesNotStringMatch(t *testing.T) {
	err := coreErrorToGRPC(errors.New("not found duplicate invalid conflict"))
	if got := status.Code(err); got != codes.Internal {
		t.Fatalf("untyped string-shaped error code = %v, want Internal", got)
	}
}
