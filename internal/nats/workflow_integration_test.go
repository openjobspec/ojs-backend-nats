package nats

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/openjobspec/ojs-backend-nats/internal/core"
)

// TestCreateWorkflow_EmptyChain guards against an index-out-of-range panic when
// a chain workflow is created without any steps.
func TestCreateWorkflow_EmptyChain(t *testing.T) {
	backend := newIntegrationBackend(t)

	before, err := backend.workflows.Keys(context.Background())
	if err != nil {
		t.Fatalf("workflows.Keys() before = %v", err)
	}
	_, err = backend.CreateWorkflow(context.Background(), &core.WorkflowRequest{
		Type:  "chain",
		Steps: nil,
	})
	if err == nil {
		t.Fatal("CreateWorkflow(empty chain) = nil error, want invalid-request error")
	}

	var ojsErr *core.OJSError
	if ok := asOJSError(err, &ojsErr); !ok {
		t.Fatalf("CreateWorkflow(empty chain) error = %T (%v), want *core.OJSError", err, err)
	}
	if ojsErr.Code != core.ErrCodeInvalidRequest {
		t.Errorf("error code = %q, want %q", ojsErr.Code, core.ErrCodeInvalidRequest)
	}
	after, keysErr := backend.workflows.Keys(context.Background())
	if keysErr != nil {
		t.Fatalf("workflows.Keys() after = %v", keysErr)
	}
	if len(after) != len(before) {
		t.Fatalf("invalid workflow persisted KV state: before=%d after=%d", len(before), len(after))
	}
}

func TestCreateWorkflow_RejectsInvalidShapeBeforePersistence(t *testing.T) {
	backend := newIntegrationBackend(t)
	ctx := context.Background()

	tests := []struct {
		name string
		req  *core.WorkflowRequest
	}{
		{
			name: "unknown type",
			req:  &core.WorkflowRequest{Type: "dag", Jobs: []core.WorkflowJobRequest{{Type: "job.run", Args: json.RawMessage(`[]`)}}},
		},
		{
			name: "empty group",
			req:  &core.WorkflowRequest{Type: "group"},
		},
		{
			name: "chain jobs field",
			req: &core.WorkflowRequest{
				Type: "chain",
				Jobs: []core.WorkflowJobRequest{{Type: "job.run", Args: json.RawMessage(`[]`)}},
			},
		},
		{
			name: "group steps field",
			req: &core.WorkflowRequest{
				Type:  "group",
				Steps: []core.WorkflowJobRequest{{Type: "job.run", Args: json.RawMessage(`[]`)}},
			},
		},
		{
			name: "missing job type",
			req: &core.WorkflowRequest{
				Type: "batch",
				Jobs: []core.WorkflowJobRequest{{Args: json.RawMessage(`[]`)}},
			},
		},
		{
			name: "non-array args",
			req: &core.WorkflowRequest{
				Type: "group",
				Jobs: []core.WorkflowJobRequest{{Type: "job.run", Args: json.RawMessage(`{}`)}},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			before, err := backend.workflows.Keys(ctx)
			if err != nil {
				t.Fatalf("workflows.Keys() before = %v", err)
			}
			if _, err := backend.CreateWorkflow(ctx, tt.req); err == nil {
				t.Fatal("CreateWorkflow() error = nil, want invalid request")
			}
			after, err := backend.workflows.Keys(ctx)
			if err != nil {
				t.Fatalf("workflows.Keys() after = %v", err)
			}
			if len(after) != len(before) {
				t.Fatalf("invalid workflow persisted KV state: before=%d after=%d", len(before), len(after))
			}
		})
	}
}

func asOJSError(err error, target **core.OJSError) bool {
	if e, ok := err.(*core.OJSError); ok {
		*target = e
		return true
	}
	return false
}
