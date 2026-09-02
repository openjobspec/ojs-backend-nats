# Finalization Audit — `ojs-backend-nats`

Branch: `refactor/clean-code-srp`  
Scope: this repository only. Existing unstaged work was preserved and extended; no commit was made.
Public HTTP routes, protobuf/schema shapes, NATS subjects, configuration, environment variables, and
dependency versions remain unchanged. One internal-only KV bucket was added for ephemeral cron claims.

## Outcome

- All 14 requested review areas were implemented with live NATS and race coverage.
- Job lifecycle transitions now use KV revisions; the CAS winner alone runs terminal metrics,
  workflow advancement, and transport-level event side effects.
- Dispatch handoffs use persisted markers plus stable JetStream `Nats-Msg-Id` values. Scheduled and
  retry indices, active sources, and requeue sources remain durable until replacement publication is
  confirmed or recoverable.
- Distributed cron scheduling now uses one UTC-defaulting parser, per-occurrence claims/leases,
  deterministic recovery by claimed job ID, revision-guarded cursor advancement, and
  revision-fenced claim deletion after the cursor is durably past the occurrence. Claims now live in
  a history-1, one-hour-TTL internal bucket instead of creating permanent stats tombstones. Claims
  contain only identity, lease, status, and registration-revision fields; recovery reloads and
  revision-fences the current registration before reconstructing the job.
- gRPC enqueue, workflow, heartbeat, streaming, and typed error behavior now preserve supported
  options and reject unsupported or invalid input rather than silently dropping it.

## Implemented findings

| Area | Resolution | Primary validation |
|---|---|---|
| Realtime middleware | Capability-preserving writer variants expose exactly the wrapped `Flusher`, `Hijacker`, `Pusher`, and `ReaderFrom` set, always support `Unwrap`, and record status/bytes. Local OTel middleware replaces the opaque shared wrapper. | `TestStatusResponseWriter_*`, full-router SSE and native WS tests |
| Empty workflows | Validates request type, shape, non-empty jobs, job type/args/options, and wrong-field combinations before workflow KV creation. | `TestCreateWorkflow_RejectsInvalidShapeBeforePersistence` |
| PubSub lifecycle | Broker closed gate; broker-owned map; linearizable Subscribe/Close; idempotent atomic removal, unsubscribe, and channel close. | 5 live/race PubSub tests |
| Durable handoffs | Stable dispatch generations, `Nats-Msg-Id`, persisted handoff markers, publish-before-source-finalization, unlimited source redelivery, reconciliation after ambiguous publication/restart. | scheduled/NACK/stalled/push/redelivery tests |
| Job state CAS | Reads carry job KV revisions; Fetch/Ack/Nack/Cancel/promoters/reaper use conditional updates/deletes. Active records carry dispatch generation to avoid acknowledging or deleting a replacement delivery. | terminal/fetch/group/chain contention tests |
| `kv.UpdateJSON` | Removed stale unconditional fallback. Context errors propagate; exhausted conflicts return `*kv.ConflictError` matching `kv.ErrConflict`. | live forced-conflict and concurrent-increment tests |
| Unique jobs | Revisioned JSON claims, preparation lease, guarded replacement/reacquisition/rollback, period expiry from existing `created_at`, state filtering, safe active replacement. | 5 live unique-policy stress tests |
| Cron | Shared `CRON_TZ` parser with UTC default; identity-only occurrence claim/lease; stable claimed job ID; current-registration reload plus `CronRevision` fencing; ambiguous-publish recovery; CAS cursor; revision-fenced completed-claim deletion; registration/generated-job/update/claim preflight against the connected server's `MaxPayload`; dedicated internal `ojs-cron-claims` KV with history 1, one-hour TTL and a 128 MiB bucket bound; bounded legacy migration that purges only confirmed revisions from `ojs-stats` without adding a stats TTL. | timezone, two-replica, ambiguity, restart, payload boundary, large-template fidelity, bounded-claim, TTL/tombstone, lease survival, legacy migration, stale-revision, delete-race, scan, stress, and benchmark tests |
| gRPC enqueue/batch | Shared conversion/defaulting; queue defaults to `default`; retry/max attempts, schedule, metadata/trace, priority, timeout, visibility, TTL, tags, and supported unique fields round-trip. Unsupported unique selectors/actions return typed errors. | converter and live RPC round-trip tests |
| gRPC workflows | Dependency-free requests map to groups; strict linear DAGs map to ordered chains; options are retained; empty, cyclic, unknown, duplicate, branching, and fan-in DAGs fail before persistence. | converter tests and live linear-chain RPC test |
| gRPC heartbeat | Validates positive protobuf duration, passes requested ID and worker ID, converts to milliseconds, and returns the backend-derived deadline. | live heartbeat tests |
| `StreamJobs` | Tracks outstanding IDs, reconciles backend state after unary ACK/NACK, enforces `max_concurrent`, and requeues outstanding work on stream exit. | live bufconn `max_concurrent=1` ACK/NACK/exit test |
| gRPC errors | Uses `errors.As(*core.OJSError)`, maps codes without string matching, maps transition conflicts to `FailedPrecondition`, duplicates to `AlreadyExists`, and attaches `google.rpc.ErrorInfo`. | typed mapping/detail tests |
| Validation/audit | Full race/coverage, Make, vet/build, live services, targeted stress, realtime, HTTP conformance, and gRPC runner probes completed. | gates below |

