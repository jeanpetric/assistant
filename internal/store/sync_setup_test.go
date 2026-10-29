package store

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBlank(t *testing.T) {
	s := openAt(t, filepath.Join(t.TempDir(), "test.db"))
	ctx := context.Background()
	if blank, err := s.Blank(ctx); err != nil || !blank {
		t.Fatalf("new database blank %v, %v", blank, err)
	}
	// Defaults edited or gone: still nothing of its own.
	if err := s.UpdateLane(ctx, 1, "Ideas", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteLane(ctx, 5); err != nil {
		t.Fatal(err)
	}
	if blank, err := s.Blank(ctx); err != nil || !blank {
		t.Fatalf("edited defaults blank %v, %v", blank, err)
	}
	if _, err := s.CreateTask(ctx, Task{Title: "mine"}); err != nil {
		t.Fatal(err)
	}
	if blank, err := s.Blank(ctx); err != nil || blank {
		t.Fatalf("with a task blank %v, %v", blank, err)
	}
}

// outboxEntries lists the outbox as "op tbl key" in seq order.
func outboxEntries(t *testing.T, s *Store) []string {
	t.Helper()
	rows, err := s.db.Query(`SELECT op || ' ' || tbl || ' ' || pk_json FROM sync_outbox ORDER BY seq`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var e string
		if err := rows.Scan(&e); err != nil {
			t.Fatal(err)
		}
		out = append(out, e)
	}
	return out
}

func indexOf(list []string, e string) int {
	for i, x := range list {
		if x == e {
			return i
		}
	}
	return -1
}

func TestStartSeedQueuesEverything(t *testing.T) {
	s := openAt(t, filepath.Join(t.TempDir(), "test.db"))
	ctx := context.Background()
	if err := s.DeleteLane(ctx, 1); err != nil {
		t.Fatal(err)
	}
	task, err := s.CreateTask(ctx, Task{Title: "mine", LaneID: 2})
	if err != nil {
		t.Fatal(err)
	}
	parent, err := s.CreateSession(ctx, Session{TaskID: task.ID, Title: "parent"})
	if err != nil {
		t.Fatal(err)
	}
	child, err := s.CreateSession(ctx, Session{TaskID: task.ID, Title: "child", ParentID: &parent.ID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddMessage(ctx, Message{SessionID: child.ID, Role: "user", Kind: "text", Content: "hi"}); err != nil {
		t.Fatal(err)
	}
	// Leftovers of an earlier try at syncing go.
	if err := s.AddBlock(ctx, "tasks", 5000, 5999); err != nil {
		t.Fatal(err)
	}

	if _, err := s.StartSeed(ctx, 1); err == nil {
		t.Fatal("seeded as the hub's node")
	}
	n, err := s.StartSeed(ctx, 2)
	if err != nil {
		t.Fatal(err)
	}
	if node, _ := s.NodeID(ctx); node != 2 {
		t.Fatalf("node %d", node)
	}
	entries := outboxEntries(t, s)
	if n != len(entries) {
		t.Fatalf("%d rows queued, %d entries", n, len(entries))
	}
	var rows int
	for tbl := range syncTables {
		var c int
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM "` + tbl + `"`).Scan(&c); err != nil {
			t.Fatal(err)
		}
		rows += c
	}
	// Every row, and the default lane this database deleted.
	if len(entries) != rows+1 || entries[0] != "delete lanes [1]" {
		t.Fatalf("%d entries for %d rows: %v", len(entries), rows, entries)
	}
	before := func(a, b string) {
		t.Helper()
		if i, j := indexOf(entries, a), indexOf(entries, b); i < 0 || j < 0 || i > j {
			t.Fatalf("%q at %d, %q at %d", a, i, b, j)
		}
	}
	before("insert lanes [2]", "insert tasks [1]")
	before("insert projects [1]", "insert tasks [1]")
	before("insert tasks [1]", "insert sessions [1]")
	before("insert sessions [1]", "insert sessions [2]")
	before("insert sessions [2]", "insert messages [1]")
	before("insert skills [1]", "insert skill_automations [1]")
	var blocks int
	s.db.QueryRow(`SELECT COUNT(*) FROM id_blocks`).Scan(&blocks)
	if blocks != 0 {
		t.Fatalf("%d id blocks left", blocks)
	}
	// Changes from now on are recorded on top.
	task.Title = "renamed"
	if err := s.UpdateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	if got := outboxEntries(t, s); got[len(got)-1] != "update tasks [1]" {
		t.Fatalf("last entry %q", got[len(got)-1])
	}
	changes, err := s.ExportChanges(ctx, 0, 0)
	if err != nil || len(changes) != rows+1 {
		t.Fatalf("%d changes, %v", len(changes), err)
	}
	for _, c := range changes {
		if c.NodeID != 2 {
			t.Fatalf("change %+v", c)
		}
	}
}

// joinable makes a hub with data and its snapshot.
func joinable(t *testing.T) (hub *Store, path string, seq int64) {
	t.Helper()
	ctx := context.Background()
	hub = openAt(t, filepath.Join(t.TempDir(), "hub.db"))
	if err := hub.SetSyncEnabled(ctx, true); err != nil {
		t.Fatal(err)
	}
	hub.SetHub(true)
	if err := hub.DeleteLane(ctx, 1); err != nil {
		t.Fatal(err)
	}
	task, err := hub.CreateTask(ctx, Task{Title: "on the hub", LaneID: 2})
	if err != nil {
		t.Fatal(err)
	}
	sess, err := hub.CreateSession(ctx, Session{TaskID: task.ID, Title: "s"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := hub.AddMessage(ctx, Message{SessionID: sess.ID, Role: "user", Kind: "text", Content: "hi"}); err != nil {
		t.Fatal(err)
	}
	if _, err := hub.AddSyncKey(ctx, "key", "laptop", 2); err != nil {
		t.Fatal(err)
	}
	path = filepath.Join(t.TempDir(), "snapshot.db")
	seq, err = hub.Snapshot(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	return hub, path, seq
}

func TestJoinTakesTheSnapshot(t *testing.T) {
	hub, path, seq := joinable(t)
	ctx := context.Background()
	s := openAt(t, filepath.Join(t.TempDir(), "node.db"))
	if err := s.UpdateLane(ctx, 2, "renamed here", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.AddBlock(ctx, "tasks", 5000, 5999); err != nil {
		t.Fatal(err)
	}
	if err := s.Join(ctx, path, 3, seq); err != nil {
		t.Fatal(err)
	}
	same(t, hub, s)
	if node, _ := s.NodeID(ctx); node != 3 {
		t.Fatalf("node %d", node)
	}
	if got, _ := s.PulledSeq(ctx); got != seq {
		t.Fatalf("pulled seq %d, want %d", got, seq)
	}
	if n, _ := s.PendingChanges(ctx); n != 0 {
		t.Fatalf("%d pending after join", n)
	}
	var blocks, keys int
	s.db.QueryRow(`SELECT COUNT(*) FROM id_blocks`).Scan(&blocks)
	s.db.QueryRow(`SELECT COUNT(*) FROM sync_keys`).Scan(&keys)
	if blocks != 0 || keys != 0 {
		t.Fatalf("%d id blocks, %d keys", blocks, keys)
	}
	// The hub's stamps came along: an older change loses, as on the hub.
	var key string
	if err := hub.db.QueryRow(`SELECT pk_json FROM sync_rows WHERE tbl = 'tasks'`).Scan(&key); err != nil {
		t.Fatal(err)
	}
	ms, _ := syncRow(t, hub, "tasks", key)
	if got, _ := syncRow(t, s, "tasks", key); got != ms {
		t.Fatalf("stamp %d, hub's %d", got, ms)
	}
	// Changes are recorded as the node's.
	if _, err := s.CreateTask(ctx, Task{Title: "joined", LaneID: 2}); err != nil {
		t.Fatal(err)
	}
	changes, err := s.ExportChanges(ctx, 0, 0)
	if err != nil || len(changes) != 1 || changes[0].NodeID != 3 {
		t.Fatalf("changes %+v, %v", changes, err)
	}
}

func TestJoinRefusesData(t *testing.T) {
	_, path, seq := joinable(t)
	ctx := context.Background()
	s := openAt(t, filepath.Join(t.TempDir(), "node.db"))
	task, err := s.CreateTask(ctx, Task{Title: "mine"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Join(ctx, path, 3, seq); !errors.Is(err, ErrNotBlank) {
		t.Fatalf("join: %v", err)
	}
	if got, err := s.Task(ctx, task.ID); err != nil || got.Title != "mine" {
		t.Fatalf("task %+v, %v", got, err)
	}
	if node, _ := s.NodeID(ctx); node != 1 {
		t.Fatalf("node %d", node)
	}
}

func TestJoinWantsTheSameSchema(t *testing.T) {
	_, path, seq := joinable(t)
	ctx := context.Background()
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`PRAGMA user_version = 999`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	s := openAt(t, filepath.Join(t.TempDir(), "node.db"))
	before, _ := s.SyncDump(ctx)
	if err := s.Join(ctx, path, 3, seq); err == nil || !strings.Contains(err.Error(), "schema") {
		t.Fatalf("join: %v", err)
	}
	if after, _ := s.SyncDump(ctx); after != before {
		t.Fatal("a failed join changed the database")
	}
	// The connection is usable afterwards, the snapshot detached.
	if _, err := s.CreateTask(ctx, Task{Title: "after"}); err != nil {
		t.Fatal(err)
	}
}

func TestBackup(t *testing.T) {
	dir := t.TempDir()
	s := openAt(t, filepath.Join(dir, "test.db"))
	ctx := context.Background()
	if _, err := s.CreateTask(ctx, Task{Title: "kept"}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "backup.db")
	if err := s.Backup(ctx, path); err != nil {
		t.Fatal(err)
	}
	if err := s.Backup(ctx, path); err == nil {
		t.Fatal("backup overwrote a file")
	}
	b := openAt(t, path)
	want, _ := s.SyncDump(ctx)
	if got, _ := b.SyncDump(ctx); got != want || !strings.Contains(got, "kept") {
		t.Fatalf("backup holds\n%s", got)
	}
	if _, err := os.Stat(path + ".partial"); !os.IsNotExist(err) {
		t.Fatalf("partial file left: %v", err)
	}
}

func TestAuthorizeSyncKey(t *testing.T) {
	s := openAt(t, filepath.Join(t.TempDir(), "hub.db"))
	ctx := context.Background()
	a, err := s.AuthorizeSyncKey(ctx, "ka", "laptop")
	if err != nil || a.NodeID != 2 {
		t.Fatalf("%+v, %v", a, err)
	}
	b, err := s.AuthorizeSyncKey(ctx, "kb", "desktop")
	if err != nil || b.NodeID != 3 {
		t.Fatalf("%+v, %v", b, err)
	}
	for _, bad := range [][2]string{{"ka", "again"}, {"kc", "laptop"}, {"kc", ""}} {
		if _, err := s.AuthorizeSyncKey(ctx, bad[0], bad[1]); err == nil {
			t.Errorf("%v authorized", bad)
		}
	}
	// A revoked key frees its name, not its node.
	if err := s.RevokeSyncKey(ctx, "kb"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AuthorizeSyncKey(ctx, "kb", "desktop"); err == nil {
		t.Fatal("revoked key authorized again")
	}
	c, err := s.AuthorizeSyncKey(ctx, "kc", "desktop")
	if err != nil || c.NodeID != 4 {
		t.Fatalf("%+v, %v", c, err)
	}
	if node, err := s.SyncKeyNode(ctx, "kc"); err != nil || node != 4 {
		t.Fatalf("node %d, %v", node, err)
	}
}

func TestJoinCopiesTheColumnsBothHave(t *testing.T) {
	_, path, seq := joinable(t)
	ctx := context.Background()
	// As on a database that went through an early build.
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`ALTER TABLE sessions DROP COLUMN limit_5h_used`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	s := openAt(t, filepath.Join(t.TempDir(), "node.db"))
	if err := s.Join(ctx, path, 3, seq); err != nil {
		t.Fatal(err)
	}
	sessions, err := s.Sessions(ctx, 0, "")
	if err != nil || len(sessions) != 1 || sessions[0].Title != "s" {
		t.Fatalf("sessions %+v, %v", sessions, err)
	}
}
