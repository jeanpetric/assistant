package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

func TestApplyPulledMovesTheCursorWithTheChanges(t *testing.T) {
	hub, node := openSyncPair(t)
	ctx := context.Background()
	task, err := hub.CreateTask(ctx, Task{Title: "from the hub"})
	if err != nil {
		t.Fatal(err)
	}
	changes, err := hub.ExportChanges(ctx, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := node.ApplyPulled(ctx, changes, 7); err != nil {
		t.Fatal(err)
	}
	if got, err := node.Task(ctx, task.ID); err != nil || got.Title != "from the hub" {
		t.Fatalf("task %+v, %v", got, err)
	}
	if seq, err := node.PulledSeq(ctx); err != nil || seq != 7 {
		t.Fatalf("pulled seq %d, %v", seq, err)
	}
	// A bad page applies nothing and leaves the cursor where it was.
	bad := append(slices.Clone(changes), Change{Table: "sync_keys", Key: json.RawMessage(`["k"]`), Row: json.RawMessage(`{}`)})
	bad[0].Row = json.RawMessage(`{"id": 1, "title": "changed", "lane_id": 2}`)
	bad[0].ChangedAtMs++
	if err := node.ApplyPulled(ctx, bad, 9); err == nil {
		t.Fatal("bad page applied")
	}
	if seq, _ := node.PulledSeq(ctx); seq != 7 {
		t.Fatalf("pulled seq %d after a bad page", seq)
	}
	if got, _ := node.Task(ctx, task.ID); got.Title != "from the hub" {
		t.Fatalf("bad page changed the task to %q", got.Title)
	}
	if n, err := node.PendingChanges(ctx); err != nil || n != 0 {
		t.Fatalf("pulled changes pending: %d, %v", n, err)
	}
}

// openSyncPair opens two copies of one database, both recording changes.
func openSyncPair(t *testing.T) (*Store, *Store) {
	t.Helper()
	ctx := context.Background()
	a := openAt(t, filepath.Join(t.TempDir(), "a.db"))
	path := filepath.Join(t.TempDir(), "b.db")
	if _, err := a.Snapshot(ctx, path); err != nil {
		t.Fatal(err)
	}
	b := openAt(t, path)
	if _, err := b.db.Exec(`UPDATE sync_node SET node_id = 2`); err != nil {
		t.Fatal(err)
	}
	for _, s := range []*Store{a, b} {
		if err := s.SetSyncEnabled(ctx, true); err != nil {
			t.Fatal(err)
		}
	}
	return a, b
}

func openAt(t *testing.T, path string) *Store {
	t.Helper()
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestSnapshotStartsPullingAtItsSeq(t *testing.T) {
	s := openAt(t, filepath.Join(t.TempDir(), "hub.db"))
	ctx := context.Background()
	if err := s.SetSyncEnabled(ctx, true); err != nil {
		t.Fatal(err)
	}
	for _, title := range []string{"a", "b"} {
		if _, err := s.CreateTask(ctx, Task{Title: title}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.SetSyncedAt(ctx, time.UnixMilli(5000)); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "node.db")
	seq, err := s.Snapshot(ctx, path)
	if err != nil || seq == 0 {
		t.Fatalf("snapshot seq %d, %v", seq, err)
	}
	node := openAt(t, path)
	if got, err := node.PulledSeq(ctx); err != nil || got != seq {
		t.Fatalf("pulled seq %d, want %d (%v)", got, seq, err)
	}
	if at, err := node.SyncedAt(ctx); err != nil || !at.IsZero() {
		t.Fatalf("synced at %v, %v", at, err)
	}
}

func TestPendingChangesCountsRows(t *testing.T) {
	s := openAt(t, filepath.Join(t.TempDir(), "test.db"))
	ctx := context.Background()
	if n, err := s.PendingChanges(ctx); err != nil || n != 0 {
		t.Fatalf("pending %d, %v", n, err)
	}
	if err := s.SetSyncEnabled(ctx, true); err != nil {
		t.Fatal(err)
	}
	task, err := s.CreateTask(ctx, Task{Title: "one"})
	if err != nil {
		t.Fatal(err)
	}
	task.Title = "renamed"
	if err := s.UpdateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateTask(ctx, Task{Title: "two"}); err != nil {
		t.Fatal(err)
	}
	if n, err := s.PendingChanges(ctx); err != nil || n != 2 {
		t.Fatalf("pending %d, %v", n, err)
	}
	seq, _ := s.LatestSeq(ctx)
	if err := s.AckOutbox(ctx, seq); err != nil {
		t.Fatal(err)
	}
	if n, _ := s.PendingChanges(ctx); n != 0 {
		t.Fatalf("pending %d after ack", n)
	}
}

func TestLowOnIDs(t *testing.T) {
	s := openAt(t, filepath.Join(t.TempDir(), "test.db"))
	ctx := context.Background()
	low, err := s.LowOnIDs(ctx)
	if err != nil || len(low) != len(idTables) {
		t.Fatalf("low %v, %v", low, err)
	}
	for _, tbl := range idTableNames() {
		if err := s.AddBlock(ctx, tbl, 1000000, 1000000+leaseSize(tbl)-1); err != nil {
			t.Fatal(err)
		}
	}
	if low, err := s.LowOnIDs(ctx); err != nil || len(low) != 0 {
		t.Fatalf("low %v, %v", low, err)
	}
	// One id used: the block in hand is no longer full, so lease the next.
	if _, err := s.CreateTask(ctx, Task{Title: "one"}); err != nil {
		t.Fatal(err)
	}
	if low, err := s.LowOnIDs(ctx); err != nil || !slices.Equal(low, []string{"tasks"}) {
		t.Fatalf("low %v, %v", low, err)
	}
	if err := s.AddBlock(ctx, "tasks", 2000000, 2000999); err != nil {
		t.Fatal(err)
	}
	if low, err := s.LowOnIDs(ctx); err != nil || len(low) != 0 {
		t.Fatalf("low %v, %v", low, err)
	}
}

func TestSyncCursorMigrationKeepsData(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	n := migrationWith(t, "ADD COLUMN pulled_seq") // before pulled_seq and synced_at
	for _, m := range migrations[:n] {
		if _, err := db.Exec(m); err != nil {
			t.Fatal(err)
		}
	}
	for _, q := range []string{
		fmt.Sprintf(`PRAGMA user_version = %d`, n),
		`INSERT INTO tasks (id, title, lane_id, position, created_at, updated_at) VALUES (252, 'plan', 2, 0, 0, 0)`,
		`UPDATE sync_state SET enabled = 1`,
		`INSERT INTO sync_rows (tbl, pk_json, changed_at_ms, node_id) VALUES ('tasks', '[252]', 10, 1)`,
		`INSERT INTO sync_outbox (tbl, pk_json, op, changed_at_ms, node_id) VALUES ('tasks', '[252]', 'insert', 10, 1)`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(q, err)
		}
	}
	db.Close()
	s := openAt(t, path)
	ctx := context.Background()
	if task, err := s.Task(ctx, 252); err != nil || task.Title != "plan" {
		t.Fatalf("task %+v, %v", task, err)
	}
	if n, err := s.PendingChanges(ctx); err != nil || n != 1 {
		t.Fatalf("pending %d, %v", n, err)
	}
	if seq, err := s.PulledSeq(ctx); err != nil || seq != 0 {
		t.Fatalf("pulled seq %d, %v", seq, err)
	}
	if at, err := s.SyncedAt(ctx); err != nil || !at.IsZero() {
		t.Fatalf("synced at %v, %v", at, err)
	}
	var enabled bool
	if err := s.db.QueryRow(`SELECT enabled FROM sync_state`).Scan(&enabled); err != nil || !enabled {
		t.Fatalf("enabled %v, %v", enabled, err)
	}
}
