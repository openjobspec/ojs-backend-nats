package core

import "testing"

func TestIsValidTransition(t *testing.T) {
	tests := []struct {
		from, to string
		want     bool
	}{
		// Valid transitions
		{StateAvailable, StateActive, true},
		{StateAvailable, StateCancelled, true},
		{StateScheduled, StateAvailable, true},
		{StateScheduled, StateCancelled, true},
		{StatePending, StateAvailable, true},
		{StatePending, StateCancelled, true},
		{StateActive, StateCompleted, true},
		{StateActive, StateRetryable, true},
		{StateActive, StateDiscarded, true},
		{StateActive, StateCancelled, true},
		{StateRetryable, StateAvailable, true},
		{StateRetryable, StateCancelled, true},

		// Invalid transitions
		{StateAvailable, StateCompleted, false},
		{StateActive, StateAvailable, false},
		{StateCompleted, StateActive, false},
		{StateCancelled, StateAvailable, false},
		{StateDiscarded, StateAvailable, false},

		// Unknown states
		{"unknown", StateActive, false},
		{StateActive, "unknown", false},
	}

	for _, tt := range tests {
		got := IsValidTransition(tt.from, tt.to)
		if got != tt.want {
			t.Errorf("IsValidTransition(%q, %q) = %v, want %v", tt.from, tt.to, got, tt.want)
		}
	}
}

func TestIsTerminalState(t *testing.T) {
	tests := []struct {
		state string
		want  bool
	}{
		{StateCompleted, true},
		{StateCancelled, true},
		{StateDiscarded, true},
		{StateAvailable, false},
		{StateActive, false},
		{StateScheduled, false},
		{StatePending, false},
		{StateRetryable, false},
	}

	for _, tt := range tests {
		got := IsTerminalState(tt.state)
		if got != tt.want {
			t.Errorf("IsTerminalState(%q) = %v, want %v", tt.state, got, tt.want)
		}
	}
}

func TestIsCancellableState(t *testing.T) {
	tests := []struct {
		state string
		want  bool
	}{
		{StateAvailable, true},
		{StateScheduled, true},
		{StatePending, true},
		{StateActive, true},
		{StateRetryable, true},
		{StateCompleted, false},
		{StateCancelled, false},
		{StateDiscarded, false},
	}

	for _, tt := range tests {
		got := IsCancellableState(tt.state)
		if got != tt.want {
			t.Errorf("IsCancellableState(%q) = %v, want %v", tt.state, got, tt.want)
		}
	}
}
