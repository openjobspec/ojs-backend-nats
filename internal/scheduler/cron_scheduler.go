// Cron Scheduling Architecture (NATS Backend)
//
// The NATS backend implements cron scheduling through the NATSBackend.FireCronJobs
// method in internal/nats/cron.go. The scheduler loop (see scheduler.go in this
// package) invokes FireCronJobs on each tick to evaluate and fire due cron jobs.
//
// # Storage Model
//
// Cron registrations are persisted in the NATS KV bucket "ojs-cron" via a
// CronStore interface. Each entry stores the cron expression, job template,
// queue, timezone, overlap policy, and the computed NextRunAt timestamp.
//
// # Evaluation Flow
//
// On each scheduler tick, FireCronJobs:
//  1. Lists all registered cron jobs from the KV store.
//  2. Parses NextRunAt and skips jobs whose next run is still in the future.
//  3. Creates a per-occurrence KV claim with a short owner lease.
//  4. Enqueues the claim's stable job ID and reconciles ambiguous publication.
//  5. Advances the cron cursor with a revision-guarded KV update.
//
// # Cron Expression Parsing
//
// Expressions are parsed by one shared robfig/cron/v3 parser using five fields
// (Minute | Hour | Dom | Month | Dow | Descriptor). CRON_TZ is applied
// consistently during registration and cursor advancement; UTC is the default.
//
// # Overlap Policy
//
// Two overlap policies are supported:
//
//   - "allow" (default): A new instance is always fired, regardless of whether
//     a previous instance is still running.
//
//   - "skip": Before firing, the scheduler checks whether the previous instance
//     is still in a non-terminal state by looking up the job ID stored in the
//     KV key "cron-instance.<name>" within the stats bucket. If the previous
//     instance is still active, the tick is skipped and only NextRunAt is
//     advanced. When a new instance is fired, its job ID is recorded for
//     future overlap checks.
//
// # Related Code
//
//   - internal/nats/cron.go          — FireCronJobs, RegisterCron, DeleteCron,
//     overlap helpers (isCronInstanceRunning, setCronInstance)
//   - internal/scheduler/scheduler.go — Scheduler loop that calls FireCronJobs
package scheduler
