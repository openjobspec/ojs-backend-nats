package scheduler

// CronScheduler is handled as part of the main Scheduler via FireCronJobs.
// This file is a placeholder for future cron-specific enhancements.
//
// The cron scheduler:
// - Reads cron registrations from the NATS KV bucket "ojs-cron"
// - Checks NextRunAt against current time
// - Fires due cron jobs by calling backend.Push()
// - Updates NextRunAt to the next schedule time
// - Supports overlap_policy: "allow" (default) and "skip"
// - Uses KV-based instance tracking for overlap detection
