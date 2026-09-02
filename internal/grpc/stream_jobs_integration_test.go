package grpc

import (
	"context"
	"net"
	"os"
	"testing"
	"time"

	natsbackend "github.com/openjobspec/ojs-backend-nats/internal/nats"
	grpcgo "google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"

	ojsv1 "github.com/openjobspec/ojs-proto/gen/go/ojs/v1"
)

func newStreamingClient(t *testing.T) (ojsv1.OJSServiceClient, *natsbackend.NATSBackend) {
	t.Helper()
	natsURL := os.Getenv("NATS_URL")
	if natsURL == "" {
		natsURL = "nats://localhost:4222"
	}
	backend, err := natsbackend.New(natsURL)
	if err != nil {
		t.Skipf("skipping StreamJobs integration test; NATS unavailable at %s: %v", natsURL, err)
	}
	t.Cleanup(func() { _ = backend.Close() })

	listener := bufconn.Listen(1024 * 1024)
	grpcServer := grpcgo.NewServer()
	Register(grpcServer, backend)
	go func() {
		_ = grpcServer.Serve(listener)
	}()
	t.Cleanup(grpcServer.Stop)

	conn, err := grpcgo.NewClient("passthrough:///bufnet",
		grpcgo.WithContextDialer(func(context.Context, string) (net.Conn, error) {
			return listener.Dial()
		}),
		grpcgo.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("grpc.DialContext() error = %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return ojsv1.NewOJSServiceClient(conn), backend
}

func TestStreamJobs_ReconcilesUnaryAckNackAndExit(t *testing.T) {
	client, backend := newStreamingClient(t)
	ctx, cancelAll := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelAll()
	queue := "grpc-stream-" + newSuffix()

	first, err := client.Enqueue(ctx, &ojsv1.EnqueueRequest{Type: "stream.first", Options: &ojsv1.EnqueueOptions{Queue: queue}})
	if err != nil {
		t.Fatalf("first Enqueue() error = %v", err)
	}
	second, err := client.Enqueue(ctx, &ojsv1.EnqueueRequest{Type: "stream.second", Options: &ojsv1.EnqueueOptions{Queue: queue}})
	if err != nil {
		t.Fatalf("second Enqueue() error = %v", err)
	}

	streamCtx, cancelStream := context.WithCancel(ctx)
	stream, err := client.StreamJobs(streamCtx, &ojsv1.StreamJobsRequest{
		Queues:        []string{queue},
		WorkerId:      "stream-worker",
		MaxConcurrent: 1,
	})
	if err != nil {
		t.Fatalf("StreamJobs() error = %v", err)
	}

	deliveredFirst, err := stream.Recv()
	if err != nil {
		t.Fatalf("first Recv() error = %v", err)
	}
	remainingID := second.Job.Id
	if deliveredFirst.Id == second.Job.Id {
		remainingID = first.Job.Id
	}

	time.Sleep(150 * time.Millisecond)
	remaining, err := backend.Info(ctx, remainingID)
	if err != nil {
		t.Fatalf("Info(remaining) error = %v", err)
	}
	if remaining.State != "available" {
		t.Fatalf("max_concurrent=1 allowed a second outstanding job; state=%q", remaining.State)
	}

	if _, err := client.Ack(ctx, &ojsv1.AckRequest{JobId: deliveredFirst.Id}); err != nil {
		t.Fatalf("Ack() error = %v", err)
	}
	deliveredSecond, err := stream.Recv()
	if err != nil {
		t.Fatalf("second Recv() after Ack error = %v", err)
	}
	if deliveredSecond.Id != remainingID {
		t.Fatalf("second delivered ID = %q, want %q", deliveredSecond.Id, remainingID)
	}

	third, err := client.Enqueue(ctx, &ojsv1.EnqueueRequest{Type: "stream.third", Options: &ojsv1.EnqueueOptions{Queue: queue}})
	if err != nil {
		t.Fatalf("third Enqueue() error = %v", err)
	}
	if _, err := client.Nack(ctx, &ojsv1.NackRequest{
		JobId: deliveredSecond.Id,
		Error: &ojsv1.JobError{Code: "temporary", Message: "retry later"},
	}); err != nil {
		t.Fatalf("Nack() error = %v", err)
	}
	deliveredThird, err := stream.Recv()
	if err != nil {
		t.Fatalf("third Recv() after Nack error = %v", err)
	}
	if deliveredThird.Id != third.Job.Id {
		t.Fatalf("third delivered ID = %q, want %q", deliveredThird.Id, third.Job.Id)
	}

	cancelStream()
	_, _ = stream.Recv()
	deadline := time.Now().Add(3 * time.Second)
	for {
		job, infoErr := backend.Info(context.Background(), deliveredThird.Id)
		if infoErr == nil && job.State == "available" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("stream exit did not requeue outstanding job; state=%v err=%v", job, infoErr)
		}
		time.Sleep(25 * time.Millisecond)
	}
}
