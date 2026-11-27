package omop

import (
	"math/big"
	"testing"
	"time"

	"github.com/colabbio/medvfs/internal/config"
	"github.com/jackc/pgx/v5/pgtype"
)

type stringerStub struct{ val string }

func (s stringerStub) String() string { return s.val }

func TestOMOPAdapter_IDAndValueToString(t *testing.T) {
	a := New(config.SourceConfig{ID: "omop_test"}, "postgres://unused")
	if a.ID() != "omop_test" {
		t.Fatalf("unexpected ID: %s", a.ID())
	}

	ts := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)
	num := pgtype.Numeric{Int: big.NewInt(1234), Exp: -2, Valid: true}
	invalidNum := pgtype.Numeric{Valid: false}

	cases := []struct {
		in   any
		want string
	}{
		{nil, ""},
		{"abc", "abc"},
		{42, "42"},
		{int64(99), "99"},
		{float64(3.5), "3.5"},
		{true, "true"},
		{ts, "2026-11-27T12:00:00Z"},
		{num, "12.34"},
		{invalidNum, ""},
		{stringerStub{"custom"}, "custom"},
	}

	for _, tc := range cases {
		if got := valueToString(tc.in); got != tc.want {
			t.Errorf("valueToString(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
