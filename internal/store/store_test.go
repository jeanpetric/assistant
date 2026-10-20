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

func open(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestReopenKeepsDataAndSkipsMigrations(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	s.CreateTask(context.Background(), Task{Title: "persist me"})
	s.Close()

	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	tasks, _ := s.Tasks(context.Background(), 0)
	lanes, _ := s.Lanes(context.Background())
	if len(tasks) != 1 || len(lanes) != 5 {
		t.Fatalf("tasks=%d lanes=%d", len(tasks), len(lanes))
	}
}

func TestTasksAndFields(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	task, err := s.CreateTask(ctx, Task{Title: "A", Kind: "research", Fields: map[string]string{"slack": "https://slack/x"}})
	if err != nil {
		t.Fatal(err)
	}
	lanes, _ := s.Lanes(ctx)
	if task.LaneID != lanes[0].ID || task.Fields["slack"] != "https://slack/x" || task.Kind != "research" {
		t.Fatalf("created %+v", task)
	}

	task.Title = "A2"
	task.Fields = map[string]string{"slack": "", "productive": "https://app.productive.io/t/1"}
	if err := s.UpdateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	got, _ := s.Task(ctx, task.ID)
	if got.Title != "A2" || len(got.Fields) != 1 || got.Fields["productive"] == "" {
		t.Fatalf("updated %+v", got)
	}

	// Unknown field keys are rejected by the foreign key.
	got.Fields = map[string]string{"nope": "x"}
	if err := s.UpdateTask(ctx, got); err == nil {
		t.Fatal("expected FK error for unknown field")
	}

	if err := s.UpsertFieldDef(ctx, FieldDef{Key: "doc", Label: "Doc", Type: "url"}); err != nil {
		t.Fatal(err)
	}
	got.Fields = map[string]string{"doc": "https://docs"}
	if err := s.UpdateTask(ctx, got); err != nil {
		t.Fatal(err)
	}
	all, _ := s.Tasks(ctx, 0)
	if all[0].Fields["doc"] != "https://docs" {
		t.Fatalf("tasks fields %+v", all[0].Fields)
	}

	if err := s.DeleteTask(ctx, task.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Task(ctx, task.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected not found, got %v", err)
	}
}

func laneOrder(t *testing.T, s *Store, laneID int64) []string {
	t.Helper()
	tasks, _ := s.Tasks(context.Background(), 0)
	var out []string
	for _, x := range tasks {
		if x.LaneID == laneID {
			out = append(out, x.Title)
		}
	}
	return out
}

func TestMoveTask(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	lanes, _ := s.Lanes(ctx)
	l0, l1 := lanes[0].ID, lanes[1].ID
	a, _ := s.CreateTask(ctx, Task{Title: "a", LaneID: l0})
	s.CreateTask(ctx, Task{Title: "b", LaneID: l0})
	c, _ := s.CreateTask(ctx, Task{Title: "c", LaneID: l0})
	s.CreateTask(ctx, Task{Title: "x", LaneID: l1})

	if _, err := s.MoveTask(ctx, c.ID, l0, 0); err != nil {
		t.Fatal(err)
	}
	if got := laneOrder(t, s, l0); len(got) != 3 || got[0] != "c" || got[1] != "a" || got[2] != "b" {
		t.Fatalf("reorder: %v", got)
	}

	from, err := s.MoveTask(ctx, a.ID, l1, 1)
	if err != nil || from != l0 {
		t.Fatalf("from=%d err=%v", from, err)
	}
	if got := laneOrder(t, s, l1); len(got) != 2 || got[1] != "a" {
		t.Fatalf("lane1: %v", got)
	}
	if got := laneOrder(t, s, l0); len(got) != 2 || got[0] != "c" || got[1] != "b" {
		t.Fatalf("lane0: %v", got)
	}

	// Index out of range appends.
	s.MoveTask(ctx, c.ID, l1, 99)
	if got := laneOrder(t, s, l1); got[len(got)-1] != "c" {
		t.Fatalf("append: %v", got)
	}

	if _, err := s.MoveTask(ctx, c.ID, 999, 0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("bad lane: %v", err)
	}
	if _, err := s.MoveTask(ctx, 999, l0, 0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("bad task: %v", err)
	}
}

func TestLaneOrderByDue(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	lanes, _ := s.Lanes(ctx)
	l0 := lanes[0].ID
	at := func(d int) *time.Time { x := time.Date(2026, 10, d, 9, 0, 0, 0, time.Local); return &x }
	s.CreateTask(ctx, Task{Title: "a", LaneID: l0})
	s.CreateTask(ctx, Task{Title: "late", LaneID: l0, DueAt: at(25)})
	b, _ := s.CreateTask(ctx, Task{Title: "b", LaneID: l0})
	soon, _ := s.CreateTask(ctx, Task{Title: "soon", LaneID: l0, DueAt: at(21)})

	if got := fmt.Sprint(laneOrder(t, s, l0)); got != "[soon late a b]" {
		t.Fatalf("due first: %v", got)
	}
	// A drop index counts in the shown order: b to just after the dated tasks.
	if _, err := s.MoveTask(ctx, b.ID, l0, 2); err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprint(laneOrder(t, s, l0)); got != "[soon late b a]" {
		t.Fatalf("manual order of the rest: %v", got)
	}
	// Dragging a dated task doesn't take it out of due order.
	s.MoveTask(ctx, soon.ID, l0, 3)
	if got := fmt.Sprint(laneOrder(t, s, l0)); got != "[soon late b a]" {
		t.Fatalf("dated task moved: %v", got)
	}
	// Clearing the due time puts it among the rest, by its position.
	soon.DueAt = nil
	s.UpdateTask(ctx, soon)
	if got := fmt.Sprint(laneOrder(t, s, l0)); got != "[late b a soon]" {
		t.Fatalf("cleared due: %v", got)
	}
}

func TestLanes(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	l, err := s.CreateLane(ctx, "Blocked", "#f00")
	if err != nil || l.Position != 5 {
		t.Fatalf("lane %+v err %v", l, err)
	}
	s.CreateTask(ctx, Task{Title: "t", LaneID: l.ID})
	if err := s.DeleteLane(ctx, l.ID); !errors.Is(err, ErrLaneNotEmpty) {
		t.Fatalf("expected not empty, got %v", err)
	}
	lanes, _ := s.Lanes(ctx)
	ids := []int64{l.ID}
	for _, x := range lanes[:5] {
		ids = append(ids, x.ID)
	}
	if err := s.ReorderLanes(ctx, ids); err != nil {
		t.Fatal(err)
	}
	lanes, _ = s.Lanes(ctx)
	if lanes[0].Name != "Blocked" {
		t.Fatalf("reorder: %+v", lanes)
	}
	if err := s.UpdateLane(ctx, l.ID, "Waiting", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteLane(ctx, lanes[5].ID); err != nil {
		t.Fatal(err)
	}
}

func TestSessionsListing(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	t1, _ := s.CreateTask(ctx, Task{Title: "one"})
	t2, _ := s.CreateTask(ctx, Task{Title: "two"})
	a, _ := s.CreateSession(ctx, Session{TaskID: t1.ID, Mode: "plan"})
	s.CreateSession(ctx, Session{TaskID: t2.ID, Mode: "plan"})
	s.AddMessage(ctx, Message{SessionID: a.ID, Role: "assistant", Kind: "text", Content: "hello"})
	s.AddMessage(ctx, Message{SessionID: a.ID, Role: "tool", Kind: "tool_result", Content: "noise"})
	s.SetSessionStatus(ctx, a.ID, StatusIdle, "")

	all, _ := s.Sessions(ctx, 0, "")
	if len(all) != 2 {
		t.Fatalf("all = %d", len(all))
	}
	mine, _ := s.Sessions(ctx, t1.ID, "")
	if len(mine) != 1 || mine[0].TaskTitle != "one" || mine[0].ProjectID != t1.ProjectID || mine[0].ProjectID == 0 || mine[0].LastMessage != "hello" {
		t.Fatalf("mine = %+v", mine)
	}
	running, _ := s.Sessions(ctx, 0, StatusRunning)
	if len(running) != 1 || running[0].TaskID != t2.ID {
		t.Fatalf("running = %+v", running)
	}
	s.MarkInterrupted(ctx)
	if running, _ := s.Sessions(ctx, 0, StatusRunning); len(running) != 0 {
		t.Fatal("expected none running")
	}
	msgs, _ := s.Messages(ctx, a.ID, 0)
	if len(msgs) != 2 {
		t.Fatal("messages")
	}
	after, _ := s.Messages(ctx, a.ID, msgs[0].ID)
	if len(after) != 1 || after[0].Content != "noise" {
		t.Fatalf("after = %+v", after)
	}

	// Deleting the task cascades.
	s.DeleteTask(ctx, t1.ID)
	if _, err := s.Session(ctx, a.ID); !errors.Is(err, ErrNotFound) {
		t.Fatal("session should cascade")
	}
}

func TestDeletingLaneCascadesRules(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	l, _ := s.CreateLane(ctx, "Tmp", "")
	r, err := s.SaveRule(ctx, Rule{Name: "r", ToLaneID: l.ID, Prompt: "p", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	s.DeleteLane(ctx, l.ID)
	if _, err := s.Rule(ctx, r.ID); !errors.Is(err, ErrNotFound) {
		t.Fatal("rule should be gone")
	}
}

func TestDueTasks(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	at := func(h int) *time.Time { x := time.Date(2026, 10, 20, h, 0, 0, 0, time.Local); return &x }
	early, _ := s.CreateTask(ctx, Task{Title: "early", Kind: "reminder", DueAt: at(9)})
	s.CreateTask(ctx, Task{Title: "late", DueAt: at(15)})
	s.CreateTask(ctx, Task{Title: "no due"})
	if !early.DueAt.Equal(*at(9)) || early.RemindedAt != nil {
		t.Fatalf("created %+v", early)
	}

	due, err := s.DueTasks(ctx, *at(10))
	if err != nil || len(due) != 1 || due[0].ID != early.ID {
		t.Fatalf("due = %+v err %v", due, err)
	}
	if err := s.MarkReminded(ctx, early.ID, *at(10)); err != nil {
		t.Fatal(err)
	}
	if due, _ := s.DueTasks(ctx, *at(10)); len(due) != 0 {
		t.Fatalf("reminded task still due: %+v", due)
	}

	// Editing other attributes keeps the reminder state.
	got, _ := s.Task(ctx, early.ID)
	got.Title = "early!"
	s.UpdateTask(ctx, got)
	if got, _ = s.Task(ctx, early.ID); got.RemindedAt == nil {
		t.Fatal("unrelated edit re-armed the reminder")
	}
	// Moving the due time re-arms it; clearing it removes it.
	got.DueAt = at(11)
	s.UpdateTask(ctx, got)
	if got, _ = s.Task(ctx, early.ID); got.RemindedAt != nil || !got.DueAt.Equal(*at(11)) {
		t.Fatalf("rescheduled %+v", got)
	}
	if due, _ := s.DueTasks(ctx, *at(16)); len(due) != 2 {
		t.Fatalf("due later = %+v", due)
	}
	got.DueAt = nil
	s.UpdateTask(ctx, got)
	if due, _ := s.DueTasks(ctx, *at(16)); len(due) != 1 || due[0].Title != "late" {
		t.Fatalf("after clearing = %+v", due)
	}
}

func TestStandaloneSessionsAndWorkdirs(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	task, _ := s.CreateTask(ctx, Task{Title: "one", Workdir: "/task/dir"})
	s.CreateSession(ctx, Session{TaskID: task.ID, Mode: "plan", Workdir: "/a"})
	x, err := s.CreateSession(ctx, Session{Mode: "plan", Model: "opus", Workdir: "/b"})
	if err != nil {
		t.Fatal(err)
	}
	if x.TaskID != 0 || x.TaskTitle != "" || x.Model != "opus" {
		t.Fatalf("standalone = %+v", x)
	}
	all, _ := s.Sessions(ctx, 0, "")
	if len(all) != 2 {
		t.Fatalf("all = %d", len(all))
	}
}

// Rebuilding the sessions table must keep messages and the cascade from tasks.
func TestSessionsMigrationKeepsData(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range migrations[:2] {
		if _, err := db.Exec(m); err != nil {
			t.Fatal(err)
		}
	}
	for _, q := range []string{
		`PRAGMA user_version = 2`,
		`INSERT INTO tasks (id, title, lane_id, position, created_at, updated_at) VALUES (1, 'old', 1, 0, 0, 0)`,
		`INSERT INTO sessions (id, task_id, status, mode, created_at, updated_at) VALUES (7, 1, 'idle', 'plan', 0, 0)`,
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
	if err != nil || x.TaskTitle != "old" || x.Model != "" {
		t.Fatalf("session = %+v, %v", x, err)
	}
	if msgs, _ := s.Messages(ctx, 7, 0); len(msgs) != 1 {
		t.Fatalf("messages = %d", len(msgs))
	}
	s.DeleteTask(ctx, 1)
	if _, err := s.Session(ctx, 7); !errors.Is(err, ErrNotFound) {
		t.Fatal("session should cascade")
	}
}

func TestDoneLaneMigrationAndLaneDelete(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range migrations[:3] {
		if _, err := db.Exec(m); err != nil {
			t.Fatal(err)
		}
	}
	for _, q := range []string{
		`PRAGMA user_version = 3`,
		`INSERT INTO tasks (id, title, lane_id, position, created_at, updated_at) VALUES (1, 'old', 1, 0, 0, 0)`,
		`INSERT INTO sessions (id, task_id, status, mode, created_at, updated_at) VALUES (7, 1, 'idle', 'plan', 0, 0)`,
		`INSERT INTO transition_rules (id, name, to_lane_id, prompt) VALUES (2, 'r', 3, 'p')`,
		`INSERT INTO schedules (id, task_id, cron, action) VALUES (4, 1, '@daily', 'agent')`,
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
	if err != nil || x.TaskTitle != "old" || x.DoneLaneID != nil {
		t.Fatalf("session = %+v, %v", x, err)
	}
	if r, err := s.Rule(ctx, 2); err != nil || r.Name != "r" || r.DoneLaneID != nil {
		t.Fatalf("rule = %+v, %v", r, err)
	}
	if sc, err := s.Schedule(ctx, 4); err != nil || sc.Cron != "@daily" || sc.DoneLaneID != nil {
		t.Fatalf("schedule = %+v, %v", sc, err)
	}

	// Deleting a lane clears it as a done lane instead of deleting what points at it.
	l, _ := s.CreateLane(ctx, "QA", "")
	if err := s.SetSessionDoneLane(ctx, 7, &l.ID); err != nil {
		t.Fatal(err)
	}
	r, _ := s.SaveRule(ctx, Rule{Name: "to qa", ToLaneID: 1, Prompt: "p", DoneLaneID: &l.ID, Enabled: true})
	sc, _ := s.SaveSchedule(ctx, Schedule{TaskID: 1, Cron: "@daily", Action: "agent", DoneLaneID: &l.ID})
	if x, _ := s.Session(ctx, 7); x.DoneLaneID == nil || *x.DoneLaneID != l.ID {
		t.Fatalf("done lane not saved: %+v", x)
	}
	if err := s.DeleteLane(ctx, l.ID); err != nil {
		t.Fatal(err)
	}
	x, _ = s.Session(ctx, 7)
	r, rerr := s.Rule(ctx, r.ID)
	sc, serr := s.Schedule(ctx, sc.ID)
	if x.DoneLaneID != nil || rerr != nil || r.DoneLaneID != nil || serr != nil || sc.DoneLaneID != nil {
		t.Fatalf("after lane delete: session %+v, rule %+v %v, schedule %+v %v", x, r, rerr, sc, serr)
	}
}

// A database that went through an earlier build has a rate_limits table at
// version 6 instead of usage columns on sessions; sessions must still load.
func TestEarlierBuildDBReadsSessions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range append(migrations[:5:5], `CREATE TABLE rate_limits (
	name TEXT PRIMARY KEY,
	used_percent REAL NOT NULL,
	resets_at INTEGER NOT NULL,
	updated_at INTEGER NOT NULL
);`, migrations[6]) {
		if _, err := db.Exec(m); err != nil {
			t.Fatal(err)
		}
	}
	for _, q := range []string{
		`PRAGMA user_version = 7`,
		`INSERT INTO tasks (id, title, lane_id, position, created_at, updated_at) VALUES (1, 'old', 1, 0, 0, 0)`,
		`INSERT INTO sessions (id, task_id, status, mode, cost_usd, claude_session_id, created_at, updated_at) VALUES (7, 1, 'idle', 'plan', 1.5, 'c-7', 0, 0)`,
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
	if err != nil || x.TaskTitle != "old" || x.CostUSD != 1.5 || x.ClaudeSessionID != "c-7" || x.LastMessage != "hi" {
		t.Fatalf("session = %+v, %v", x, err)
	}
	if xs, err := s.Sessions(ctx, 1, ""); err != nil || len(xs) != 1 {
		t.Fatalf("sessions = %+v, %v", xs, err)
	}
}

func TestHandoffMigrationKeepsSessions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	const n = 8 // migrations before parent_id
	for _, m := range migrations[:n] {
		if _, err := db.Exec(m); err != nil {
			t.Fatal(err)
		}
	}
	for _, q := range []string{
		`PRAGMA user_version = 8`,
		`INSERT INTO tasks (id, title, lane_id, position, created_at, updated_at) VALUES (1, 'old', 1, 0, 0, 0)`,
		`INSERT INTO sessions (id, task_id, title, status, mode, claude_session_id, created_at, updated_at) VALUES (7, 1, 'work', 'idle', 'plan', 'c-7', 0, 0)`,
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
	if err != nil || x.Title != "work" || x.ClaudeSessionID != "c-7" || x.LastMessage != "hi" || x.ParentID != nil || x.ChildID != nil {
		t.Fatalf("session = %+v, %v", x, err)
	}

	child, err := s.CreateSession(ctx, Session{TaskID: 1, Title: "work", Mode: "plan", ParentID: &x.ID})
	if err != nil || child.ParentID == nil || *child.ParentID != 7 {
		t.Fatalf("child = %+v, %v", child, err)
	}
	if x, _ = s.Session(ctx, 7); x.ChildID == nil || *x.ChildID != child.ID {
		t.Fatalf("parent should point at its child: %+v", x)
	}
}

func TestContextFrom(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	research, _ := s.CreateTask(ctx, Task{Title: "Research"})
	spec, _ := s.CreateTask(ctx, Task{Title: "Spec"})
	build, err := s.CreateTask(ctx, Task{Title: "Build", ContextFrom: []int64{spec.ID, research.ID, spec.ID}})
	if err != nil || fmt.Sprint(build.ContextFrom) != fmt.Sprint([]int64{spec.ID, research.ID}) {
		t.Fatalf("build = %+v, %v", build, err)
	}
	if research.ContextFrom == nil || len(research.ContextFrom) != 0 {
		t.Fatalf("no links should be an empty list: %#v", research.ContextFrom)
	}
	tasks, _ := s.Tasks(ctx, 0)
	if got := tasks[2]; got.ID != build.ID || len(got.ContextFrom) != 2 || got.ContextFrom[0] != spec.ID {
		t.Fatalf("board task = %+v", got)
	}
	if users, err := s.ContextUsers(ctx, research.ID); err != nil || len(users) != 1 || users[0] != build.ID {
		t.Fatalf("users = %v, %v", users, err)
	}

	for _, bad := range [][]int64{{build.ID}, {999}} {
		if err := s.SetContextFrom(ctx, build.ID, bad); !errors.Is(err, ErrBadContext) {
			t.Fatalf("SetContextFrom(%v) = %v", bad, err)
		}
	}
	if got, _ := s.Task(ctx, build.ID); len(got.ContextFrom) != 2 {
		t.Fatalf("a rejected list must leave the old one: %+v", got.ContextFrom)
	}
	if err := s.SetContextFrom(ctx, build.ID, []int64{research.ID}); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteTask(ctx, research.ID); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Task(ctx, build.ID); len(got.ContextFrom) != 0 {
		t.Fatalf("deleting a source should unlink it: %+v", got.ContextFrom)
	}
}

func TestContextMigrationKeepsData(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	const n = 10 // migrations before task_context
	for _, m := range migrations[:n] {
		if _, err := db.Exec(m); err != nil {
			t.Fatal(err)
		}
	}
	for _, q := range []string{
		`PRAGMA user_version = 10`,
		`UPDATE projects SET workdir = '/w' WHERE id = 1`,
		`INSERT INTO tasks (id, title, lane_id, position, created_at, updated_at) VALUES (1, 'old', 1, 0, 0, 0), (2, 'next', 1, 1, 0, 0)`,
		`INSERT INTO task_fields (task_id, key, value) VALUES (1, 'slack', 'https://s')`,
		`INSERT INTO sessions (id, task_id, title, status, mode, created_at, updated_at) VALUES (7, 1, 'work', 'idle', 'plan', 0, 0)`,
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
	old, err := s.Task(ctx, 1)
	if err != nil || old.Title != "old" || old.Fields["slack"] != "https://s" || len(old.ContextFrom) != 0 {
		t.Fatalf("task = %+v, %v", old, err)
	}
	if x, err := s.Session(ctx, 7); err != nil || x.LastMessage != "hi" {
		t.Fatalf("session = %+v, %v", x, err)
	}
	if p, _ := s.Project(ctx, 1); p.Workdir != "/w" {
		t.Fatalf("project = %+v", p)
	}
	if err := s.SetContextFrom(ctx, 2, []int64{1}); err != nil {
		t.Fatal(err)
	}
	if next, _ := s.Task(ctx, 2); len(next.ContextFrom) != 1 || next.ContextFrom[0] != 1 {
		t.Fatalf("next = %+v", next)
	}
}

func TestSessionSummary(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	task, _ := s.CreateTask(ctx, Task{Title: "Research"})
	x, _ := s.CreateSession(ctx, Session{TaskID: task.ID, Title: "work", Mode: "plan"})
	if x.Summary != "" || x.SummaryAt != nil || x.SummaryStale {
		t.Fatalf("new session = %+v", x)
	}
	if id, err := s.LastMessageID(ctx, x.ID); err != nil || id != 0 {
		t.Fatalf("no messages: %d, %v", id, err)
	}
	m, _ := s.AddMessage(ctx, Message{SessionID: x.ID, Role: "user", Kind: "text", Content: "go"})
	if id, _ := s.LastMessageID(ctx, x.ID); id != m.ID {
		t.Fatalf("last message = %d, want %d", id, m.ID)
	}
	before, _ := s.Session(ctx, x.ID)
	if err := s.SetSessionSummary(ctx, x.ID, "Found it.", m.ID, 0.25); err != nil {
		t.Fatal(err)
	}
	got, _ := s.Session(ctx, x.ID)
	if got.Summary != "Found it." || got.SummaryUpto != m.ID || got.SummaryAt == nil || got.SummaryStale || got.CostUSD != 0.25 {
		t.Fatalf("summarised = %+v", got)
	}
	if !got.UpdatedAt.Equal(before.UpdatedAt) {
		t.Fatal("a summary must not count as activity in the session")
	}
	s.AddMessage(ctx, Message{SessionID: x.ID, Role: "user", Kind: "text", Content: "more"})
	if got, _ = s.Session(ctx, x.ID); !got.SummaryStale || got.Summary != "Found it." {
		t.Fatalf("a newer message should make the summary stale: %+v", got)
	}
	if err := s.SetSessionSummary(ctx, 999, "x", 0, 0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing session: %v", err)
	}
}

func TestSummaryMigrationKeepsSessions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	const n = 12 // migrations before the session summary
	for _, m := range migrations[:n] {
		if _, err := db.Exec(m); err != nil {
			t.Fatal(err)
		}
	}
	for _, q := range []string{
		`PRAGMA user_version = 12`,
		`INSERT INTO tasks (id, title, lane_id, position, created_at, updated_at) VALUES (1, 'old', 1, 0, 0, 0), (2, 'next', 1, 1, 0, 0)`,
		`INSERT INTO task_context (task_id, source_id, position) VALUES (2, 1, 0)`,
		`INSERT INTO sessions (id, task_id, title, status, mode, claude_session_id, cost_usd, created_at, updated_at) VALUES (7, 1, 'work', 'idle', 'plan', 'c-7', 1.5, 0, 5)`,
		`INSERT INTO sessions (id, task_id, title, status, mode, parent_id, created_at, updated_at) VALUES (8, 1, 'work', 'idle', 'plan', 7, 0, 6)`,
		`INSERT INTO messages (session_id, role, kind, content, created_at) VALUES (7, 'user', 'text', 'hi', 0), (7, 'system', 'result', 'done', 0)`,
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
	if err != nil || x.Title != "work" || x.ClaudeSessionID != "c-7" || x.CostUSD != 1.5 || x.LastMessage != "done" ||
		x.ChildID == nil || *x.ChildID != 8 || x.Summary != "" || x.SummaryUpto != 0 || x.SummaryAt != nil || x.SummaryStale {
		t.Fatalf("session = %+v, %v", x, err)
	}
	if msgs, _ := s.Messages(ctx, 7, 0); len(msgs) != 2 {
		t.Fatalf("messages = %+v", msgs)
	}
	if next, _ := s.Task(ctx, 2); len(next.ContextFrom) != 1 || next.ContextFrom[0] != 1 {
		t.Fatalf("next = %+v", next)
	}
	if err := s.SetSessionSummary(ctx, 7, "sum", 2, 0); err != nil {
		t.Fatal(err)
	}
	if x, _ = s.Session(ctx, 7); x.Summary != "sum" || x.SummaryStale {
		t.Fatalf("summarised = %+v", x)
	}
}

func TestNeedsInputAndSessionNotifications(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	task, _ := s.CreateTask(ctx, Task{Title: "t"})
	a, _ := s.CreateSession(ctx, Session{TaskID: task.ID, Mode: "default"})
	b, _ := s.CreateSession(ctx, Session{TaskID: task.ID, Mode: "default"})
	s.CreateSession(ctx, Session{Mode: "default"}) // standalone: no task to count it on

	if err := s.SetSessionNeedsInput(ctx, a.ID, "asked a question"); err != nil {
		t.Fatal(err)
	}
	s.SetSessionNeedsInput(ctx, b.ID, "permission denied: Bash")
	if got, _ := s.Session(ctx, a.ID); got.NeedsInput != "asked a question" {
		t.Fatalf("session = %+v", got)
	}
	if got, err := s.NeedsInputByTask(ctx); err != nil || len(got) != 1 || got[task.ID] != 2 {
		t.Fatalf("by task = %v, %v", got, err)
	}
	s.SetSessionNeedsInput(ctx, a.ID, "")
	if got, _ := s.NeedsInputByTask(ctx); got[task.ID] != 1 {
		t.Fatalf("after clearing: %v", got)
	}

	s.NotifySession(ctx, &task.ID, &a.ID, "needs you")
	s.Notify(ctx, nil, "plain")
	ns, err := s.Notifications(ctx, false)
	if err != nil || len(ns) != 2 || ns[0].SessionID != nil || ns[1].SessionID == nil || *ns[1].SessionID != a.ID || ns[1].TaskTitle != "t" {
		t.Fatalf("notifications = %+v, %v", ns, err)
	}
}

func TestNeedsInputMigrationKeepsData(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	const n = 15 // migrations before needs_input
	for _, m := range migrations[:n] {
		if _, err := db.Exec(m); err != nil {
			t.Fatal(err)
		}
	}
	for _, q := range []string{
		fmt.Sprintf(`PRAGMA user_version = %d`, n),
		`INSERT INTO tasks (id, title, kind, lane_id, position, created_at, updated_at) VALUES (1, 'old', 'task', 1, 0, 0, 0)`,
		`INSERT INTO sessions (id, task_id, title, status, mode, created_at, updated_at) VALUES (7, 1, 'kept', 'idle', 'default', 0, 0)`,
		`INSERT INTO messages (session_id, role, kind, content, created_at) VALUES (7, 'assistant', 'text', 'hello', 0)`,
		`INSERT INTO notifications (id, task_id, message, created_at) VALUES (3, 1, 'old note', 0)`,
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
	sess, err := s.Session(ctx, 7)
	if err != nil || sess.Title != "kept" || sess.Status != StatusIdle || sess.NeedsInput != "" || sess.LastMessage != "hello" {
		t.Fatalf("session = %+v, %v", sess, err)
	}
	ns, err := s.Notifications(ctx, false)
	if err != nil || len(ns) != 1 || ns[0].Message != "old note" || ns[0].TaskID == nil || ns[0].SessionID != nil {
		t.Fatalf("notifications = %+v, %v", ns, err)
	}
	// Deleting the session keeps the notification about it.
	task := sess.TaskID
	s.NotifySession(ctx, &task, &sess.ID, "new")
	if _, err := s.db.Exec(`DELETE FROM sessions WHERE id = 7`); err != nil {
		t.Fatal(err)
	}
	if ns, _ = s.Notifications(ctx, false); len(ns) != 2 || ns[0].SessionID != nil {
		t.Fatalf("after deleting the session: %+v", ns)
	}
}

func TestAgentStatusDerivation(t *testing.T) {
	lane := func(id int64) *int64 { return &id }
	cases := []struct {
		name string
		xs   []agentSession // latest first
		want AgentStatus
		ok   bool
	}{
		{"no sessions", nil, AgentStatus{}, false},
		{"running wins", []agentSession{
			{ID: 3, Status: StatusIdle, NeedsInput: "asked a question"},
			{ID: 2, Status: StatusRunning},
		}, AgentStatus{Status: AgentRunning, SessionID: 2}, true},
		{"needs you over a newer outcome", []agentSession{
			{ID: 3, Status: StatusError, Error: "boom"},
			{ID: 2, Status: StatusIdle, NeedsInput: "asked a question", Seen: true},
		}, AgentStatus{Status: AgentNeedsYou, SessionID: 2, Reason: "asked a question"}, true},
		{"turn finished", []agentSession{{ID: 1, Status: StatusIdle, LaneID: 5}},
			AgentStatus{Status: AgentFinished, SessionID: 1}, true},
		{"finished, task not in its done lane", []agentSession{{ID: 1, Status: StatusIdle, LaneID: 5, DoneLaneID: lane(6)}},
			AgentStatus{Status: AgentFinished, SessionID: 1}, true},
		{"done", []agentSession{{ID: 1, Status: StatusIdle, LaneID: 6, DoneLaneID: lane(6)}},
			AgentStatus{Status: AgentDone, SessionID: 1}, true},
		{"failed", []agentSession{{ID: 1, Status: StatusError, Error: "exit 1"}},
			AgentStatus{Status: AgentFailed, SessionID: 1, Reason: "exit 1"}, true},
		{"interrupted", []agentSession{{ID: 1, Status: StatusInterrupted}},
			AgentStatus{Status: AgentFailed, SessionID: 1, Reason: "interrupted by a restart"}, true},
		{"cancelled", []agentSession{{ID: 1, Status: StatusCancelled}},
			AgentStatus{Status: AgentCancelled, SessionID: 1}, true},
		{"latest seen hides older outcomes", []agentSession{
			{ID: 2, Status: StatusIdle, Seen: true},
			{ID: 1, Status: StatusError},
		}, AgentStatus{}, false},
		{"only the latest counts", []agentSession{
			{ID: 2, Status: StatusCancelled},
			{ID: 1, Status: StatusError},
		}, AgentStatus{Status: AgentCancelled, SessionID: 2}, true},
	}
	for _, c := range cases {
		got, ok := agentStatus(c.xs)
		if ok != c.ok || got != c.want {
			t.Errorf("%s: got %+v, %v; want %+v, %v", c.name, got, ok, c.want, c.ok)
		}
	}
}

func TestAgentStatuses(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	lanes, _ := s.Lanes(ctx)
	task, _ := s.CreateTask(ctx, Task{Title: "t", LaneID: lanes[0].ID})
	other, _ := s.CreateTask(ctx, Task{Title: "untouched"})
	s.CreateSession(ctx, Session{Mode: "default"}) // standalone: no task
	status := func() (AgentStatus, bool) {
		t.Helper()
		all, err := s.AgentStatuses(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := all[other.ID]; ok {
			t.Fatalf("a task without sessions has a status: %v", all)
		}
		st, ok := all[task.ID]
		return st, ok
	}

	done := lanes[1].ID
	a, _ := s.CreateSession(ctx, Session{TaskID: task.ID, Mode: "default", DoneLaneID: &done})
	if st, _ := status(); st.Status != AgentRunning || st.SessionID != a.ID {
		t.Fatalf("running: %+v", st)
	}
	s.SetSessionStatus(ctx, a.ID, StatusIdle, "")
	if st, _ := status(); st.Status != AgentFinished {
		t.Fatalf("finished: %+v", st)
	}
	s.MoveTask(ctx, task.ID, done, -1)
	if st, _ := status(); st.Status != AgentDone {
		t.Fatalf("done: %+v", st)
	}
	if err := s.DismissSession(ctx, a.ID); err != nil {
		t.Fatal(err)
	}
	if st, ok := status(); ok {
		t.Fatalf("dismissed: %+v", st)
	}
	if err := s.DismissSession(ctx, 999); !errors.Is(err, ErrNotFound) {
		t.Fatalf("dismiss unknown: %v", err)
	}

	// A new turn starts unseen; a failure shows until the task is moved.
	s.SetSessionStatus(ctx, a.ID, StatusRunning, "")
	s.SetSessionStatus(ctx, a.ID, StatusError, "boom")
	if st, _ := status(); st.Status != AgentFailed || st.Reason != "boom" {
		t.Fatalf("failed: %+v", st)
	}
	b, _ := s.CreateSession(ctx, Session{TaskID: task.ID, Mode: "default"})
	if err := s.SeeTaskSessions(ctx, task.ID); err != nil {
		t.Fatal(err)
	}
	if st, _ := status(); st.Status != AgentRunning || st.SessionID != b.ID {
		t.Fatalf("seeing keeps the running one: %+v", st)
	}
	if got, _ := s.Session(ctx, a.ID); !got.Seen {
		t.Fatalf("not seen: %+v", got)
	}
	if got, _ := s.Session(ctx, b.ID); got.Seen {
		t.Fatalf("running session marked seen: %+v", got)
	}

	// Waiting for the user wins over how the latest turn ended.
	s.SetSessionStatus(ctx, b.ID, StatusIdle, "")
	s.SetSessionNeedsInput(ctx, b.ID, "asked a question")
	if st, _ := status(); st.Status != AgentNeedsYou || st.Reason != "asked a question" {
		t.Fatalf("needs you: %+v", st)
	}

	// Restarting the app marks interrupted sessions unseen.
	s.SetSessionStatus(ctx, b.ID, StatusRunning, "")
	s.DismissSession(ctx, b.ID)
	s.MarkInterrupted(ctx)
	if st, _ := status(); st.Status != AgentFailed || st.SessionID != b.ID {
		t.Fatalf("interrupted: %+v", st)
	}
}

func TestSeenMigrationKeepsData(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	const n = 17 // migrations before seen
	for _, m := range migrations[:n] {
		if _, err := db.Exec(m); err != nil {
			t.Fatal(err)
		}
	}
	for _, q := range []string{
		fmt.Sprintf(`PRAGMA user_version = %d`, n),
		`INSERT INTO tasks (id, title, kind, lane_id, position, created_at, updated_at) VALUES (1, 'old', 'task', 1, 0, 0, 0)`,
		`INSERT INTO sessions (id, task_id, title, status, mode, created_at, updated_at) VALUES (7, 1, 'ended', 'error', 'default', 0, 0)`,
		`INSERT INTO sessions (id, task_id, title, status, mode, needs_input, created_at, updated_at) VALUES (8, 1, 'asks', 'idle', 'default', 'asked a question', 0, 1)`,
		`INSERT INTO sessions (id, task_id, title, status, mode, created_at, updated_at) VALUES (9, 1, 'busy', 'running', 'default', 0, 2)`,
		`INSERT INTO messages (session_id, role, kind, content, created_at) VALUES (7, 'assistant', 'text', 'hello', 0)`,
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
	want := map[int64]bool{7: true, 8: false, 9: false}
	for id, seen := range want {
		x, err := s.Session(ctx, id)
		if err != nil || x.Seen != seen || x.TaskID != 1 {
			t.Fatalf("session %d = %+v, %v", id, x, err)
		}
	}
	if x, _ := s.Session(ctx, 7); x.Title != "ended" || x.Status != StatusError || x.LastMessage != "hello" {
		t.Fatalf("session 7 = %+v", x)
	}
	if x, _ := s.Session(ctx, 8); x.NeedsInput != "asked a question" {
		t.Fatalf("session 8 = %+v", x)
	}
	if st, err := s.AgentStatuses(ctx); err != nil || st[1].Status != AgentRunning {
		t.Fatalf("statuses = %+v, %v", st, err)
	}
}

func TestSessionProjectMigrationKeepsSessions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	const n = 18 // migrations before the session's project
	for _, m := range migrations[:n] {
		if _, err := db.Exec(m); err != nil {
			t.Fatal(err)
		}
	}
	for _, q := range []string{
		fmt.Sprintf(`PRAGMA user_version = %d`, n),
		`UPDATE projects SET workdir = '/assistant' WHERE id = 1`,
		`INSERT INTO projects (id, name, workdir, position) VALUES (2, 'Web', '/web', 1)`,
		`INSERT INTO tasks (id, title, kind, project_id, lane_id, position, created_at, updated_at) VALUES (1, 'old', 'task', 2, 1, 0, 0, 0)`,
		`INSERT INTO sessions (id, task_id, title, status, mode, workdir, seen, created_at, updated_at) VALUES (7, 1, 'task', 'idle', 'default', '/assistant', 1, 0, 0)`,
		`INSERT INTO sessions (id, title, status, mode, workdir, created_at, updated_at) VALUES (8, 'in web', 'idle', 'default', '/web', 0, 1)`,
		`INSERT INTO sessions (id, title, status, mode, workdir, created_at, updated_at) VALUES (9, 'elsewhere', 'error', 'default', '/tmp', 0, 2)`,
		`INSERT INTO messages (session_id, role, kind, content, created_at) VALUES (8, 'assistant', 'text', 'hello', 0)`,
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
	// A task's session shows its task's project; standalone ones the project whose directory they ran in.
	want := map[int64]int64{7: 2, 8: 2, 9: 0}
	for id, project := range want {
		if x, err := s.Session(ctx, id); err != nil || x.ProjectID != project {
			t.Fatalf("session %d = %+v, %v", id, x, err)
		}
	}
	if x, _ := s.Session(ctx, 8); x.Title != "in web" || x.TaskID != 0 || x.LastMessage != "hello" || x.Workdir != "/web" {
		t.Fatalf("session 8 = %+v", x)
	}
	if x, _ := s.Session(ctx, 7); !x.Seen || x.TaskTitle != "old" {
		t.Fatalf("session 7 = %+v", x)
	}
}

func TestStandaloneSessionKeepsProject(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	p, _ := s.CreateProject(ctx, Project{Name: "Web"})
	task, _ := s.CreateTask(ctx, Task{Title: "t"})
	a, _ := s.CreateSession(ctx, Session{ProjectID: p.ID, Mode: "plan"})
	b, _ := s.CreateSession(ctx, Session{Mode: "plan"})
	// A task's session follows its task, whatever project it was given.
	c, _ := s.CreateSession(ctx, Session{TaskID: task.ID, ProjectID: p.ID, Mode: "plan"})
	if a.ProjectID != p.ID || b.ProjectID != 0 || c.ProjectID != task.ProjectID {
		t.Fatalf("projects = %d %d %d", a.ProjectID, b.ProjectID, c.ProjectID)
	}
	other, _ := s.CreateProject(ctx, Project{Name: "Other"})
	s.MoveTaskToProject(ctx, task.ID, other.ID)
	if x, _ := s.Session(ctx, c.ID); x.ProjectID != other.ID {
		t.Fatalf("moved task's session = %+v", x)
	}
	if err := s.DeleteProject(ctx, p.ID); err != nil {
		t.Fatal(err)
	}
	if x, err := s.Session(ctx, a.ID); err != nil || x.ProjectID != 0 {
		t.Fatalf("session of a deleted project = %+v, %v", x, err)
	}
}

func TestArchiveNotifications(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	s.Notify(ctx, nil, "read one")
	s.Notify(ctx, nil, "unread one")
	ns, _ := s.Notifications(ctx, false)
	s.MarkNotificationRead(ctx, ns[1].ID)
	readAt := mustNotification(t, s, ns[1].ID).ReadAt

	if err := s.ArchiveNotifications(ctx); err != nil {
		t.Fatal(err)
	}
	if ns, err := s.Notifications(ctx, false); err != nil || len(ns) != 0 {
		t.Fatalf("after archiving: %+v, %v", ns, err)
	}
	// The rows stay: archiving marks the unread read and keeps an earlier read time.
	var unread int
	s.db.QueryRow(`SELECT COUNT(*) FROM notifications WHERE read_at IS NULL OR archived_at IS NULL`).Scan(&unread)
	if unread != 0 {
		t.Fatalf("%d notifications left unread or unarchived", unread)
	}
	var kept int64
	s.db.QueryRow(`SELECT read_at FROM notifications WHERE id = ?`, ns[1].ID).Scan(&kept)
	if kept != readAt.UnixMilli() {
		t.Fatalf("read_at moved: %d -> %d", readAt.UnixMilli(), kept)
	}
	// New ones show again.
	s.Notify(ctx, nil, "fresh")
	if ns, _ := s.Notifications(ctx, true); len(ns) != 1 || ns[0].Message != "fresh" {
		t.Fatalf("after a new one: %+v", ns)
	}
}

func mustNotification(t *testing.T, s *Store, id int64) Notification {
	t.Helper()
	ns, _ := s.Notifications(context.Background(), false)
	for _, n := range ns {
		if n.ID == id {
			return n
		}
	}
	t.Fatalf("no notification %d", id)
	return Notification{}
}

func TestNotificationArchiveMigrationKeepsData(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	const n = 22 // migrations before archived notifications
	for _, m := range migrations[:n] {
		if _, err := db.Exec(m); err != nil {
			t.Fatal(err)
		}
	}
	for _, q := range []string{
		fmt.Sprintf(`PRAGMA user_version = %d`, n),
		`INSERT INTO tasks (id, title, lane_id, position, created_at, updated_at) VALUES (1, 't', 1, 0, 0, 0)`,
		`INSERT INTO sessions (id, task_id, title, status, mode, created_at, updated_at) VALUES (7, 1, 'work', 'idle', 'plan', 0, 0)`,
		`INSERT INTO notifications (id, task_id, session_id, message, created_at, read_at) VALUES (3, 1, 7, 'old read', 0, 5)`,
		`INSERT INTO notifications (id, task_id, message, created_at) VALUES (4, 1, 'old unread', 1)`,
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
	ns, err := s.Notifications(ctx, false)
	if err != nil || len(ns) != 2 || ns[0].Message != "old unread" || ns[0].ReadAt != nil ||
		ns[1].Message != "old read" || ns[1].ReadAt == nil || ns[1].SessionID == nil || *ns[1].SessionID != 7 || ns[1].TaskTitle != "t" {
		t.Fatalf("notifications = %+v, %v", ns, err)
	}
}

func TestRuleProjectAndSkillMigrationKeepsRules(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	n := len(migrations) - 1 // before rules had a project and a skill
	for _, m := range migrations[:n] {
		if _, err := db.Exec(m); err != nil {
			t.Fatal(err)
		}
	}
	for _, q := range []string{
		fmt.Sprintf(`PRAGMA user_version = %d`, n),
		`INSERT INTO transition_rules (id, name, from_lane_id, to_lane_id, prompt, mode, workdir, done_lane_id, enabled)
			VALUES (2, 'Code', 2, 3, 'p', 'plan', '/w', 4, 0)`,
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
	r, err := s.Rule(ctx, 2)
	if err != nil || r.Name != "Code" || *r.FromLaneID != 2 || r.ToLaneID != 3 || r.Prompt != "p" || r.Mode != "plan" ||
		r.Workdir != "/w" || *r.DoneLaneID != 4 || r.Enabled || r.ProjectID != nil || r.SkillID != nil {
		t.Fatalf("rule = %+v, %v", r, err)
	}
}

func TestRulesPerProjectAndSkill(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	p, _ := s.CreateProject(ctx, Project{Name: "Backyard"})
	sk, _ := s.SaveSkill(ctx, Skill{Name: "Review", Body: "b"})
	any, _ := s.SaveRule(ctx, Rule{Name: "any", ToLaneID: 3, Prompt: "p", Enabled: true})
	mine, err := s.SaveRule(ctx, Rule{Name: "mine", ProjectID: &p.ID, ToLaneID: 3, SkillID: &sk.ID, Enabled: true})
	if err != nil || *mine.ProjectID != p.ID || *mine.SkillID != sk.ID {
		t.Fatalf("rule = %+v, %v", mine, err)
	}
	if got, _ := s.MatchingRules(ctx, 1, 2, 3); len(got) != 1 || got[0].ID != any.ID {
		t.Fatalf("other project matches %+v", got)
	}
	if got, _ := s.MatchingRules(ctx, p.ID, 2, 3); len(got) != 1 || got[0].ID != mine.ID {
		t.Fatalf("own project's rule should override the any-project one: %+v", got)
	}
	// Only for the same transition: an any-project rule from a specific lane still fires.
	two := int64(2)
	fromTwo, _ := s.SaveRule(ctx, Rule{Name: "from two", FromLaneID: &two, ToLaneID: 3, Prompt: "p", Enabled: true})
	if got, _ := s.MatchingRules(ctx, p.ID, 2, 3); len(got) != 2 || got[0].ID != mine.ID || got[1].ID != fromTwo.ID {
		t.Fatalf("own project matches %+v", got)
	}
	// A disabled project rule overrides nothing.
	mine.Enabled = false
	if _, err := s.SaveRule(ctx, mine); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.MatchingRules(ctx, p.ID, 2, 3); len(got) != 2 || got[0].ID != any.ID {
		t.Fatalf("disabled project rule matches %+v", got)
	}
	if err := s.DeleteRule(ctx, fromTwo.ID); err != nil {
		t.Fatal(err)
	}

	// A rule goes with its skill, and with its project.
	if err := s.DeleteSkill(ctx, sk.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Rule(ctx, mine.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("rule outlived its skill: %v", err)
	}
	mine, _ = s.SaveRule(ctx, Rule{Name: "mine", ProjectID: &p.ID, ToLaneID: 3, Prompt: "p", Enabled: true})
	if err := s.DeleteProject(ctx, p.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Rule(ctx, mine.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("rule outlived its project: %v", err)
	}
	if _, err := s.Rule(ctx, any.ID); err != nil {
		t.Fatal(err)
	}
}
