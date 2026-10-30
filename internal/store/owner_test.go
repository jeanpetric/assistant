package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"
)

// foreign makes the session one node 2 runs, as if synced from there.
func foreign(t *testing.T, s *Store, id int64) {
	t.Helper()
	if _, err := s.db.Exec(`UPDATE sessions SET node_id = 2, node_name = 'office-mac' WHERE id = ?`, id); err != nil {
		t.Fatal(err)
	}
}

// Upgrading gives every session a node: the one of its latest change if sync
// recorded one, else this database's own. Their data stays.
func TestSessionNodeMigration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	n := migrationWith(t, "ALTER TABLE sessions ADD COLUMN node_id")
	for _, m := range migrations[:n] {
		if _, err := db.Exec(m); err != nil {
			t.Fatal(err)
		}
	}
	for _, q := range []string{
		fmt.Sprintf(`PRAGMA user_version = %d`, n),
		`UPDATE sync_node SET node_id = 3`,
		`INSERT INTO tasks (id, title, lane_id, position, project_id, created_at, updated_at) VALUES (5, 'Ship it', 1, 0, 1, 0, 0)`,
		`INSERT INTO sessions (id, task_id, title, status, mode, claude_session_id, created_at, updated_at) VALUES (7, 5, 'Mine', 'running', 'plan', 'c-7', 0, 0)`,
		`INSERT INTO sessions (id, task_id, title, status, mode, created_at, updated_at) VALUES (8, 5, 'Theirs', 'idle', 'plan', 0, 0)`,
		`INSERT INTO sync_rows (tbl, pk_json, changed_at_ms, node_id, deleted) VALUES ('sessions', '[8]', 1, 4, 0)`,
		`INSERT INTO messages (id, session_id, role, kind, content, created_at) VALUES (3, 7, 'user', 'text', 'go', 0)`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(q, err)
		}
	}
	db.Close()

	s := openAt(t, path)
	ctx := context.Background()
	mine, err := s.Session(ctx, 7)
	if err != nil || mine.NodeID != 3 || mine.NodeName != "" || !mine.Owned || mine.Title != "Mine" || mine.ClaudeSessionID != "c-7" {
		t.Fatalf("mine = %+v, %v", mine, err)
	}
	theirs, err := s.Session(ctx, 8)
	if err != nil || theirs.NodeID != 4 || theirs.Owned || theirs.Node() != "node 4" {
		t.Fatalf("theirs = %+v, %v", theirs, err)
	}
	if msgs, err := s.Messages(ctx, 7, 0); err != nil || len(msgs) != 1 || msgs[0].Content != "go" {
		t.Fatalf("messages = %+v, %v", msgs, err)
	}
}

// A new session runs on this node, named by its host.
func TestNewSessionIsThisNodes(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	task, _ := s.CreateTask(ctx, Task{Title: "t", LaneID: 2})
	sess, err := s.CreateSession(ctx, Session{TaskID: task.ID})
	if err != nil {
		t.Fatal(err)
	}
	if sess.NodeID != 1 || sess.NodeName != hostname() || !sess.Owned {
		t.Fatalf("session = %+v", sess)
	}
	foreign(t, s, sess.ID)
	if sess, _ = s.Session(ctx, sess.ID); sess.NodeID != 2 || sess.Owned || sess.Node() != "office-mac" {
		t.Fatalf("synced = %+v", sess)
	}
}