Additional conformance repair: Fetch now increments `attempt` when entering `active`; NACK preserves
that attempt and increments only on the next Fetch. Discard responses expose both `completed_at` and
`discarded_at`.

## Exact validation gates

### Go / Make

- Packages: **11**
- Top-level tests: **172**
- `go test ./... -race -covermode=atomic -coverprofile=... -count=1`: **PASS**
- Total statement coverage: **58.0%**
- Package coverage: API **31.5%**, gRPC **74.7%**, KV **17.0%**, NATS **59.4%**,
  scheduler **15.8%**, server **97.0%**
- `make test`: **PASS**
- `make lint` (`go vet ./...`): **PASS**
- `make build`: **PASS**
- `gofmt -l` over every changed/untracked Go file: **0 files**
- `git diff --check`: **PASS**

### Live integration / stress

- NATS TCP: **connected**
- Redis: **unavailable** (`127.0.0.1:6379: connection refused`)
- Targeted live/race stress tests: **25/25 PASS**
- Cron tombstone finalization live/race stress: **14 top-level tests × 10 runs = 140/140 PASS**
- Cron claim scan benchmark with **1,000 unrelated claim-bucket keys** and **8 in-flight claims**:
  **155,847,125 ns/op**, **8.000 claims/op**
- Full-router realtime: **2/2 PASS** (SSE and native WebSocket through the complete middleware stack)
- gRPC package: **32/32 top-level tests PASS**, including live NATS RPCs and bufconn streaming

### Conformance

- Isolated relevant HTTP lifecycle/operation tests: **9/9 PASS**
  (`L0-LC-003/004/005/006/008/010/013/014`, `L0-OPS-004`).
- Final level-2 cron category rerun against the rebuilt binary: **8/8 PASS** without Redis reset.
- Full fresh HTTP L0 runner: **98/152 PASS**, **54 FAIL**, **0 skipped**, **0 errored**.
  The 54 are environmental/surface blockers below, not failures in the isolated lifecycle matrix.
- Targeted gRPC runner probe: **2/8 PASS**. Native gRPC server tests remain green; runner failures
  are caused by its HTTP-shaped ACK/NACK assertions against the intentionally preserved lean proto.

### Cron-claim retention finalization rerun — 2026-08-11

- New live NATS coverage verifies multi-occurrence cleanup, legacy completed-claim reconciliation,
  stale revision fencing, deletion while another replica owns a live lease, a stale scheduler
  racing deletion, and filtered scans. Cron fixtures now delete their registrations during cleanup.
- The first 10× stress attempt exposed a minute-boundary assumption in the test fixture; the due
  occurrence was aligned to the current minute. The final repeated run passed **210/210** cases.
- Full `-race -covermode=atomic`, `make test`, `make lint`, `make build`, gRPC, and full-router
  realtime gates all passed.
- Redis was unavailable during this rerun (`127.0.0.1:6379: connection refused`), so cron
  conformance ran against shared NATS state without Redis reset. The category run was **7/8**.
  On the final binary, `L2-CRON-003` passed; `L2-CRON-004` initially encountered **196** stale
  UUID-named cron registrations left by earlier stress runs, then passed after those test-generated
  registrations were removed. The new fixture cleanup prevents that scheduler-backlog recurrence.

### Permanent cron-tombstone elimination — 2026-08-11

- New claims, reconciliation, and deletion use the internal `ojs-cron-claims` bucket. Its one-hour
  TTL is far beyond the one-second lease and ten-second scheduler recovery cadence; history 1 plus
  TTL removes both values and delete markers. `ojs-stats` remains configured with no global TTL.
- Startup and scheduler maintenance process at most 256 legacy revisions per pass, resume by stream
  revision, migrate live claims before reconciliation, and subject-purge only through the observed
  revision. A concurrently written newer revision is therefore retained.
- Live tests prove value/tombstone TTL expiry, survival beyond the lease window, legacy tombstone
  removal, newer-revision fencing, and safe in-flight legacy migration through job confirmation and
  cursor advancement.
- Final gates: full race/atomic coverage **PASS** (58.0%), `make test/lint/build` **PASS**, gRPC race
  package **PASS**, full-router SSE/WebSocket **2/2 PASS**, cron stress **140/140 PASS**, and HTTP
  level-2 cron conformance **8/8 PASS**.
- One initial stress pass exposed a timing-sensitive assertion that required an internal handoff
  marker to remain even when reconciliation had already safely removed it. The final test retains
  the end-to-end restart/no-duplicate checks and passed all ten repetitions.

### Cron-claim value-cap alignment — 2026-08-11

