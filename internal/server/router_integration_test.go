package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/openjobspec/ojs-backend-nats/internal/core"
	natsbackend "github.com/openjobspec/ojs-backend-nats/internal/nats"
)

func TestRouterEndToEnd_JobLifecycle(t *testing.T) {
	tsURL := newIntegrationRouterServer(t)
	queue := "it-api-" + core.NewUUIDv7()

	createResp := postJSON(t, tsURL+"/ojs/v1/jobs", map[string]any{
		"type": "email.send",
		"args": []any{"user@example.com"},
		"options": map[string]any{
			"queue": queue,
		},
	})
	if createResp.StatusCode != http.StatusCreated {
		t.Fatalf("create status = %d, want %d", createResp.StatusCode, http.StatusCreated)
	}
	createdBody := decodeJSONBody(t, createResp.Body)
	jobID, ok := lookupString(createdBody, "job", "id")
	if !ok || jobID == "" {
		t.Fatalf("create response missing job.id: %#v", createdBody)
	}

	fetchResp := postJSON(t, tsURL+"/ojs/v1/workers/fetch", map[string]any{
		"queues":    []string{queue},
		"count":     1,
		"worker_id": "worker-1",
	})
	if fetchResp.StatusCode != http.StatusOK {
		t.Fatalf("fetch status = %d, want %d", fetchResp.StatusCode, http.StatusOK)
	}
	fetchBody := decodeJSONBody(t, fetchResp.Body)
	fetchedID, ok := lookupString(fetchBody, "job", "id")
	if !ok || fetchedID != jobID {
		t.Fatalf("fetch returned job.id=%q, want %q", fetchedID, jobID)
	}

	ackResp := postJSON(t, tsURL+"/ojs/v1/workers/ack", map[string]any{
		"job_id": jobID,
		"result": map[string]any{"ok": true},
	})
	defer ackResp.Body.Close()
	if ackResp.StatusCode != http.StatusOK {
		t.Fatalf("ack status = %d, want %d", ackResp.StatusCode, http.StatusOK)
	}

	getResp, err := http.Get(tsURL + "/ojs/v1/jobs/" + jobID)
	if err != nil {
		t.Fatalf("GET job error: %v", err)
	}
	defer getResp.Body.Close()
	if getResp.StatusCode != http.StatusOK {
		t.Fatalf("get status = %d, want %d", getResp.StatusCode, http.StatusOK)
	}
	getBody := decodeJSONBody(t, getResp.Body)
	state, ok := lookupString(getBody, "job", "state")
	if !ok || state != "completed" {
		t.Fatalf("job state = %q, want %q", state, "completed")
	}
}

func TestRouterEndToEnd_WorkflowChainCompletes(t *testing.T) {
	tsURL := newIntegrationRouterServer(t)
	queue := "it-wf-" + core.NewUUIDv7()

	createWFResp := postJSON(t, tsURL+"/ojs/v1/workflows", map[string]any{
		"type": "chain",
		"name": "it-chain",
		"steps": []map[string]any{
			{
				"name": "step-1",
				"type": "wf.step.one",
				"args": []any{"a"},
				"options": map[string]any{
					"queue": queue,
				},
			},
			{
				"name": "step-2",
				"type": "wf.step.two",
				"args": []any{"b"},
				"options": map[string]any{
					"queue": queue,
				},
			},
		},
	})
	if createWFResp.StatusCode != http.StatusCreated {
		t.Fatalf("workflow create status = %d, want %d", createWFResp.StatusCode, http.StatusCreated)
	}
	createWFBody := decodeJSONBody(t, createWFResp.Body)
	wfID, ok := lookupString(createWFBody, "workflow", "id")
	if !ok || wfID == "" {
		t.Fatalf("workflow create response missing workflow.id: %#v", createWFBody)
	}

	firstJob := fetchOneJobEventually(t, tsURL, queue, "wf-worker")
	if firstJob == nil {
		t.Fatal("first workflow job was not fetched")
	}
	if firstJobType, _ := firstJob["type"].(string); firstJobType != "wf.step.one" {
		t.Fatalf("first workflow job type = %q, want %q", firstJobType, "wf.step.one")
	}
	firstJobID, _ := firstJob["id"].(string)
	ackJob(t, tsURL, firstJobID)

	secondJob := fetchOneJobEventually(t, tsURL, queue, "wf-worker")
	if secondJob == nil {
		t.Fatal("second workflow job was not fetched")
	}
	if secondJobType, _ := secondJob["type"].(string); secondJobType != "wf.step.two" {
		t.Fatalf("second workflow job type = %q, want %q", secondJobType, "wf.step.two")
	}
	secondJobID, _ := secondJob["id"].(string)
	ackJob(t, tsURL, secondJobID)

	wf := getWorkflowEventually(t, tsURL, wfID)
	if state, _ := wf["state"].(string); state != "completed" {
		t.Fatalf("workflow state = %q, want %q", state, "completed")
	}
}

