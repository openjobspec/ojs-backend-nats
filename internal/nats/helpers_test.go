package nats

import (
	"testing"
)

func TestMatchesPatternExact(t *testing.T) {
	tests := []struct {
		s, pattern string
		want       bool
	}{
		{"hello", "hello", true},
		{"hello", "world", false},
		{"", "", true},
	}
	for _, tt := range tests {
		got := matchesPattern(tt.s, tt.pattern)
		if got != tt.want {
			t.Errorf("matchesPattern(%q, %q) = %v, want %v", tt.s, tt.pattern, got, tt.want)
		}
	}
}

func TestMatchesPatternRegex(t *testing.T) {
	tests := []struct {
		s, pattern string
		want       bool
	}{
		{"email.send", "email\\..*", true},
		{"email.send", "payment\\..*", false},
		{"data.sync.full", "data\\..*", true},
		{"test123", "test\\d+", true},
		{"test", "test\\d+", false},
	}
	for _, tt := range tests {
		got := matchesPattern(tt.s, tt.pattern)
		if got != tt.want {
			t.Errorf("matchesPattern(%q, %q) = %v, want %v", tt.s, tt.pattern, got, tt.want)
		}
	}
}

func TestMatchesPatternCaching(t *testing.T) {
	// Call twice to exercise cache path
	if !matchesPattern("abc", "abc") {
		t.Error("first call should match")
	}
	if !matchesPattern("abc", "abc") {
		t.Error("cached call should still match")
	}
}

func TestCurrentTime(t *testing.T) {
	t1 := currentTime()
	t2 := currentTime()
	if t2.Before(t1) {
		t.Error("currentTime() should be monotonically non-decreasing")
	}
}
