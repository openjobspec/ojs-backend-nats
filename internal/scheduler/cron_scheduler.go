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
//  3. Checks the overlap policy before firing (see below).
//  4. Enqueues a new job via NATSBackend.Push() using the cron's JobTemplate.
//  5. Computes the next run time and updates the cron entry in KV.
//
// # Cron Expression Parsing
//
// Expressions are parsed with robfig/cron/v3 using a five-field parser
// (Minute | Hour | Dom | Month | Dow | Descriptor). Timezone-aware schedules
// are supported via the CRON_TZ= prefix when CronJob.Timezone is set.
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
