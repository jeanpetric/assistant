package store

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

// Upgrading gives every schedule and task this database's node; their data
// stays.
func TestScheduleTaskNodeMigration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	n := migrationWith(t, "ALTER TABLE schedules ADD COLUMN node_id")
	for _, m := range migrations[:n] {
		if _, err := db.Exec(m); err != nil {
			t.Fatal(err)
		}
	}
	for _, q := range []string{
		fmt.Sprintf(`PRAGMA user_version = %d`, n),
		`UPDATE sync_node SET node_id = 3`,
		`INSERT INTO tasks (id, title, description, lane_id, position, project_id, due_at, created_at, updated_at) VALUES (5, 'Ship it', 'soon', 2, 0, 1, 1000, 0, 0)`,
		`INSERT INTO schedules (id, task_id, cron, action, prompt, enabled, next_run_at) VALUES (9, 5, '0 9 * * *', 'remind', 'Ping', 1, 2000)`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(q, err)
		}
	}
	db.Close()

	s := openAt(t, path)
	ctx := context.Background()
	task, err := s.Task(ctx, 5)
	if err != nil || task.Title != "Ship it" || task.Description != "soon" || task.DueAt == nil || task.DueAt.UnixMilli() != 1000 {
		t.Fatalf("task = %+v, %v", task, err)
	}
	x, err := s.Schedule(ctx, 9)
	if err != nil || x.NodeID != 3 || x.Cron != "0 9 * * *" || x.Prompt != "Ping" || !x.Enabled || x.NextRunAt.UnixMilli() != 2000 {
		t.Fatalf("schedule = %+v, %v", x, err)
	}
	var taskNode int64
	if err := s.db.QueryRow(`SELECT node_id FROM tasks WHERE id = 5`).Scan(&taskNode); err != nil || taskNode != 3 {
		t.Fatalf("task node = %d, %v", taskNode, err)
	}
	// Both are this node's, so they are due here.
	if due, _ := s.DueSchedules(ctx, time.UnixMilli(5000)); len(due) != 1 {
		t.Fatalf("due schedules = %+v", due)
	}
	if due, _ := s.DueTasks(ctx, time.UnixMilli(5000)); len(due) != 1 {
		t.Fatalf("due tasks = %+v", due)
	}
}

// New schedules and tasks are this node's; another node's are seen but never due here.
func TestDueOnlyThisNodes(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	past := time.Now().Add(-time.Minute)
	mine, _ := s.CreateTask(ctx, Task{Title: "mine", LaneID: 2, DueAt: &past})
	theirs, _ := s.CreateTask(ctx, Task{Title: "theirs", LaneID: 2, DueAt: &past})
	ms, _ := s.SaveSchedule(ctx, Schedule{TaskID: mine.ID, Cron: "* * * * *", Action: ActionRemind, Enabled: true, NextRunAt: &past})
	ts, _ := s.SaveSchedule(ctx, Schedule{TaskID: theirs.ID, Cron: "* * * * *", Action: ActionRemind, Enabled: true, NextRunAt: &past})
	if ms.NodeID != 1 {
		t.Fatalf("new schedule's node = %d", ms.NodeID)
	}
	exec(t, s, fmt.Sprintf(`UPDATE tasks SET node_id = 2 WHERE id = %d`, theirs.ID),
		fmt.Sprintf(`UPDATE schedules SET node_id = 2 WHERE id = %d`, ts.ID))

	due, err := s.DueSchedules(ctx, time.Now())
	if err != nil || len(due) != 1 || due[0].ID != ms.ID {
		t.Fatalf("due schedules = %+v, %v", due, err)
	}
	tasks, err := s.DueTasks(ctx, time.Now())
	if err != nil || len(tasks) != 1 || tasks[0].ID != mine.ID {
		t.Fatalf("due tasks = %+v, %v", tasks, err)
	}
	if all, _ := s.Schedules(ctx, theirs.ID); len(all) != 1 || all[0].NodeID != 2 {
		t.Fatalf("their schedules = %+v", all)
	}
}

// Seeding moves this database's schedules and tasks to its new node, so they
// still run here; another node's stay that node's.
func TestStartSeedKeepsSchedulesAndTasksOwned(t *testing.T) {
	s := openAt(t, filepath.Join(t.TempDir(), "test.db"))
	ctx := context.Background()
	if err := s.SetSyncEnabled(ctx, true); err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-time.Minute)
	mine, _ := s.CreateTask(ctx, Task{Title: "mine", LaneID: 2, DueAt: &past})
	theirs, _ := s.CreateTask(ctx, Task{Title: "theirs", LaneID: 2, DueAt: &past})
	ms, _ := s.SaveSchedule(ctx, Schedule{TaskID: mine.ID, Cron: "* * * * *", Action: ActionRemind, Enabled: true, NextRunAt: &past})
	exec(t, s, fmt.Sprintf(`UPDATE tasks SET node_id = 2 WHERE id = %d`, theirs.ID))
	if _, err := s.StartSeed(ctx, 5); err != nil {
		t.Fatal(err)
	}
	if x, _ := s.Schedule(ctx, ms.ID); x.NodeID != 5 {
		t.Fatalf("schedule = %+v", x)
	}
	if due, _ := s.DueSchedules(ctx, time.Now()); len(due) != 1 {
		t.Fatalf("due schedules = %+v", due)
	}
	if due, _ := s.DueTasks(ctx, time.Now()); len(due) != 1 || due[0].ID != mine.ID {
		t.Fatalf("due tasks = %+v", due)
	}
}
