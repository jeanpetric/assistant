package store

import (
	"context"
	"time"
)

// LimitWindow is the last known usage of one subscription window.
type LimitWindow struct {
	UsedPercent float64
	ResetsAt    time.Time
}

// RateLimits returns the stored windows by name.
func (s *Store) RateLimits(ctx context.Context) (map[string]LimitWindow, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT name, used_percent, resets_at FROM rate_limits`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]LimitWindow{}
	for rows.Next() {
		var name string
		var w LimitWindow
		var resets int64
		if err := rows.Scan(&name, &w.UsedPercent, &resets); err != nil {
			return nil, err
		}
		w.ResetsAt = fromMillis(resets)
		out[name] = w
	}
	return out, rows.Err()
}

// SaveRateLimit replaces the stored usage of the named window.
func (s *Store) SaveRateLimit(ctx context.Context, name string, w LimitWindow) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO rate_limits (name, used_percent, resets_at, updated_at) VALUES (?, ?, ?, ?)
		ON CONFLICT (name) DO UPDATE SET used_percent = excluded.used_percent, resets_at = excluded.resets_at, updated_at = excluded.updated_at`,
		name, w.UsedPercent, w.ResetsAt.UnixMilli(), s.ts())
	return err
}
