package scheduler

// Reaper is handled as part of the main Scheduler via RequeueStalled.
// This file is a placeholder for future reaper-specific enhancements.
//
// The stalled job reaper:
// - Scans the "ojs-active" KV bucket for jobs past their visibility deadline
// - Requeues stalled jobs by:
//   1. Updating job state to "available" in the "ojs-jobs" KV bucket
//   2. Removing from the "ojs-active" KV bucket
//   3. Acking the stale JetStream message
//   4. Re-publishing the job ID to the queue's JetStream subject
//
// JetStream's native AckWait provides a secondary safety net:
// if a message is not acked within AckWait, JetStream marks it
// for redelivery. However, since we set MaxDeliver=1 (retries
// are managed via KV state), the reaper is the primary mechanism
// for handling stalled jobs.
