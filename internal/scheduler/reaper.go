// Stalled Job Reaper (NATS Backend)
//
// The reaper detects and recovers jobs that have exceeded their visibility
// timeout—typically because a worker crashed or became unresponsive. The
// implementation lives in NATSBackend.RequeueStalled in
// internal/nats/scheduler_hooks.go. The scheduler loop (see scheduler.go in
// this package) invokes RequeueStalled on each tick.
//
// # Visibility Timeout and the Active KV Bucket
//
// When a worker fetches a job, the backend writes an entry to the "ojs-active"
// KV bucket keyed by job ID. The value is an activeJobInfo struct containing
// the computed VisibilityDeadline (current time + VisibilityTimeoutMs). This
// deadline is the contract: if the worker does not complete or extend the job
// before the deadline, the reaper is allowed to reclaim it.
//
// # Recovery Flow
//
// On each tick, RequeueStalled performs the following for every entry in
// the "ojs-active" bucket:
//
//  1. Check deadline — Parse VisibilityDeadline from the active entry.
//     If the current time is still before the deadline, skip the job.
//
//  2. Validate state — Read the canonical job state from the "ojs-jobs" KV
//     bucket. If the job is no longer in the "active" state (e.g., it was
//     completed between ticks), simply remove the stale active entry and
//     move on.
//
//  3. Update state — Set the job state back to "available", clear StartedAt,
//     and update EnqueuedAt to now. Write the updated state to "ojs-jobs".
//
//  4. Remove active entry — Delete the job's key from "ojs-active".
//
//  5. Ack stale message — Acknowledge the original JetStream message via
//     the consumer tracker so JetStream does not attempt its own redelivery.
//
//  6. Republish — Publish the job ID to the queue's JetStream subject so it
//     becomes available for another worker to pick up.
//
// # JetStream AckWait vs. Reaper
//
// JetStream has a native AckWait mechanism that marks un-acked messages for
// redelivery. However, the NATS backend sets MaxDeliver=1 because retries are
// managed through KV state and the OJS retry extension, not through JetStream
// redelivery. This makes the reaper the primary mechanism for recovering
// stalled jobs. JetStream AckWait serves only as a secondary safety net.
//
// # Related Code
//
//   - internal/nats/scheduler_hooks.go — RequeueStalled, PromoteScheduled,
//     PromoteRetries
//   - internal/nats/consumer.go        — Consumer/message tracking, AckMessage
//   - internal/scheduler/scheduler.go  — Scheduler loop that calls RequeueStalled
package scheduler