- Removed the claim bucket's 1 MiB `MaxValueSize`. `BucketCron`, `BucketJobs`, and
  `ojs-cron-claims` now rely on the same NATS server payload limit.
- The claim bucket remains bounded by history 1, a one-hour TTL, and 128 MiB total storage.
- The live large-template test now pauses publication to verify the claim stays below 512 bytes
  independently of argument size, then verifies all projected options, publication, cursor
  advancement, claim cleanup, payload fidelity, and ACK.
- The repository NATS test/development configuration now sets an 8 MiB server payload limit so the
  storage-path regression is exercised in CI and Docker Compose rather than skipped behind NATS's
  default 1 MiB transport limit.
- Final gates: targeted oversized-template live/race test **PASS**; full race/atomic coverage
  **PASS** (57.9%); cron live/race stress **10 tests × 10 runs = 100/100 PASS**;
  `make test`, `make lint`, and `make build` **PASS**; Docker Compose configuration validation
  **PASS**; HTTP level-2 cron conformance **8/8 PASS**.

### Cron payload-limit consistency finalization — 2026-08-11

- Removed `JobTemplate` and `overlap_policy` from occurrence claims. New claims serialize only the
  cron name, occurrence, stable job ID, status/phase, owner/lease timestamps, claimed timestamp, and
  `CronRevision`. Reconciliation reloads the current registration, requires the claimed revision
  and occurrence cursor to match, and only then reconstructs the stable-ID job.
- `RegisterCron` now serializes and preflights the initial registration, the actual generated job
  state (args, metadata, retry/unique, scheduling, priority, timeout, tags, visibility, and rate
  limit projections plus OJS state/timestamps), the bounded claim, and the future cursor-update
  registration. Limits come from `Conn.MaxPayload()`; a 96-byte margin covers serialized KV CAS
  headers. No runtime 8 MiB assumption was added.
- Boundary coverage creates an initial registration exactly one byte below the connected server's
  prior registration-only ceiling while the generated job plus KV header margin exceeds it. It is
  rejected before persistence with typed `invalid_request` details naming `generated_job`.
- Final validation:
  - targeted payload/recovery stress: **5 tests × 10 = 50/50 PASS** under `-race`;
  - configured 8 MiB NATS stress: **3 tests × 3 = 9/9 PASS**, with `/varz` reporting
    `max_payload=8388608`;
  - all cron tests under `-race`: **PASS**;
  - full `-race -covermode=atomic`: **PASS**, total coverage **58.2%**, NATS package **59.6%**;
  - `make test`, `make lint`, `make build`: **PASS**;
  - gRPC `-race`: **PASS**; full-router SSE/WebSocket: **2/2 PASS**;
  - Docker Compose config: **PASS**; HTTP level-2 cron conformance: **8/8 PASS**.
- Compose image build remains blocked at `go mod download`: the repository-local
  `replace github.com/openjobspec/ojs-go-backend-common => ../ojs-go-backend-common` target is
  outside the preserved `ojs-backend-nats` build context.

## Genuine blockers

1. **HTTP conformance isolation:** both HTTP and gRPC runners flush Redis only. This backend stores
   authoritative state in NATS KV/JetStream, so full-suite tests reuse queued/retryable jobs. The nine
   lifecycle/operation failures in the full L0 run all pass when isolated (**9/9**).
2. **Unsupported extension surfaces:** **43** full-L0 failures target extension routes/features not
   exposed by this backend; another **2** (`L0-EVT-001/002`) require the absent historical
   `GET /ojs/v1/events` route. Adding routes was explicitly prohibited.
3. **gRPC conformance shape mismatch:** the runner normalizes ACK to only `acknowledged` and NACK to
   `state`/`next_attempt_at`, while reusing HTTP assertions for `id`, `attempt`, `max_attempts`, and
   terminal timestamps. Those fields do not exist in the preserved protobuf responses.
4. **Conformance JSON report bug:** the sibling runner cannot serialize some failed step
   `json.RawMessage` values (`invalid character ... after top-level value`); table output was used.
5. **Docker image build:** `docker compose build ojs-server` fails at `go mod download` because the
   isolated build context cannot resolve `replace github.com/openjobspec/ojs-go-backend-common =>
   ../ojs-go-backend-common`. Fixing it requires changing dependency/build context outside this task.
6. **Compose healthcheck:** the NATS service maps/checks port 8222 but its preserved command does not
   enable the monitoring listener. Docker marks it unhealthy although port 4222, JetStream, and all
   live tests work.
7. **Redis reset unavailable for this rerun:** Redis was not listening on port 6379. The relevant
   cron category nevertheless passed **8/8** against the shared live NATS environment.

## Preserved boundaries

- No changes to sibling repositories, generated proto/OpenAPI/schema artifacts, `go.mod`, or
  `go.sum`.
- No public route, subject, stream name, consumer name, configuration field, environment variable,
  or image identity changes. The added KV bucket is internal-only.
- No `git add`, commit, or history rewrite; all work remains unstaged.
