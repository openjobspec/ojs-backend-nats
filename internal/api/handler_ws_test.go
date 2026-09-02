package api

import "testing"

func TestFormatEventID(t *testing.T) {
	tests := []struct {
		id   uint64
		want string
	}{
		{0, "evt_0"},
		{1, "evt_1"},
		{42, "evt_42"},
		{9999, "evt_9999"},
		{10000, "evt_10000"},
		{123456789, "evt_123456789"},
	}
	for _, tt := range tests {
		if got := formatEventID(tt.id); got != tt.want {
			t.Errorf("formatEventID(%d) = %q, want %q", tt.id, got, tt.want)
		}
	}
}

// TestFormatEventID_Unique guards against the previous 4-digit truncation bug
// where IDs past 9,999 collided.
func TestFormatEventID_Unique(t *testing.T) {
	seen := make(map[string]uint64)
	for _, id := range []uint64{5, 10005, 20005, 999999999} {
		got := formatEventID(id)
		if prev, ok := seen[got]; ok {
			t.Fatalf("formatEventID collision: %d and %d both produced %q", prev, id, got)
		}
		seen[got] = id
	}
}
