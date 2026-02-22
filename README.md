# ojs-backend-nats

[![CI](https://github.com/openjobspec/ojs-backend-nats/actions/workflows/ci.yml/badge.svg)](https://github.com/openjobspec/ojs-backend-nats/actions/workflows/ci.yml)
![Conformance](https://github.com/openjobspec/ojs-backend-nats/raw/main/.github/badges/conformance.svg)
[![Go Report Card](https://goreportcard.com/badge/github.com/openjobspec/ojs-backend-nats)](https://goreportcard.com/report/github.com/openjobspec/ojs-backend-nats)
[![License](https://img.shields.io/badge/License-Apache_2.0-blue.svg)](LICENSE)

A NATS JetStream-backed implementation of the [Open Job Spec (OJS)](https://github.com/openjobspec/spec) server.

## Overview

This backend implements the full OJS specification using **NATS with JetStream** for job queuing and **NATS KV** (backed by JetStream) for state management. It provides all 5 conformance levels (0-4) including retries, scheduling, workflows, unique jobs, and cron.

## Architecture

### Three-Layer Design

Like all OJS backends, this project follows a three-layer architecture:

| Layer | Package | Purpose |
|-------|---------|---------|
| **API** | `internal/api/` | HTTP handlers (chi router), request validation, error responses |
| **Core** | `internal/core/` | Business logic interfaces, job state machine, retry evaluation |
| **Storage** | `internal/nats/`, `internal/kv/` | NATS JetStream + KV implementation of core interfaces |

The `api/` and `core/` packages are shared across all OJS backends. Only the storage layer changes.

### OJS-to-NATS Concept Mapping

| OJS Concept | NATS Implementation |
|-------------|-------------------|
| **Job queue** | JetStream stream `OJS` with subject filter `ojs.queue.{name}.jobs` |
| **Job enqueue** | JetStream publish to `ojs.queue.{name}.jobs` |
| **Job fetch** | Pull consumer `Fetch()` with explicit ack policy |
| **Job ack** | JetStream `msg.Ack()` + KV state update |
| **Job nack** | `msg.Ack()` + KV retry index (scheduler re-publishes when due) |
| **Visibility timeout** | KV-tracked deadline + reaper goroutine |
| **Heartbeat** | `msg.InProgress()` + KV deadline extension |
| **Priority** | Stored in KV, applied at fetch time |
| **Scheduled jobs** | KV index `ojs-scheduled`, scheduler promotes when due |
| **Dead letter** | KV index `ojs-dead` on max delivery exceeded |
| **Job state** | NATS KV bucket `ojs-jobs` (key: job_id, value: state JSON) |
| **Events** | Publish to `ojs.events.{event_type}` subject |
| **Unique jobs** | NATS KV `Create()` (fails if key exists) for locks |
| **Cron** | KV bucket `ojs-cron`, scheduler goroutine for firing |
| **Workflows** | KV bucket `ojs-workflows` for state tracking |
| **Queue stats** | Derived from JetStream consumer info + KV counters |

### Subject Hierarchy

```
ojs.queue.{name}.jobs            -- main job messages
ojs.queue.{name}.pri.{0-9}       -- priority-segmented jobs (future)
ojs.dead.{name}                   -- dead letter messages
ojs.events.>                      -- lifecycle events (wildcardable)
ojs.events.job.completed          -- specific event subscription
ojs.events.workflow.>              -- all workflow events
```

### NATS KV Buckets

| Bucket | Purpose |
|--------|---------|
| `ojs-jobs` | Full job state (JSON), key = job_id |
| `ojs-unique` | Unique job locks, key = fingerprint hash |
| `ojs-cron` | Cron registrations, key = cron name |
| `ojs-workers` | Worker info with TTL, key = worker_id |
| `ojs-workflows` | Workflow state, key = workflow_id |
| `ojs-queues` | Queue metadata (paused, rate limit), key = queue name |
| `ojs-scheduled` | Scheduled job index, key = job_id |
| `ojs-retry` | Retry job index, key = job_id |
| `ojs-dead` | Dead letter index, key = job_id |
| `ojs-active` | Active job tracking with visibility deadline |
| `ojs-stats` | Queue statistics counters |

### Background Schedulers

| Scheduler | Interval | Purpose |
|-----------|----------|---------|
| Scheduled promoter | 1s | Moves due scheduled jobs to available |
| Retry promoter | 200ms | Moves due retry jobs to available |
| Stalled reaper | 500ms | Requeues jobs past visibility timeout |
| Cron scheduler | 10s | Fires due cron jobs |

## Quick Start

### Prerequisites

- Go 1.22+
- NATS 2.10+ with JetStream enabled

### Run with Docker Compose

```bash
make docker-up
```

This starts NATS with JetStream enabled and the OJS server.

### Run locally

```bash
# Start NATS with JetStream
nats-server --jetstream

# Build and run
make run
```

### Configuration

| Variable | Default | Description |
|----------|---------|-------------|
| `NATS_URL` | `nats://localhost:4222` | NATS connection URL |
| `OJS_PORT` | `8080` | HTTP server port |

## Build, Test, and Lint

```bash
make build          # Build server binary to bin/ojs-server
make test           # go test ./... -race -cover
make lint           # go vet ./...
make fmt            # gofmt -w on all Go files
make run            # Build and run (needs NATS_URL)
make docker-up      # Start server + NATS via Docker Compose
make docker-down    # Stop Docker Compose
```

### Conformance Tests

```bash
make conformance              # Run all conformance levels
make conformance-level-0      # Run specific level (0-4)
```

## Trade-offs vs. Redis/Postgres Backends

### Strengths

- **Single binary dependency**: NATS server is a single binary with no external dependencies
- **Lightweight operations**: No Redis cluster management or PostgreSQL administration
- **Built-in clustering**: NATS clustering is built-in and simple to configure
- **Native per-message ack/nak**: JetStream provides native message acknowledgment
- **KV store eliminates external state**: No need for a separate database for job state
- **Subject-based routing**: Flexible event subscription with wildcards
- **Excellent Go ecosystem**: First-class Go client with modern JetStream API

### Weaknesses

- **Smaller community**: NATS has a smaller community than Redis or Kafka
- **JetStream maturity**: Less battle-tested at extreme scale compared to Kafka
- **No transactional enqueueing**: Cannot atomically enqueue a job with application data
- **Fewer managed offerings**: Fewer cloud-managed NATS services available
- **KV scanning**: Some operations (e.g., queue stats) require scanning KV buckets

### Design Decisions

1. **Single stream with subject filtering**: All OJS messages share the `OJS` stream with subject-based routing. This simplifies retention and replication while providing logical separation.

2. **Pull consumers**: Workers use pull consumers for job fetching, giving the application control over flow and matching the OJS fetch-ack-nack model.

3. **KV for state, JetStream for queuing**: Job state is stored in NATS KV for random access, while JetStream handles the queue ordering and delivery mechanics.

4. **Scheduler-based retries**: Rather than using `msg.NakWithDelay()`, retries are managed through KV state and a scheduler goroutine. This provides cleaner state tracking and is consistent with the Redis backend approach.

5. **MaxDeliver=1**: JetStream consumers are configured with `MaxDeliver=1` because retry logic is managed at the application level via KV state and the scheduler.

## Conformance Levels

| Level | Status | Notes |
|-------|--------|-------|
| 0 | Full | JetStream pull consumers are a natural fit |
| 1 | Full | KV-tracked visibility, scheduler-based retry with backoff |
| 2 | Full | Scheduler + KV for cron, delayed job promotion |
| 3 | Full | KV-backed workflow state tracking |
| 4 | Full | KV for unique jobs, priority via KV, queue pause |

## Observability

### OpenTelemetry

The server supports distributed tracing via OpenTelemetry. Set the following environment variable to enable:

```bash
export OTEL_EXPORTER_OTLP_ENDPOINT=http://localhost:4317
```

Traces are exported in OTLP format over gRPC. Compatible with Jaeger, Zipkin, Grafana Tempo, and any OTLP-compatible collector.

You can also use the legacy env vars `OJS_OTEL_ENABLED=true` and `OJS_OTEL_ENDPOINT` for explicit control.

## Production Deployment Notes

- **Rate limiting**: This server does not enforce request rate limits. Place a reverse proxy (e.g., Nginx, Envoy, or a cloud load balancer) in front of the server to add rate limiting in production.
- **Authentication**: Set `OJS_API_KEY` to require Bearer token auth on all endpoints. For local-only testing, set `OJS_ALLOW_INSECURE_NO_AUTH=true`.
- **TLS**: Terminate TLS at a reverse proxy or load balancer rather than at the application level.

## License

Apache 2.0

