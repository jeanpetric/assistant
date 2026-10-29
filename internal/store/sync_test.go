package store

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"slices"
	"testing"
)

// outbox returns the changes waiting to be pushed, oldest first, as
// "op tbl pk", and empties it.
func outbox(t *testing.T, s *Store) []string {
	t.Helper()
	rows, err := s.db.Query(`SELECT op, tbl, pk_json, node_id FROM sync_outbox ORDER BY seq`)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for rows.Next() {
		var op, tbl, pk string
		var node int64
		if err := rows.Scan(&op, &tbl, &pk, &node); err != nil {
			t.Fatal(err)
		}
		if node != 1 {
			t.Errorf("%s %s %s from node %d", op, tbl, pk, node)
		}
		out = append(out, op+" "+tbl+" "+pk)
	}
	rows.Close()
	if _, err := s.db.Exec(`DELETE FROM sync_outbox`); err != nil {
		t.Fatal(err)
	}
	return out
}

func exec(t *testing.T, s *Store, qs ...string) {
	t.Helper()
	for _, q := range qs {
		if _, err := s.db.Exec(q); err != nil {
			t.Fatal(q, err)
		}
	}
}

// syncRow is a row's latest change: its stamp and whether it is a tombstone.
func syncRow(t *testing.T, s *Store, tbl, pk string) (changedAt int64, deleted bool) {
	t.Helper()
	err := s.db.QueryRow(`SELECT changed_at_ms, deleted FROM sync_rows WHERE tbl = ? AND pk_json = ?`, tbl, pk).
		Scan(&changedAt, &deleted)
	if err != nil {
		t.Fatalf("sync_rows %s %s: %v", tbl, pk, err)
	}
	return changedAt, deleted
}

