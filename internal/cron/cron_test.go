package cron

import (
	"testing"
	"time"
)

func TestNext(t *testing.T) {
	base := time.Date(2026, 10, 20, 10, 30, 15, 0, time.UTC) // Tuesday
	cases := []struct {
		expr string
		want time.Time
	}{
		{"* * * * *", time.Date(2026, 10, 20, 10, 31, 0, 0, time.UTC)},
		{"0 9 * * *", time.Date(2026, 10, 21, 9, 0, 0, 0, time.UTC)},
		{"45 10 * * *", time.Date(2026, 10, 20, 10, 45, 0, 0, time.UTC)},
		{"*/15 * * * *", time.Date(2026, 10, 20, 10, 45, 0, 0, time.UTC)},
		{"0 9 * * 1-5", time.Date(2026, 10, 21, 9, 0, 0, 0, time.UTC)},
		{"0 9 * * 1", time.Date(2026, 10, 26, 9, 0, 0, 0, time.UTC)},
		{"0 9 * * 7", time.Date(2026, 10, 25, 9, 0, 0, 0, time.UTC)},
		{"0 0 1 * *", time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)},
		{"0 0 29 2 *", time.Date(2028, 2, 29, 0, 0, 0, 0, time.UTC)},
		{"@daily", time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC)},
		{"0 8,17 * * *", time.Date(2026, 10, 20, 17, 0, 0, 0, time.UTC)},
		{"5/20 * * * *", time.Date(2026, 10, 20, 10, 45, 0, 0, time.UTC)},
		// dom and dow both restricted: either matches (the 1st, or Fridays).
		{"0 0 1 * 5", time.Date(2026, 10, 23, 0, 0, 0, 0, time.UTC)},
	}
	for _, c := range cases {
		s, err := Parse(c.expr)
		if err != nil {
			t.Fatalf("%q: %v", c.expr, err)
		}
		if got := s.Next(base); !got.Equal(c.want) {
			t.Errorf("%q: got %v, want %v", c.expr, got, c.want)
		}
	}
}

func TestParseErrors(t *testing.T) {
	for _, expr := range []string{"", "* * * *", "60 * * * *", "* 24 * * *", "* * 0 * *", "*/0 * * * *", "a * * * *", "5-1 * * * *"} {
		if _, err := Parse(expr); err == nil {
			t.Errorf("%q: expected error", expr)
		}
	}
}

func TestNextImpossible(t *testing.T) {
	s, err := Parse("0 0 31 2 *")
	if err != nil {
		t.Fatal(err)
	}
	if got := s.Next(time.Now()); !got.IsZero() {
		t.Errorf("expected zero time, got %v", got)
	}
}
