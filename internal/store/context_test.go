package store

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
)

func TestSessionContext(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	x, err := s.CreateSession(ctx, Session{Title: "s", Status: StatusIdle, Mode: "plan"})
	if err != nil {
		t.Fatal(err)
	}
	before, _ := s.Session(ctx, x.ID)
	if before.ContextTokens != 0 || before.ContextWindow != 0 {
		t.Fatalf("new session = %+v", before)
	}
	// Tokens arrive during a turn, the window at its end; a zero keeps the value.
	if err := s.SetSessionContext(ctx, x.ID, 5000, 0); err != nil {
		t.Fatal(err)
	}
	s.SetSessionContext(ctx, x.ID, 0, 200000)
	s.SetSessionContext(ctx, x.ID, 7000, 0)
	got, _ := s.Session(ctx, x.ID)
	if got.ContextTokens != 7000 || got.ContextWindow != 200000 {
		t.Fatalf("session = %+v", got)
	}
	if !got.UpdatedAt.Equal(before.UpdatedAt) {
		t.Fatalf("updated_at moved: %v -> %v", before.UpdatedAt, got.UpdatedAt)
	}
}

func TestContextFullnessMigrationKeepsData(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	const n = 19 // migrations before context fullness
	for _, m := range migrations[:n] {
		if _, err := db.Exec(m); err != nil {
			t.Fatal(err)
		}
	}
	for _, q := range []string{
		fmt.Sprintf(`PRAGMA user_version = %d`, n),
		`INSERT INTO tasks (id, title, lane_id, position, created_at, updated_at) VALUES (1, 't', 1, 0, 0, 0)`,
		`INSERT INTO sessions (id, task_id, title, status, mode, claude_session_id, cost_usd, needs_input, created_at, updated_at) VALUES (7, 1, 'work', 'idle', 'plan', 'c-7', 1.5, 'asked', 0, 5)`,
		`INSERT INTO messages (session_id, role, kind, content, created_at) VALUES (7, 'user', 'text', 'hi', 0)`,
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
	x, err := s.Session(ctx, 7)
	if err != nil || x.TaskID != 1 || x.Title != "work" || x.ClaudeSessionID != "c-7" || x.CostUSD != 1.5 || x.NeedsInput != "asked" ||
		x.ContextTokens != 0 || x.ContextWindow != 0 {
		t.Fatalf("session = %+v, %v", x, err)
	}
	if msgs, _ := s.Messages(ctx, 7, 0); len(msgs) != 1 {
		t.Fatalf("messages = %+v", msgs)
	}
}
