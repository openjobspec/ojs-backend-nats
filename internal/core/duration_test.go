package core

import (
	"testing"
	"time"
)

func TestParseISO8601Duration(t *testing.T) {
	tests := []struct {
		input   string
		want    time.Duration
		wantErr bool
	}{
		{"PT1S", 1 * time.Second, false},
		{"PT5S", 5 * time.Second, false},
		{"PT1M", 1 * time.Minute, false},
		{"PT5M", 5 * time.Minute, false},
		{"PT1H", 1 * time.Hour, false},
		{"PT1H30M", 1*time.Hour + 30*time.Minute, false},
		{"PT1M30S", 1*time.Minute + 30*time.Second, false},
		{"PT1H1M1S", 1*time.Hour + 1*time.Minute + 1*time.Second, false},
		{"PT0.5S", 500 * time.Millisecond, false},

		// Invalid
		{"", 0, true},
		{"P1D", 0, true},
		{"1S", 0, true},
		{"PT", 0, true},   // zero duration
		{"PT0S", 0, true}, // zero duration
		{"invalid", 0, true},
	}

	for _, tt := range tests {
		got, err := ParseISO8601Duration(tt.input)
		if (err != nil) != tt.wantErr {
			t.Errorf("ParseISO8601Duration(%q) error = %v, wantErr %v", tt.input, err, tt.wantErr)
			continue
		}
		if !tt.wantErr && got != tt.want {
			t.Errorf("ParseISO8601Duration(%q) = %v, want %v", tt.input, got, tt.want)
		}
	}
}

func TestFormatISO8601Duration(t *testing.T) {
	tests := []struct {
		input time.Duration
		want  string
	}{
		{0, "PT0S"},
		{1 * time.Second, "PT1S"},
		{5 * time.Minute, "PT5M"},
		{1 * time.Hour, "PT1H"},
		{1*time.Hour + 30*time.Minute, "PT1H30M"},
		{1*time.Minute + 30*time.Second, "PT1M30S"},
		{1*time.Hour + 1*time.Minute + 1*time.Second, "PT1H1M1S"},
	}

	for _, tt := range tests {
		got := FormatISO8601Duration(tt.input)
		if got != tt.want {
			t.Errorf("FormatISO8601Duration(%v) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestRoundTrip(t *testing.T) {
	durations := []time.Duration{
		1 * time.Second,
		5 * time.Minute,
		1 * time.Hour,
		2*time.Hour + 30*time.Minute + 45*time.Second,
	}

	for _, d := range durations {
		formatted := FormatISO8601Duration(d)
		parsed, err := ParseISO8601Duration(formatted)
		if err != nil {
			t.Errorf("Round trip failed for %v: format=%q, parse error=%v", d, formatted, err)
			continue
		}
		if parsed != d {
			t.Errorf("Round trip mismatch for %v: format=%q, parsed=%v", d, formatted, parsed)
		}
	}
}