func TestRouterEndToEnd_QueuePauseAndRateLimit(t *testing.T) {
	tsURL := newIntegrationRouterServer(t)
	queue := "it-queue-" + core.NewUUIDv7()

	createResp1 := postJSON(t, tsURL+"/ojs/v1/jobs", map[string]any{
		"type": "email.send",
		"args": []any{"one@example.com"},
		"options": map[string]any{
			"queue": queue,
			"rate_limit": map[string]any{
				"max_per_second": 1,
			},
		},
	})
	if createResp1.StatusCode != http.StatusCreated {
		t.Fatalf("first create status = %d, want %d", createResp1.StatusCode, http.StatusCreated)
	}
	_ = decodeJSONBody(t, createResp1.Body)

	createResp2 := postJSON(t, tsURL+"/ojs/v1/jobs", map[string]any{
		"type": "email.send",
		"args": []any{"two@example.com"},
		"options": map[string]any{
			"queue": queue,
		},
	})
	if createResp2.StatusCode != http.StatusCreated {
		t.Fatalf("second create status = %d, want %d", createResp2.StatusCode, http.StatusCreated)
	}
	_ = decodeJSONBody(t, createResp2.Body)

	pauseResp := postJSON(t, tsURL+"/ojs/v1/queues/"+queue+"/pause", map[string]any{})
	if pauseResp.StatusCode != http.StatusOK {
		t.Fatalf("pause status = %d, want %d", pauseResp.StatusCode, http.StatusOK)
	}
	_ = decodeJSONBody(t, pauseResp.Body)

	pausedFetch := fetchResponse(t, tsURL, queue, "queue-worker")
	if len(extractJobs(pausedFetch)) != 0 {
		t.Fatal("paused queue should return no jobs")
	}

	resumeResp := postJSON(t, tsURL+"/ojs/v1/queues/"+queue+"/resume", map[string]any{})
	if resumeResp.StatusCode != http.StatusOK {
		t.Fatalf("resume status = %d, want %d", resumeResp.StatusCode, http.StatusOK)
	}
	_ = decodeJSONBody(t, resumeResp.Body)

	firstJob := fetchOneJobEventually(t, tsURL, queue, "queue-worker")
	if firstJob == nil {
		t.Fatal("first fetch after resume did not return a job")
	}
	firstID, _ := firstJob["id"].(string)
	ackJob(t, tsURL, firstID)

	immediateSecondFetch := fetchResponse(t, tsURL, queue, "queue-worker")
	if len(extractJobs(immediateSecondFetch)) != 0 {
		t.Fatal("rate-limited immediate second fetch should return no jobs")
	}

	time.Sleep(1100 * time.Millisecond)
	secondJob := fetchOneJobEventually(t, tsURL, queue, "queue-worker")
	if secondJob == nil {
		t.Fatal("fetch after rate-limit window did not return a job")
	}
	secondID, _ := secondJob["id"].(string)
	ackJob(t, tsURL, secondID)
}

