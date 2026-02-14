package core

import (
	"testing"
	"time"
)

func TestCalculateBackoff_Exponential(t *testing.T) {
	policy := &RetryPolicy{
		InitialInterval:    "PT1S",
		BackoffCoefficient: 2.0,
		BackoffType:        "exponential",
	}

	tests := []struct {
		attempt int
		want    time.Duration
	}{
		{1, 1 * time.Second},  // 1 * 2^0
		{2, 2 * time.Second},  // 1 * 2^1
		{3, 4 * time.Second},  // 1 * 2^2
		{4, 8 * time.Second},  // 1 * 2^3
		{5, 16 * time.Second}, // 1 * 2^4
	}

	for _, tt := range tests {
		got := CalculateBackoff(policy, tt.attempt)
		if got != tt.want {
			t.Errorf("CalculateBackoff(exponential, attempt=%d) = %v, want %v", tt.attempt, got, tt.want)
		}
	}
}

func TestCalculateBackoff_Linear(t *testing.T) {
	policy := &RetryPolicy{
		InitialInterval: "PT2S",
		BackoffType:     "linear",
	}

	tests := []struct {
		attempt int
		want    time.Duration
	}{
		{1, 2 * time.Second},
		{2, 4 * time.Second},
		{3, 6 * time.Second},
	}

	for _, tt := range tests {
		got := CalculateBackoff(policy, tt.attempt)
		if got != tt.want {
			t.Errorf("CalculateBackoff(linear, attempt=%d) = %v, want %v", tt.attempt, got, tt.want)
		}
	}
}

func TestCalculateBackoff_Constant(t *testing.T) {
	policy := &RetryPolicy{
		InitialInterval: "PT5S",
		BackoffType:     "constant",
	}

	for attempt := 1; attempt <= 5; attempt++ {
		got := CalculateBackoff(policy, attempt)
		if got != 5*time.Second {
			t.Errorf("CalculateBackoff(constant, attempt=%d) = %v, want %v", attempt, got, 5*time.Second)
		}
	}
}

func TestCalculateBackoff_MaxInterval(t *testing.T) {
	policy := &RetryPolicy{
		InitialInterval:    "PT1S",
		BackoffCoefficient: 2.0,
		BackoffType:        "exponential",
		MaxInterval:        "PT10S",
	}

	// attempt 5 would be 16s but capped at 10s
	got := CalculateBackoff(policy, 5)
	if got != 10*time.Second {
		t.Errorf("CalculateBackoff with max interval = %v, want %v", got, 10*time.Second)
	}
}

func TestCalculateBackoff_NilPolicy(t *testing.T) {
	got := CalculateBackoff(nil, 1)
	if got == 0 {
		t.Error("CalculateBackoff(nil, 1) should return non-zero default backoff")
	}
}

func TestCalculateBackoff_Jitter(t *testing.T) {
	policy := &RetryPolicy{
		InitialInterval: "PT10S",
		BackoffType:     "constant",
		Jitter:          true,
	}

	seen := make(map[time.Duration]bool)
	for i := 0; i < 20; i++ {
		d := CalculateBackoff(policy, 1)
		seen[d] = true
		// Jitter range: 0.5x to 1.5x -> 5s to 15s
		if d < 5*time.Second || d > 15*time.Second {
			t.Errorf("CalculateBackoff with jitter = %v, outside expected range [5s, 15s]", d)
		}
	}
	if len(seen) < 2 {
		t.Error("CalculateBackoff with jitter produced no variation in 20 attempts")
	}
}

func TestCalculateBackoff_BackoffStrategy(t *testing.T) {
	// Test that BackoffStrategy alias works
	policy := &RetryPolicy{
		InitialInterval: "PT3S",
		BackoffStrategy: "constant",
	}

	got := CalculateBackoff(policy, 1)
	if got != 3*time.Second {
		t.Errorf("CalculateBackoff with BackoffStrategy = %v, want %v", got, 3*time.Second)
	}
}