func openSynced(t *testing.T) *Store {
	t.Helper()
	s := open(t)
	if err := s.SetSyncEnabled(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	return s
}

// Every table is either synced or local, and a synced table's key is the
// one its rows are named by.
func TestSyncTablesCoverEveryTable(t *testing.T) {
	s := open(t)
	rows, err := s.db.Query(`SELECT m.name, c.name FROM sqlite_master m, pragma_table_info(m.name) c
		WHERE m.type = 'table' AND m.name != 'sqlite_sequence' AND c.pk > 0 ORDER BY m.name, c.pk`)
	if err != nil {
		t.Fatal(err)
	}
	keys := map[string][]string{}
	for rows.Next() {
		var tbl, col string
		if err := rows.Scan(&tbl, &col); err != nil {
			t.Fatal(err)
		}
		keys[tbl] = append(keys[tbl], col)
	}
	rows.Close()
	for tbl, cols := range keys {
		synced, ok := syncTables[tbl]
		switch {
		case ok && localTables[tbl]:
			t.Errorf("%s is both synced and local", tbl)
		case ok && !slices.Equal(synced, cols):
			t.Errorf("%s is synced by %v, its key is %v", tbl, synced, cols)
		case !ok && !localTables[tbl]:
			t.Errorf("%s is neither synced nor local", tbl)
		}
	}
	if len(keys) != len(syncTables)+len(localTables) {
		t.Errorf("tables %v, synced %d, local %d", keys, len(syncTables), len(localTables))
	}
}

func TestSyncRecordsNothingWhileOff(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	task, err := s.CreateTask(ctx, Task{Title: "t", Fields: map[string]string{"slack": "https://x"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteTask(ctx, task.ID); err != nil {
		t.Fatal(err)
	}
	var n int
	s.db.QueryRow(`SELECT (SELECT COUNT(*) FROM sync_outbox) + (SELECT COUNT(*) FROM sync_rows)`).Scan(&n)
	if n != 0 {
		t.Fatalf("%d changes recorded with sync off", n)
	}
}

func TestSyncRecordsNothingWhileApplying(t *testing.T) {
	s := openSynced(t)
	exec(t, s, `UPDATE sync_state SET applying = 1`,
		`INSERT INTO tasks (id, title, lane_id, position, created_at, updated_at) VALUES (7, 'remote', 2, 0, 0, 0)`,
		`UPDATE tasks SET title = 'remote 2' WHERE id = 7`,
		`DELETE FROM tasks WHERE id = 7`)
	if got := outbox(t, s); len(got) != 0 {
		t.Fatalf("outbox = %v", got)
	}
	var n int
	s.db.QueryRow(`SELECT COUNT(*) FROM sync_rows`).Scan(&n)
	if n != 0 {
		t.Fatalf("%d rows recorded while applying", n)
	}
	exec(t, s, `UPDATE sync_state SET applying = 0`,
		`INSERT INTO tasks (id, title, lane_id, position, created_at, updated_at) VALUES (8, 'local', 2, 0, 0, 0)`)
	if got := outbox(t, s); !slices.Equal(got, []string{"insert tasks [8]"}) {
		t.Fatalf("outbox = %v", got)
	}
}

func TestSyncRecordsEachOperation(t *testing.T) {
	s := openSynced(t)
	exec(t, s, `INSERT INTO tasks (id, title, lane_id, position, created_at, updated_at) VALUES (7, 't', 2, 0, 0, 0)`)
	if got := outbox(t, s); !slices.Equal(got, []string{"insert tasks [7]"}) {
		t.Fatalf("insert: %v", got)
	}
	inserted, deleted := syncRow(t, s, "tasks", "[7]")
	if deleted || inserted == 0 {
		t.Fatalf("after insert: %d %v", inserted, deleted)
	}
	exec(t, s, `UPDATE tasks SET title = 'u' WHERE id = 7`)
	if got := outbox(t, s); !slices.Equal(got, []string{"update tasks [7]"}) {
		t.Fatalf("update: %v", got)
	}
	updated, deleted := syncRow(t, s, "tasks", "[7]")
	if deleted || updated <= inserted {
		t.Fatalf("after update: %d (was %d) %v", updated, inserted, deleted)
	}
	exec(t, s, `DELETE FROM tasks WHERE id = 7`)
	if got := outbox(t, s); !slices.Equal(got, []string{"delete tasks [7]"}) {
		t.Fatalf("delete: %v", got)
	}
	if gone, deleted := syncRow(t, s, "tasks", "[7]"); !deleted || gone <= updated {
		t.Fatalf("after delete: %d (was %d) %v", gone, updated, deleted)
	}
	// The outbox entry carries the row's stamp.
	exec(t, s, `UPDATE settings SET archive_after_minutes = 5`)
	var entry, row int64
	s.db.QueryRow(`SELECT changed_at_ms FROM sync_outbox WHERE tbl = 'settings'`).Scan(&entry)
	row, _ = syncRow(t, s, "settings", "[1]")
	if entry != row || entry == 0 {
		t.Fatalf("outbox stamp %d, row stamp %d", entry, row)
	}
}

// A local change is stamped after the one it replaces, even one stamped
// ahead of this node's clock.
func TestSyncStampsAfterTheReplacedChange(t *testing.T) {
	s := openSynced(t)
	const ahead = int64(1) << 50
	exec(t, s, fmt.Sprintf(`INSERT INTO sync_rows (tbl, pk_json, changed_at_ms, node_id, deleted) VALUES ('lanes', '[2]', %d, 3, 0)`, ahead),
		`UPDATE lanes SET name = 'Next' WHERE id = 2`)
	var stamp, node int64
	s.db.QueryRow(`SELECT changed_at_ms, node_id FROM sync_rows WHERE tbl = 'lanes' AND pk_json = '[2]'`).Scan(&stamp, &node)
	if stamp != ahead+1 || node != 1 {
		t.Fatalf("stamp %d node %d, want %d node 1", stamp, node, ahead+1)
	}
}

// Rows are named by their whole key, composite and text keys included.
func TestSyncNamesRowsByKey(t *testing.T) {
	s := openSynced(t)
	ctx := context.Background()
	src, err := s.CreateTask(ctx, Task{Title: "source"})
	if err != nil {
		t.Fatal(err)
	}
	mem, err := s.CreateMemory(ctx, Memory{Name: "m", Body: "b"})
	if err != nil {
		t.Fatal(err)
	}
	outbox(t, s)
	task, err := s.CreateTask(ctx, Task{Title: "t", Fields: map[string]string{"slack": "https://x"},
		ContextFrom: []int64{src.ID}, Memories: []int64{mem.ID}})
	if err != nil {
		t.Fatal(err)
	}
	got := outbox(t, s)
	for _, want := range []string{
		fmt.Sprintf("insert tasks [%d]", task.ID),
		fmt.Sprintf(`insert task_fields [%d,"slack"]`, task.ID),
		fmt.Sprintf("insert task_context [%d,%d]", task.ID, src.ID),
		fmt.Sprintf("insert task_memories [%d,%d]", task.ID, mem.ID),
	} {
		if !slices.Contains(got, want) {
			t.Errorf("no %q in %v", want, got)
		}
	}
	exec(t, s, `INSERT INTO field_defs (key, label, position) VALUES ('jira', 'Jira', 9)`,
		fmt.Sprintf(`INSERT INTO trello_cards (card_id, task_id) VALUES ('abc123', %d)`, task.ID))
	if got := outbox(t, s); !slices.Equal(got, []string{`insert field_defs ["jira"]`, `insert trello_cards ["abc123"]`}) {
		t.Fatalf("text keys: %v", got)
	}
}

// A row whose key changes is deleted under the old key.
func TestSyncRecordsAChangedKey(t *testing.T) {
	s := openSynced(t)
	exec(t, s, `UPDATE field_defs SET key = 'pr' WHERE key = 'pull_request'`)
	got := outbox(t, s)
	slices.Sort(got)
	if want := []string{`delete field_defs ["pull_request"]`, `update field_defs ["pr"]`}; !slices.Equal(got, want) {
		t.Fatalf("outbox = %v, want %v", got, want)
	}
	if _, deleted := syncRow(t, s, "field_defs", `["pull_request"]`); !deleted {
		t.Fatal("old key not a tombstone")
	}
}

// Rows removed or changed by ON DELETE CASCADE / SET NULL are recorded too.
func TestSyncRecordsCascades(t *testing.T) {
	s := openSynced(t)
	ctx := context.Background()
	task, err := s.CreateTask(ctx, Task{Title: "t", Fields: map[string]string{"slack": "https://x"}})
	if err != nil {
		t.Fatal(err)
	}
	sess, err := s.CreateSession(ctx, Session{TaskID: task.ID, Status: "idle", Mode: "plan"})
	if err != nil {
		t.Fatal(err)
	}
	msg, err := s.AddMessage(ctx, Message{SessionID: sess.ID, Role: "user", Kind: "text", Content: "hi"})
	if err != nil {
		t.Fatal(err)
	}
	mem, err := s.CreateMemory(ctx, Memory{Name: "m", Body: "b"})
	if err != nil {
		t.Fatal(err)
	}
	var rev int64
	if err := s.db.QueryRow(`SELECT id FROM memory_revisions WHERE memory_id = ?`, mem.ID).Scan(&rev); err != nil {
		t.Fatal(err)
	}
	exec(t, s, fmt.Sprintf(`UPDATE memory_revisions SET task_id = %d WHERE id = %d`, task.ID, rev))
	outbox(t, s)

	if err := s.DeleteTask(ctx, task.ID); err != nil {
		t.Fatal(err)
	}
	got := outbox(t, s)
	for _, want := range []string{
		fmt.Sprintf("delete tasks [%d]", task.ID),
		fmt.Sprintf(`delete task_fields [%d,"slack"]`, task.ID),
		fmt.Sprintf("delete sessions [%d]", sess.ID),
		fmt.Sprintf("delete messages [%d]", msg.ID),
		fmt.Sprintf("update memory_revisions [%d]", rev),
	} {
		if !slices.Contains(got, want) {
			t.Errorf("no %q in %v", want, got)
		}
	}
	for _, r := range [][2]string{
		{"tasks", fmt.Sprintf("[%d]", task.ID)},
		{"task_fields", fmt.Sprintf(`[%d,"slack"]`, task.ID)},
		{"sessions", fmt.Sprintf("[%d]", sess.ID)},
		{"messages", fmt.Sprintf("[%d]", msg.ID)},
	} {
		if _, deleted := syncRow(t, s, r[0], r[1]); !deleted {
			t.Errorf("%s %s is not a tombstone", r[0], r[1])
		}
	}
}

// Local tables leave no trace.
func TestSyncSkipsLocalTables(t *testing.T) {
	s := openSynced(t)
	ctx := context.Background()
	task, _ := s.CreateTask(ctx, Task{Title: "t"})
	sess, _ := s.CreateSession(ctx, Session{TaskID: task.ID, Status: "running", Mode: "plan"})
	outbox(t, s)
	exec(t, s, fmt.Sprintf(`INSERT INTO queued_messages (session_id, content, created_at) VALUES (%d, 'later', 0)`, sess.ID))
	if err := s.AddBlock(ctx, "tasks", 1000, 1999); err != nil {
		t.Fatal(err)
	}
	if got := outbox(t, s); len(got) != 0 {
		t.Fatalf("outbox = %v", got)
	}
}

// Triggers are made afresh on every open, once, and migrations' own changes
// are not recorded.
func TestSyncTriggersOnReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	countTriggers := func() int {
		var n int
		s.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'trigger' AND name LIKE 'sync\_%' ESCAPE '\'`).Scan(&n)
		return n
	}
	want := 4 * len(syncTables)
	if n := countTriggers(); n != want {
		t.Fatalf("%d triggers, want %d", n, want)
	}
	s.SetSyncEnabled(context.Background(), true)
	s.Close()
	if s, err = Open(path); err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if n := countTriggers(); n != want {
		t.Fatalf("%d triggers after reopen, want %d", n, want)
	}
	exec(t, s, `UPDATE lanes SET name = 'Later' WHERE id = 1`)
	if got := outbox(t, s); !slices.Equal(got, []string{"update lanes [1]"}) {
		t.Fatalf("outbox = %v", got)
	}

	// A pending migration runs without the triggers.
	exec(t, s, fmt.Sprintf(`PRAGMA user_version = %d`, len(migrations)-1), `DROP TABLE sync_state`,
		`DROP TABLE sync_outbox`, `DROP TABLE sync_rows`)
	migrations = append(migrations, `UPDATE lanes SET name = 'Migrated' WHERE id = 1;`)
	defer func() { migrations = migrations[:len(migrations)-1] }()
	s.Close()
	if s, err = Open(path); err != nil {
		t.Fatal(err)
	}
	s.SetSyncEnabled(context.Background(), true)
	var name string
	s.db.QueryRow(`SELECT name FROM lanes WHERE id = 1`).Scan(&name)
	if got := outbox(t, s); name != "Migrated" || len(got) != 0 {
		t.Fatalf("lane %q, outbox = %v", name, got)
	}
}

func TestSyncMigrationKeepsData(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	const n = 33 // migrations before change capture
	for _, m := range migrations[:n] {
		if _, err := db.Exec(m); err != nil {
			t.Fatal(err)
		}
	}
	for _, q := range []string{
		fmt.Sprintf(`PRAGMA user_version = %d`, n),
		`INSERT INTO tasks (id, title, lane_id, position, created_at, updated_at) VALUES (252, 'plan', 2, 0, 0, 0)`,
		`INSERT INTO task_fields (task_id, key, value) VALUES (252, 'slack', 'https://x')`,
		`INSERT INTO sessions (id, task_id, status, mode, created_at, updated_at) VALUES (291, 252, 'idle', 'plan', 0, 0)`,
		`INSERT INTO messages (id, session_id, role, kind, content, created_at) VALUES (9000, 291, 'user', 'text', 'hi', 0)`,
		`INSERT INTO id_blocks (tbl, next_id, end_id) VALUES ('tasks', 1000, 1999)`,
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
	if err != nil || task.Title != "plan" || task.Fields["slack"] != "https://x" {
		t.Fatalf("task = %+v, %v", task, err)
	}
	if msgs, _ := s.Messages(ctx, 291, 0); len(msgs) != 1 || msgs[0].Content != "hi" {
		t.Fatalf("messages = %+v", msgs)
	}
	// Capture starts off.
	next, err := s.CreateTask(ctx, Task{Title: "next"})
	if err != nil || next.ID != 1000 {
		t.Fatalf("new task = %+v, %v", next, err)
	}
	var enabled, applying, recorded int
	s.db.QueryRow(`SELECT enabled, applying, (SELECT COUNT(*) FROM sync_outbox) + (SELECT COUNT(*) FROM sync_rows) FROM sync_state`).
		Scan(&enabled, &applying, &recorded)
	if enabled != 0 || applying != 0 || recorded != 0 {
		t.Fatalf("enabled %d applying %d recorded %d", enabled, applying, recorded)
	}
}
