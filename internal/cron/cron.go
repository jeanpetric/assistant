// Package cron parses standard 5-field cron expressions
// (minute hour day-of-month month day-of-week) and computes next run times.
package cron

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

type Spec struct {
	minute, hour, dom, month, dow uint64 // bitsets
	domStar, dowStar              bool
}

var macros = map[string]string{
	"@hourly":  "0 * * * *",
	"@daily":   "0 0 * * *",
	"@weekly":  "0 0 * * 0",
	"@monthly": "0 0 1 * *",
	"@yearly":  "0 0 1 1 *",
}

func Parse(expr string) (*Spec, error) {
	expr = strings.TrimSpace(expr)
	if m, ok := macros[expr]; ok {
		expr = m
	}
	f := strings.Fields(expr)
	if len(f) != 5 {
		return nil, fmt.Errorf("cron: want 5 fields, got %d", len(f))
	}
	var s Spec
	var err error
	if s.minute, err = parseField(f[0], 0, 59); err != nil {
		return nil, fmt.Errorf("cron minute: %w", err)
	}
	if s.hour, err = parseField(f[1], 0, 23); err != nil {
		return nil, fmt.Errorf("cron hour: %w", err)
	}
	if s.dom, err = parseField(f[2], 1, 31); err != nil {
		return nil, fmt.Errorf("cron day-of-month: %w", err)
	}
	if s.month, err = parseField(f[3], 1, 12); err != nil {
		return nil, fmt.Errorf("cron month: %w", err)
	}
	if s.dow, err = parseField(f[4], 0, 7); err != nil {
		return nil, fmt.Errorf("cron day-of-week: %w", err)
	}
	if s.dow&(1<<7) != 0 { // 7 is Sunday too
		s.dow |= 1
	}
	s.domStar, s.dowStar = f[2] == "*", f[4] == "*"
	return &s, nil
}

func parseField(field string, lo, hi int) (uint64, error) {
	var bits uint64
	for _, part := range strings.Split(field, ",") {
		step := 1
		if i := strings.Index(part, "/"); i >= 0 {
			n, err := strconv.Atoi(part[i+1:])
			if err != nil || n <= 0 {
				return 0, fmt.Errorf("bad step in %q", part)
			}
			step, part = n, part[:i]
		}
		from, to := lo, hi
		switch {
		case part == "*":
		case strings.Contains(part, "-"):
			a, b, _ := strings.Cut(part, "-")
			var err1, err2 error
			from, err1 = strconv.Atoi(a)
			to, err2 = strconv.Atoi(b)
			if err1 != nil || err2 != nil {
				return 0, fmt.Errorf("bad range %q", part)
			}
		default:
			n, err := strconv.Atoi(part)
			if err != nil {
				return 0, fmt.Errorf("bad value %q", part)
			}
			from, to = n, n
			if step > 1 { // "5/15" means 5-hi/15
				to = hi
			}
		}
		if from < lo || to > hi || from > to {
			return 0, fmt.Errorf("%q out of range %d-%d", part, lo, hi)
		}
		for v := from; v <= to; v += step {
			bits |= 1 << uint(v)
		}
	}
	return bits, nil
}

func has(bits uint64, v int) bool { return bits&(1<<uint(v)) != 0 }

func (s *Spec) dayMatches(t time.Time) bool {
	dom, dow := has(s.dom, t.Day()), has(s.dow, int(t.Weekday()))
	if s.domStar || s.dowStar {
		return dom && dow
	}
	return dom || dow // standard cron: either restricted field may match
}

// Next returns the first matching minute strictly after t, in t's location.
// It returns the zero time if nothing matches within five years.
func (s *Spec) Next(t time.Time) time.Time {
	t = t.Truncate(time.Minute).Add(time.Minute)
	limit := t.AddDate(5, 0, 0)
	for t.Before(limit) {
		if !has(s.month, int(t.Month())) {
			t = time.Date(t.Year(), t.Month()+1, 1, 0, 0, 0, 0, t.Location())
			continue
		}
		if !s.dayMatches(t) {
			t = time.Date(t.Year(), t.Month(), t.Day()+1, 0, 0, 0, 0, t.Location())
			continue
		}
		if !has(s.hour, t.Hour()) {
			t = time.Date(t.Year(), t.Month(), t.Day(), t.Hour()+1, 0, 0, 0, t.Location())
			continue
		}
		if !has(s.minute, t.Minute()) {
			t = t.Add(time.Minute)
			continue
		}
		return t
	}
	return time.Time{}
}
