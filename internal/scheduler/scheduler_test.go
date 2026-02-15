package scheduler

import "testing"

func TestSchedulerStop_Idempotent(t *testing.T) {
	s := &Scheduler{
		stop: make(chan struct{}),
	}

	s.Stop()

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("Stop should be idempotent, panicked on second call: %v", r)
		}
	}()

	s.Stop()
}
