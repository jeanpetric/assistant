package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

func TestSyncKeys(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	now := time.UnixMilli(1_000_000)
	s.SetClock(func() time.Time { return now })

	if _, err := s.SyncKeyNode(ctx, "a"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown key: %v", err)
	}
	if _, err := s.AddSyncKey(ctx, "a", "laptop", 2); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddSyncKey(ctx, "a", "again", 3); err == nil {
		t.Fatal("same key added twice")
	}
	if _, err := s.AddSyncKey(ctx, "b", "no node", 0); err == nil {
		t.Fatal("key without a node added")
	}
	if node, err := s.SyncKeyNode(ctx, "a"); err != nil || node != 2 {
		t.Fatalf("node = %d, %v", node, err)
	}

	now = now.Add(time.Minute)
	if err := s.RevokeSyncKey(ctx, "a"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SyncKeyNode(ctx, "a"); !errors.Is(err, ErrKeyRevoked) {
		t.Fatalf("revoked key: %v", err)
	}
	now = now.Add(time.Minute)
	if err := s.RevokeSyncKey(ctx, "a"); err != nil {
		t.Fatal(err)
	}
	if err := s.RevokeSyncKey(ctx, "x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("revoke unknown: %v", err)
	}
	keys, err := s.SyncKeys(ctx)
	if err != nil || len(keys) != 1 {
		t.Fatalf("keys = %+v, %v", keys, err)
	}
	k := keys[0]
	if k.Name != "laptop" || k.NodeID != 2 || k.CreatedAt.UnixMilli() != 1_000_000 ||
		k.RevokedAt == nil || k.RevokedAt.UnixMilli() != 1_060_000 {
		t.Fatalf("key = %+v", k)
	}
}

// Keys stay local: adding one records nothing to push.
func TestSyncKeysNotRecorded(t *testing.T) {
	s := openSynced(t)
	if _, err := s.AddSyncKey(context.Background(), "a", "laptop", 2); err != nil {
		t.Fatal(err)
	}
	if got := outbox(t, s); len(got) != 0 {
		t.Fatalf("outbox = %v", got)
	}
}

func TestSyncKeysMigrationKeepsData(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	const n = 34 // migrations before sync keys
	for _, m := range migrations[:n] {
		if _, err := db.Exec(m); err != nil {
			t.Fatal(err)
		}
	}
	for _, q := range []string{
		fmt.Sprintf(`PRAGMA user_version = %d`, n),
		`INSERT INTO tasks (id, title, lane_id, position, created_at, updated_at) VALUES (252, 'plan', 2, 0, 0, 0)`,
		`INSERT INTO sessions (id, task_id, status, mode, created_at, updated_at) VALUES (291, 252, 'idle', 'plan', 0, 0)`,
		`INSERT INTO messages (id, session_id, role, kind, content, created_at) VALUES (9000, 291, 'user', 'text', 'hi', 0)`,
		`UPDATE sync_state SET enabled = 1`,
		`INSERT INTO sync_rows (tbl, pk_json, changed_at_ms, node_id) VALUES ('tasks', '[252]', 5, 1)`,
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
	task, err := s.Task(ctx, 252)
	if err != nil || task.Title != "plan" {
		t.Fatalf("task = %+v, %v", task, err)
	}
	if msgs, _ := s.Messages(ctx, 291, 0); len(msgs) != 1 || msgs[0].Content != "hi" {
		t.Fatalf("messages = %+v", msgs)
	}
	var enabled, rows int
	s.db.QueryRow(`SELECT enabled, (SELECT COUNT(*) FROM sync_rows) FROM sync_state`).Scan(&enabled, &rows)
	if enabled != 1 || rows != 1 {
		t.Fatalf("enabled %d, sync rows %d", enabled, rows)
	}
	if keys, err := s.SyncKeys(ctx); err != nil || len(keys) != 0 {
		t.Fatalf("keys = %+v, %v", keys, err)
	}
}
