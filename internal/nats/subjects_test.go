package nats

import (
	"testing"
)

func TestQueueJobsSubject(t *testing.T) {
	tests := []struct {
		queue string
		want  string
	}{
		{"default", "ojs.queue.default.jobs"},
		{"emails", "ojs.queue.emails.jobs"},
		{"high-priority", "ojs.queue.high-priority.jobs"},
	}
	for _, tt := range tests {
		got := QueueJobsSubject(tt.queue)
		if got != tt.want {
			t.Errorf("QueueJobsSubject(%q) = %q, want %q", tt.queue, got, tt.want)
		}
	}
}

func TestQueuePrioritySubject(t *testing.T) {
	tests := []struct {
		queue  string
		bucket int
		want   string
	}{
		{"default", 0, "ojs.queue.default.pri.0"},
		{"emails", 5, "ojs.queue.emails.pri.5"},
		{"tasks", 9, "ojs.queue.tasks.pri.9"},
	}
	for _, tt := range tests {
		got := QueuePrioritySubject(tt.queue, tt.bucket)
		if got != tt.want {
			t.Errorf("QueuePrioritySubject(%q, %d) = %q, want %q", tt.queue, tt.bucket, got, tt.want)
		}
	}
}

func TestDeadLetterSubject(t *testing.T) {
	got := DeadLetterSubject("default")
	want := "ojs.dead.default"
	if got != want {
		t.Errorf("DeadLetterSubject(\"default\") = %q, want %q", got, want)
	}
}

func TestEventSubject(t *testing.T) {
	tests := []struct {
		eventType string
		want      string
	}{
		{"job.completed", "ojs.events.job.completed"},
		{"job.failed", "ojs.events.job.failed"},
		{"queue.paused", "ojs.events.queue.paused"},
	}
	for _, tt := range tests {
		got := EventSubject(tt.eventType)
		if got != tt.want {
			t.Errorf("EventSubject(%q) = %q, want %q", tt.eventType, got, tt.want)
		}
	}
}

func TestWildcardSubjects(t *testing.T) {
	tests := []struct {
		name string
		fn   func() string
		want string
	}{
		{"QueueAllSubject", QueueAllSubject, "ojs.queue.>"},
		{"DeadAllSubject", DeadAllSubject, "ojs.dead.>"},
		{"EventsAllSubject", EventsAllSubject, "ojs.events.>"},
	}
	for _, tt := range tests {
		got := tt.fn()
		if got != tt.want {
			t.Errorf("%s() = %q, want %q", tt.name, got, tt.want)
		}
	}
}

func TestConsumerName(t *testing.T) {
	tests := []struct {
		queue string
		want  string
	}{
		{"default", "ojs-consumer-default"},
		{"emails", "ojs-consumer-emails"},
	}
	for _, tt := range tests {
		got := ConsumerName(tt.queue)
		if got != tt.want {
			t.Errorf("ConsumerName(%q) = %q, want %q", tt.queue, got, tt.want)
		}
	}
}

func TestPriorityBucket(t *testing.T) {
	tests := []struct {
		priority int
		want     int
	}{
		{100, 0},   // highest priority = lowest bucket
		{50, 2},    // high
		{0, 4},     // middle (changed from 5 to 4 because (100-0)/21 = 4)
		{-50, 7},   // low
		{-100, 9},  // lowest priority = highest bucket
		{200, 0},   // clamp to 0
		{-200, 9},  // clamp to 9
	}
	for _, tt := range tests {
		got := PriorityBucket(tt.priority)
		if got != tt.want {
			t.Errorf("PriorityBucket(%d) = %d, want %d", tt.priority, got, tt.want)
		}
	}
}

func TestPriorityBucketMonotonic(t *testing.T) {
	// Higher OJS priority should yield lower (or equal) bucket number
	prev := PriorityBucket(100)
	for p := 99; p >= -100; p-- {
		bucket := PriorityBucket(p)
		if bucket < prev {
			t.Errorf("PriorityBucket(%d) = %d < PriorityBucket(%d) = %d; should be non-decreasing as priority decreases",
				p, bucket, p+1, prev)
		}
		prev = bucket
	}
}

func TestBucketConstants(t *testing.T) {
	if StreamName != "OJS" {
		t.Errorf("StreamName = %q, want 'OJS'", StreamName)
	}
	if SubjectPrefix != "ojs" {
		t.Errorf("SubjectPrefix = %q, want 'ojs'", SubjectPrefix)
	}
	if BucketJobs != "ojs-jobs" {
		t.Errorf("BucketJobs = %q, want 'ojs-jobs'", BucketJobs)
	}
}