// Seeding a new node keeps this computer's sessions its own, each pushed once
// under the new node; another node's stay that node's.
func TestStartSeedKeepsSessionsOwned(t *testing.T) {
	s := openAt(t, filepath.Join(t.TempDir(), "test.db"))
	ctx := context.Background()
	if err := s.SetSyncEnabled(ctx, true); err != nil {
		t.Fatal(err)
	}
	task, _ := s.CreateTask(ctx, Task{Title: "t", LaneID: 2})
	mine, _ := s.CreateSession(ctx, Session{TaskID: task.ID})
	theirs, _ := s.CreateSession(ctx, Session{TaskID: task.ID})
	foreign(t, s, theirs.ID)
	if _, err := s.StartSeed(ctx, 5); err != nil {
		t.Fatal(err)
	}
	if x, _ := s.Session(ctx, mine.ID); x.NodeID != 5 || !x.Owned {
		t.Fatalf("mine = %+v", x)
	}
	if x, _ := s.Session(ctx, theirs.ID); x.NodeID != 2 || x.Owned {
		t.Fatalf("theirs = %+v", x)
	}
	changes, err := s.ExportChanges(ctx, 0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	seen := 0
	for _, c := range changes {
		if c.Table == "sessions" && string(c.Key) == fmt.Sprintf("[%d]", mine.ID) {
			seen++
			var row struct {
				NodeID int64 `json:"node_id"`
			}
			if err := json.Unmarshal(c.Row, &row); err != nil || row.NodeID != 5 {
				t.Fatalf("pushed %s, %v", c.Row, err)
			}
		}
	}
	if seen != 1 {
		t.Fatalf("session pushed %d times", seen)
	}
}

// A restart marks this node's running sessions interrupted; another node's
// run there and are left as they are.
func TestMarkInterruptedLeavesForeignSessions(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	task, _ := s.CreateTask(ctx, Task{Title: "t", LaneID: 2})
	mine, _ := s.CreateSession(ctx, Session{TaskID: task.ID})
	theirs, _ := s.CreateSession(ctx, Session{TaskID: task.ID})
	foreign(t, s, theirs.ID)
	if err := s.MarkInterrupted(ctx); err != nil {
		t.Fatal(err)
	}
	if x, _ := s.Session(ctx, mine.ID); x.Status != StatusInterrupted {
		t.Fatalf("mine = %s", x.Status)
	}
	if x, _ := s.Session(ctx, theirs.ID); x.Status != StatusRunning {
		t.Fatalf("theirs = %s", x.Status)
	}
}

// A message from another node's session names that node; this node's don't.
func TestTaskMessageSessionNode(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	task, _ := s.CreateTask(ctx, Task{Title: "t", LaneID: 2})
	mine, _ := s.CreateSession(ctx, Session{TaskID: task.ID})
	theirs, _ := s.CreateSession(ctx, Session{TaskID: task.ID})
	foreign(t, s, theirs.ID)
	a, _ := s.AddTaskMessage(ctx, TaskMessage{TaskID: task.ID, SessionID: &mine.ID, Body: "a"})
	b, _ := s.AddTaskMessage(ctx, TaskMessage{TaskID: task.ID, SessionID: &theirs.ID, Body: "b"})
	u, _ := s.AddTaskMessage(ctx, TaskMessage{TaskID: task.ID, Body: "u"})
	if a.SessionNode != "" || b.SessionNode != "office-mac" || u.SessionNode != "" {
		t.Fatalf("nodes %q %q %q", a.SessionNode, b.SessionNode, u.SessionNode)
	}
}

// Upgrading keeps every session and its messages; none reports to another.
// report_to is set when the session is made, synced with the rest of its row,
// and outlives the session it names.
func TestSessionReportToMigration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	n := migrationWith(t, "ALTER TABLE sessions ADD COLUMN report_to")
	for _, m := range migrations[:n] {
		if _, err := db.Exec(m); err != nil {
			t.Fatal(err)
		}
	}
	for _, q := range []string{
		fmt.Sprintf(`PRAGMA user_version = %d`, n),
		`INSERT INTO tasks (id, title, lane_id, position, project_id, created_at, updated_at) VALUES (5, 'Ship it', 1, 0, 1, 0, 0)`,
		`INSERT INTO sessions (id, task_id, title, status, mode, claude_session_id, node_name, created_at, updated_at) VALUES (7, 5, 'Mine', 'idle', 'plan', 'c-7', 'mac', 0, 0)`,
		`INSERT INTO messages (id, session_id, role, kind, content, created_at) VALUES (3, 7, 'user', 'text', 'go', 0)`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(q, err)
		}
	}
	db.Close()

	s := openAt(t, path)
	ctx := context.Background()
	old, err := s.Session(ctx, 7)
	if err != nil || old.ReportTo != nil || old.Title != "Mine" || old.ClaudeSessionID != "c-7" || old.NodeName != "mac" || old.TaskID != 5 {
		t.Fatalf("old = %+v, %v", old, err)
	}
	if msgs, err := s.Messages(ctx, 7, 0); err != nil || len(msgs) != 1 || msgs[0].Content != "go" {
		t.Fatalf("messages = %+v, %v", msgs, err)
	}

	if err := s.SetSyncEnabled(ctx, true); err != nil {
		t.Fatal(err)
	}
	sub, err := s.CreateSession(ctx, Session{TaskID: 5, Title: "sub", ReportTo: &old.ID})
	if err != nil || sub.ReportTo == nil || *sub.ReportTo != 7 {
		t.Fatalf("sub = %+v, %v", sub, err)
	}
	changes, err := s.ExportChanges(ctx, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	var row map[string]any
	for _, c := range changes {
		if c.Table == "sessions" && string(c.Key) == fmt.Sprintf("[%d]", sub.ID) {
			json.Unmarshal(c.Row, &row)
		}
	}
	if row["report_to"] != float64(7) {
		t.Fatalf("synced row = %v", row)
	}
	if err := s.DeleteTask(ctx, 5); err != nil {
		t.Fatal(err)
	}
	other, _ := s.CreateTask(ctx, Task{Title: "t", LaneID: 2})
	kept, err := s.CreateSession(ctx, Session{TaskID: other.ID, ReportTo: &sub.ID})
	if err != nil || kept.ReportTo == nil || *kept.ReportTo != sub.ID {
		t.Fatalf("kept = %+v, %v", kept, err)
	}
}
