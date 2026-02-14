package nats

import "fmt"

// Subject hierarchy for OJS-to-NATS mapping.
//
//	ojs.queue.{name}.jobs       -- main job messages
//	ojs.queue.{name}.pri.{0-9}  -- priority-segmented jobs
//	ojs.dead.{name}              -- dead letter messages
//	ojs.scheduled                -- delayed jobs awaiting their time
//	ojs.events.>                 -- lifecycle events (wildcardable)
const (
	// Stream subjects
	StreamName     = "OJS"
	SubjectPrefix  = "ojs"

	// KV bucket names
	BucketJobs      = "ojs-jobs"
	BucketUnique    = "ojs-unique"
	BucketCron      = "ojs-cron"
	BucketWorkers   = "ojs-workers"
	BucketWorkflows = "ojs-workflows"
	BucketQueues    = "ojs-queues"
	BucketScheduled = "ojs-scheduled"
	BucketRetry     = "ojs-retry"
	BucketDead      = "ojs-dead"
	BucketActive    = "ojs-active"
	BucketStats     = "ojs-stats"
)

// QueueJobsSubject returns the subject for publishing jobs to a queue.
// Example: ojs.queue.default.jobs
func QueueJobsSubject(queue string) string {
	return fmt.Sprintf("%s.queue.%s.jobs", SubjectPrefix, queue)
}

// QueuePrioritySubject returns a priority-segmented subject.
// Priority buckets: 0 (highest) to 9 (lowest).
// Example: ojs.queue.default.pri.3
func QueuePrioritySubject(queue string, bucket int) string {
	return fmt.Sprintf("%s.queue.%s.pri.%d", SubjectPrefix, queue, bucket)
}

// DeadLetterSubject returns the subject for dead letter messages.
// Example: ojs.dead.default
func DeadLetterSubject(queue string) string {
	return fmt.Sprintf("%s.dead.%s", SubjectPrefix, queue)
}

// EventSubject returns a subject for lifecycle events.
// Example: ojs.events.job.completed
func EventSubject(eventType string) string {
	return fmt.Sprintf("%s.events.%s", SubjectPrefix, eventType)
}

// QueueAllSubject returns the wildcard subject for all queue messages.
// Used for stream subject filter.
func QueueAllSubject() string {
	return fmt.Sprintf("%s.queue.>", SubjectPrefix)
}

// DeadAllSubject returns the wildcard subject for all dead letter messages.
func DeadAllSubject() string {
	return fmt.Sprintf("%s.dead.>", SubjectPrefix)
}

// EventsAllSubject returns the wildcard subject for all events.
func EventsAllSubject() string {
	return fmt.Sprintf("%s.events.>", SubjectPrefix)
}

// ConsumerName returns the durable consumer name for a queue.
func ConsumerName(queue string) string {
	return fmt.Sprintf("ojs-consumer-%s", queue)
}

// PriorityBucket maps an OJS priority (-100 to 100) to a bucket (0-9).
// Higher OJS priority = lower bucket number = fetched first.
func PriorityBucket(priority int) int {
	// Map [-100, 100] to [0, 9]
	// priority 100  -> bucket 0 (highest)
	// priority 0    -> bucket 5
	// priority -100 -> bucket 9 (lowest)
	bucket := (100 - priority) / 21
	if bucket < 0 {
		bucket = 0
	}
	if bucket > 9 {
		bucket = 9
	}
	return bucket
}
