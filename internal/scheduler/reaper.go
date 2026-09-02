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
//  5. Publish replacement — Publish with a stable JetStream Msg-Id while the
//     active source and a durable handoff marker still exist.
//
//  6. Finalize source — Only after publication is confirmed, acknowledge the
//     matching source dispatch and conditionally remove its active entry.
//
// # JetStream AckWait vs. Reaper
//
// JetStream has a native AckWait mechanism that marks un-acked messages for
// redelivery. The NATS backend allows unlimited delivery attempts so a process
// crash before the job-state CAS cannot lose the durable source. KV revisions
// ensure that only one delivery can move a job to active, while the reaper
// handles visibility expiry after that transition.
//
// # Related Code
//
//   - internal/nats/scheduler_hooks.go — RequeueStalled, PromoteScheduled,
//     PromoteRetries
//   - internal/nats/consumer.go        — Consumer/message tracking, AckMessage
//   - internal/scheduler/scheduler.go  — Scheduler loop that calls RequeueStalled
package scheduler
