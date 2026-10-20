package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

func TestRateLimits(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if got, err := s.RateLimits(ctx); err != nil || len(got) != 0 {
		t.Fatalf("empty: %+v %v", got, err)
	}
	resets := time.UnixMilli(time.Now().Add(time.Hour).UnixMilli())
	s.SaveRateLimit(ctx, "five_hour", LimitWindow{UsedPercent: 10, ResetsAt: resets})
	s.SaveRateLimit(ctx, "seven_day", LimitWindow{UsedPercent: 3, ResetsAt: resets})
	if err := s.SaveRateLimit(ctx, "five_hour", LimitWindow{UsedPercent: 42.5, ResetsAt: resets}); err != nil {
		t.Fatal(err)
	}
	s.Close()

	s, err = Open(path) // survives a restart
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	got, err := s.RateLimits(ctx)
	if err != nil || len(got) != 2 || got["five_hour"].UsedPercent != 42.5 || !got["five_hour"].ResetsAt.Equal(resets) ||
		got["seven_day"].UsedPercent != 3 {
		t.Fatalf("limits = %+v, %v", got, err)
	}
}

func TestRateLimitsMigrationKeepsData(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range migrations[:5] {
		if _, err := db.Exec(m); err != nil {
			t.Fatal(err)
		}
	}
	for _, q := range []string{
		`PRAGMA user_version = 5`,
		`INSERT INTO tasks (id, title, lane_id, position, created_at, updated_at, archived_at, lane_changed_at) VALUES (1, 'old', 5, 0, 1000, 5000, 6000, 5000)`,
		`INSERT INTO task_fields (task_id, key, value) VALUES (1, 'slack', 'https://slack/x')`,
		`INSERT INTO sessions (id, task_id, status, mode, created_at, updated_at) VALUES (7, 1, 'idle', 'plan', 0, 0)`,
		`INSERT INTO messages (session_id, role, kind, content, created_at) VALUES (7, 'user', 'text', 'hi', 0)`,
		`UPDATE settings SET archive_after_minutes = 60`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(q, err)
		}
	}
	db.Close()

	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	if x, err := s.Task(ctx, 1); err != nil || x.Title != "old" || x.ArchivedAt == nil || x.Fields["slack"] != "https://slack/x" {
		t.Fatalf("task = %+v, %v", x, err)
	}
	if msgs, _ := s.Messages(ctx, 7, 0); len(msgs) != 1 || msgs[0].Content != "hi" {
		t.Fatalf("messages = %+v", msgs)
	}
	if x, _ := s.Settings(ctx); x.ArchiveAfterMinutes != 60 || x.ArchiveLaneID == nil || *x.ArchiveLaneID != 5 {
		t.Fatalf("settings = %+v", x)
	}
	if got, err := s.RateLimits(ctx); err != nil || len(got) != 0 {
		t.Fatalf("limits = %+v, %v", got, err)
	}
}
