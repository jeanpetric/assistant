package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
)

func TestIDBlocksMigrationKeepsIDs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	const n = 32 // migrations before id blocks
	for _, m := range migrations[:n] {
		if _, err := db.Exec(m); err != nil {
			t.Fatal(err)
		}
	}
	for _, q := range []string{
		fmt.Sprintf(`PRAGMA user_version = %d`, n),
		`INSERT INTO tasks (id, title, lane_id, position, created_at, updated_at) VALUES (252, 'plan', 2, 0, 0, 0)`,
		`INSERT INTO tasks (id, title, lane_id, position, created_at, updated_at) VALUES (40, 'old', 5, 0, 0, 0)`,
		`INSERT INTO task_context (task_id, source_id, position) VALUES (252, 40, 0)`,
		`INSERT INTO sessions (id, task_id, status, mode, created_at, updated_at) VALUES (291, 252, 'idle', 'plan', 0, 0)`,
		`INSERT INTO messages (id, session_id, role, kind, content, created_at) VALUES (9000, 291, 'user', 'text', 'hi', 0)`,
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
	if node, err := s.NodeID(ctx); err != nil || node != 1 {
		t.Fatalf("node = %d, %v", node, err)
	}
	task, err := s.Task(ctx, 252)
	if err != nil || task.Title != "plan" || len(task.ContextFrom) != 1 || task.ContextFrom[0] != 40 {
		t.Fatalf("task = %+v, %v", task, err)
	}
	if x, err := s.Session(ctx, 291); err != nil || x.TaskID != 252 {
		t.Fatalf("session = %+v, %v", x, err)
	}
	if msgs, _ := s.Messages(ctx, 291, 0); len(msgs) != 1 || msgs[0].ID != 9000 {
		t.Fatalf("messages = %+v", msgs)
	}
	// Without blocks ids go on from the highest, as before.
	next, err := s.CreateTask(ctx, Task{Title: "next"})
	if err != nil || next.ID != 253 {
		t.Fatalf("new task = %+v, %v", next, err)
	}
	m, err := s.AddMessage(ctx, Message{SessionID: 291, Role: "user", Kind: "text", Content: "again"})
	if err != nil || m.ID != 9001 {
		t.Fatalf("new message = %+v, %v", m, err)
	}
}

func TestIDBlockExhausted(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	newTask := func() (int64, error) {
		x, err := s.CreateTask(ctx, Task{Title: "t"})
		return x.ID, err
	}
	first, err := newTask()
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AddBlock(ctx, "tasks", 1001, 1002); err != nil {
		t.Fatal(err)
	}
	if err := s.AddBlock(ctx, "tasks", 1002, 1005); err == nil {
		t.Fatal("overlapping block added")
	}
	for _, bad := range [][2]int64{{0, 5}, {10, 9}} {
		if err := s.AddBlock(ctx, "tasks", bad[0], bad[1]); err == nil {
			t.Fatalf("block %v added", bad)
		}
	}
	if err := s.AddBlock(ctx, "nope", 1, 2); err == nil {
		t.Fatal("block for unknown table added")
	}
	var got []int64
	for range 3 {
		id, err := newTask()
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, id)
	}
	// The block is used up, then ids go on locally from the highest.
	if fmt.Sprint(got) != "[1001 1002 1003]" {
		t.Fatalf("ids = %v (first %d)", got, first)
	}

	// Leased only: a used-up block stops inserts until another is added.
	s.SetLeasedIDs(true)
	if _, err := newTask(); !errors.Is(err, ErrNoIDs) {
		t.Fatalf("err = %v", err)
	}
	// Ids already taken are skipped; the lowest block goes first.
	if err := s.AddBlock(ctx, "tasks", 3000, 3001); err != nil {
		t.Fatal(err)
	}
	if err := s.AddBlock(ctx, "tasks", 1003, 1004); err != nil {
		t.Fatal(err)
	}
	got = nil
	for range 3 {
		id, err := newTask()
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, id)
	}
	if fmt.Sprint(got) != "[1004 3000 3001]" {
		t.Fatalf("ids = %v", got)
	}
	if _, err := newTask(); !errors.Is(err, ErrNoIDs) {
		t.Fatalf("err = %v", err)
	}
}

func TestDisjointBlocksNeverShareIDs(t *testing.T) {
	ctx := context.Background()
	a, b := open(t), open(t)
	for i, s := range []*Store{a, b} {
		s.SetLeasedIDs(true)
		for tbl := range idTables {
			from := int64(1000 + i*1000)
			if err := s.AddBlock(ctx, tbl, from, from+99); err != nil {
				t.Fatal(err)
			}
		}
	}
	seen := map[string]map[int64]bool{}
	note := func(tbl string, id int64) {
		if seen[tbl] == nil {
			seen[tbl] = map[int64]bool{}
		}
		if seen[tbl][id] {
			t.Fatalf("%s id %d made twice", tbl, id)
		}
		seen[tbl][id] = true
	}
	for range 5 {
		for _, s := range []*Store{a, b} {
			lane, err := s.CreateLane(ctx, "l", "")
			if err != nil {
				t.Fatal(err)
			}
			note("lanes", lane.ID)
			p, err := s.CreateProject(ctx, Project{Name: "p"})
			if err != nil {
				t.Fatal(err)
			}
			note("projects", p.ID)
			task, err := s.CreateTask(ctx, Task{Title: "t", ProjectID: p.ID, LaneID: lane.ID})
			if err != nil {
				t.Fatal(err)
			}
			note("tasks", task.ID)
			x, err := s.CreateSession(ctx, Session{TaskID: task.ID, Mode: "plan"})
			if err != nil {
				t.Fatal(err)
			}
			note("sessions", x.ID)
			m, err := s.AddMessage(ctx, Message{SessionID: x.ID, Role: "user", Kind: "text", Content: "hi"})
			if err != nil {
				t.Fatal(err)
			}
			note("messages", m.ID)
		}
	}
	// Every table took its ids from its block only.
	for _, s := range []*Store{a, b} {
		for tbl := range idTables {
			var outside int
			err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+tbl+` WHERE id >= 1000 AND id NOT IN
				(SELECT id FROM `+tbl+` WHERE id BETWEEN 1000 AND 1099 OR id BETWEEN 2000 AND 2099)`).Scan(&outside)
			if err != nil || outside != 0 {
				t.Fatalf("%s: %d ids outside the blocks, %v", tbl, outside, err)
			}
		}
	}
}

// Every table with an integer key the app adds rows to takes its ids from
// blocks (sync_outbox numbers its own); a new such table must join idTables.
func TestIDTablesCoverIntegerKeys(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	rows, err := s.db.QueryContext(ctx, `SELECT m.name FROM sqlite_master m, pragma_table_info(m.name) c
		WHERE m.type = 'table' AND c.pk = 1 AND upper(c.type) = 'INTEGER'
			AND NOT EXISTS (SELECT 1 FROM pragma_table_info(m.name) c2 WHERE c2.pk > 1)`)
	if err != nil {
		t.Fatal(err)
	}
	var tables []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		tables = append(tables, name)
	}
	rows.Close()
	local := map[string]bool{"settings": true, "sync_node": true, "sync_state": true, "sync_outbox": true, "local_settings": true}
	for _, name := range tables {
		if !idTables[name] && !local[name] {
			t.Errorf("table %s is not in idTables", name)
		}
	}
	if len(tables) != len(idTables)+len(local) {
		t.Errorf("tables %v, idTables %v", tables, idTables)
	}
}
