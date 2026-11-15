package sql

import (
	"testing"
	"time"
)

func TestValueToString(t *testing.T) {
	tests := []struct {
		input any
		want  string
	}{
		{nil, ""},
		{"hello", "hello"},
		{int(42), "42"},
		{int64(123456789), "123456789"},
		{float64(3.14159), "3.14159"},
		{true, "true"},
		{false, "false"},
		{time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC), "2026-10-17T12:00:00Z"},
	}

	for _, tt := range tests {
		got := ValueToString(tt.input)
		if got != tt.want {
			t.Errorf("ValueToString(%v) = %q, want %q", tt.input, got, tt.want)
		}
	}
}