func postJSON(t *testing.T, url string, payload any) *http.Response {
	t.Helper()

	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("json marshal error: %v", err)
	}
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("request build error: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("HTTP POST error: %v", err)
	}
	return resp
}

func decodeJSONBody(t *testing.T, body io.ReadCloser) map[string]any {
	t.Helper()
	defer body.Close()

	var out map[string]any
	if err := json.NewDecoder(body).Decode(&out); err != nil {
		t.Fatalf("decode body error: %v", err)
	}
	return out
}

func lookupString(m map[string]any, outer, inner string) (string, bool) {
	node, ok := m[outer].(map[string]any)
	if !ok {
		return "", false
	}
	value, ok := node[inner].(string)
	return value, ok
}

func newIntegrationRouterServer(t *testing.T) string {
	t.Helper()

	natsURL := os.Getenv("NATS_URL")
	if natsURL == "" {
		natsURL = "nats://localhost:4222"
	}

	backend, err := natsbackend.New(natsURL)
	if err != nil {
		t.Skipf("skipping integration test; NATS unavailable at %s: %v", natsURL, err)
	}
	t.Cleanup(func() {
		_ = backend.Close()
	})

	ts := httptest.NewServer(NewRouter(backend))
	t.Cleanup(ts.Close)
	return ts.URL
}

func fetchResponse(t *testing.T, baseURL, queue, workerID string) map[string]any {
	t.Helper()
	resp := postJSON(t, baseURL+"/ojs/v1/workers/fetch", map[string]any{
		"queues":    []string{queue},
		"count":     1,
		"worker_id": workerID,
	})
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		t.Fatalf("fetch status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	return decodeJSONBody(t, resp.Body)
}

func fetchOneJob(t *testing.T, baseURL, queue, workerID string) map[string]any {
	t.Helper()
	body := fetchResponse(t, baseURL, queue, workerID)
	if job, ok := body["job"].(map[string]any); ok {
		return job
	}
	return nil
}

func fetchOneJobEventually(t *testing.T, baseURL, queue, workerID string) map[string]any {
	t.Helper()
	for i := 0; i < 20; i++ {
		if job := fetchOneJob(t, baseURL, queue, workerID); job != nil {
			return job
		}
		time.Sleep(50 * time.Millisecond)
	}
	return nil
}

func ackJob(t *testing.T, baseURL, jobID string) {
	t.Helper()
	resp := postJSON(t, baseURL+"/ojs/v1/workers/ack", map[string]any{
		"job_id": jobID,
		"result": map[string]any{"ok": true},
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("ack status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
}

func getWorkflowEventually(t *testing.T, baseURL, workflowID string) map[string]any {
	t.Helper()

	for i := 0; i < 20; i++ {
		resp, err := http.Get(baseURL + "/ojs/v1/workflows/" + workflowID)
		if err != nil {
			t.Fatalf("GET workflow error: %v", err)
		}
		if resp.StatusCode == http.StatusOK {
			body := decodeJSONBody(t, resp.Body)
			if wf, ok := body["workflow"].(map[string]any); ok {
				state, _ := wf["state"].(string)
				if state == "completed" || state == "failed" || state == "cancelled" {
					return wf
				}
			}
			time.Sleep(50 * time.Millisecond)
			continue
		}
		_ = resp.Body.Close()
		time.Sleep(50 * time.Millisecond)
	}

	t.Fatalf("workflow %s did not reach terminal state in time", workflowID)
	return nil
}

func extractJobs(fetchBody map[string]any) []map[string]any {
	raw, ok := fetchBody["jobs"].([]any)
	if !ok {
		return nil
	}
	jobs := make([]map[string]any, 0, len(raw))
	for _, item := range raw {
		if job, ok := item.(map[string]any); ok {
			jobs = append(jobs, job)
		}
	}
	return jobs
}
