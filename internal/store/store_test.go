package store

import (
	"context"
	"database/sql"
	"errors"
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
	tasks, _ := s.Tasks(context.Background())
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
	all, _ := s.Tasks(ctx)
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
	tasks, _ := s.Tasks(context.Background())
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
	if len(mine) != 1 || mine[0].TaskTitle != "one" || mine[0].LastMessage != "hello" {
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
	dirs, _ := s.RecentWorkdirs(ctx, 10)
	if len(dirs) != 3 {
		t.Fatalf("dirs = %v", dirs)
	}
	if dirs, _ := s.RecentWorkdirs(ctx, 1); len(dirs) != 1 {
		t.Fatalf("limit: %v", dirs)
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

func TestLimitsMigrationKeepsSessions(t *testing.T) {
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
	if err != nil || x.TaskTitle != "old" || x.CostUSD != 1.5 || x.ClaudeSessionID != "c-7" || x.Limits.FiveHour != nil || x.Limits.SevenDay != nil {
		t.Fatalf("session = %+v, %v", x, err)
	}
	if msgs, _ := s.Messages(ctx, 7, 0); len(msgs) != 1 {
		t.Fatalf("messages = %d", len(msgs))
	}

	resets := time.UnixMilli(time.Now().Add(time.Hour).UnixMilli())
	if err := s.SetSessionLimits(ctx, 7, Limits{FiveHour: &LimitWindow{UsedPercent: 40, ResetsAt: resets}}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetSessionLimits(ctx, 7, Limits{SevenDay: &LimitWindow{UsedPercent: 9, ResetsAt: resets}}); err != nil {
		t.Fatal(err)
	}
	x, _ = s.Session(ctx, 7)
	if l := x.Limits; l.FiveHour == nil || l.FiveHour.UsedPercent != 40 || !l.FiveHour.ResetsAt.Equal(resets) || l.SevenDay == nil || l.SevenDay.UsedPercent != 9 {
		t.Fatalf("limits = %+v", x.Limits)
	}
	if err := s.SetSessionLimits(ctx, 99, Limits{}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown session: %v", err)
	}
}
