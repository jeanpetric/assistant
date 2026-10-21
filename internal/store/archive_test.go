package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestArchiveAndRestore(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	now := time.Date(2026, 10, 20, 9, 0, 0, 0, time.Local)
	s.SetClock(func() time.Time { return now })
	lanes, _ := s.Lanes(ctx)
	l0, l1 := lanes[0].ID, lanes[1].ID
	a, _ := s.CreateTask(ctx, Task{Title: "a", LaneID: l0})
	b, _ := s.CreateTask(ctx, Task{Title: "b", LaneID: l0})
	s.CreateTask(ctx, Task{Title: "c", LaneID: l0})

	if err := s.ArchiveTask(ctx, b.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.ArchiveTask(ctx, b.ID); err != nil {
		t.Fatalf("archive twice: %v", err)
	}
	if err := s.ArchiveTask(ctx, 999); !errors.Is(err, ErrNotFound) {
		t.Fatalf("archive missing: %v", err)
	}
	if got := laneOrder(t, s, l0); len(got) != 2 || got[0] != "a" || got[1] != "c" {
		t.Fatalf("board after archive: %v", got)
	}
	arch, _ := s.ArchivedTasks(ctx, 0)
	if len(arch) != 1 || arch[0].ID != b.ID || arch[0].LaneID != l0 || arch[0].ArchivedAt == nil || !arch[0].ArchivedAt.Equal(now) {
		t.Fatalf("archived = %+v", arch)
	}

	// New and moved tasks go after the board tasks, ignoring the archived one.
	d, _ := s.CreateTask(ctx, Task{Title: "d", LaneID: l0})
	if d.Position != 2 {
		t.Fatalf("new task position %d", d.Position)
	}

	later := now.Add(time.Hour)
	s.SetClock(func() time.Time { return later })
	if err := s.UnarchiveTask(ctx, b.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.UnarchiveTask(ctx, b.ID); err != nil {
		t.Fatalf("restore twice: %v", err)
	}
	if got := laneOrder(t, s, l0); len(got) != 4 || got[3] != "b" {
		t.Fatalf("board after restore: %v", got)
	}
	got, _ := s.Task(ctx, b.ID)
	if got.ArchivedAt != nil || !got.LaneChangedAt.Equal(later) {
		t.Fatalf("restored = %+v", got)
	}

	// Moving an archived task restores it in the target lane.
	s.ArchiveTask(ctx, a.ID)
	if _, err := s.MoveTask(ctx, a.ID, l1, 0); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Task(ctx, a.ID); got.ArchivedAt != nil || got.LaneID != l1 {
		t.Fatalf("moved archived = %+v", got)
	}
}

func TestLaneChangedAt(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	now := time.Date(2026, 10, 20, 9, 0, 0, 0, time.Local)
	s.SetClock(func() time.Time { return now })
	lanes, _ := s.Lanes(ctx)
	task, _ := s.CreateTask(ctx, Task{Title: "a", LaneID: lanes[0].ID})
	if !task.LaneChangedAt.Equal(now) {
		t.Fatalf("created lane_changed_at = %v", task.LaneChangedAt)
	}
	s.CreateTask(ctx, Task{Title: "b", LaneID: lanes[0].ID})

	step := func() { now = now.Add(time.Minute) }
	step()
	s.MoveTask(ctx, task.ID, lanes[0].ID, 1) // reorder within the lane
	task.Title = "edited"
	s.UpdateTask(ctx, task)
	if got, _ := s.Task(ctx, task.ID); !got.LaneChangedAt.Equal(now.Add(-time.Minute)) {
		t.Fatalf("reorder/edit changed lane_changed_at: %v", got.LaneChangedAt)
	}
	step()
	s.MoveTask(ctx, task.ID, lanes[1].ID, 0)
	if got, _ := s.Task(ctx, task.ID); !got.LaneChangedAt.Equal(now) {
		t.Fatalf("lane move: %v", got.LaneChangedAt)
	}
}

func TestArchiveStale(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	now := time.Date(2026, 10, 20, 9, 0, 0, 0, time.Local)
	s.SetClock(func() time.Time { return now })
	lanes, _ := s.Lanes(ctx)
	done, other := lanes[4].ID, lanes[0].ID
	old, _ := s.CreateTask(ctx, Task{Title: "old", LaneID: done})
	s.CreateTask(ctx, Task{Title: "elsewhere", LaneID: other})
	now = now.Add(2 * time.Hour)
	s.CreateTask(ctx, Task{Title: "fresh", LaneID: done})

	ids, err := s.ArchiveStale(ctx, done, now.Add(-time.Hour))
	if err != nil || len(ids) != 1 || ids[0] != old.ID {
		t.Fatalf("archived %v, %v", ids, err)
	}
	if got := laneOrder(t, s, done); len(got) != 1 || got[0] != "fresh" {
		t.Fatalf("done lane: %v", got)
	}
	if got := laneOrder(t, s, other); len(got) != 1 {
		t.Fatalf("other lane touched: %v", got)
	}
	if ids, _ := s.ArchiveStale(ctx, done, now.Add(-time.Hour)); len(ids) != 0 {
		t.Fatalf("archived again: %v", ids)
	}
}

func TestSettings(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	lanes, _ := s.Lanes(ctx)
	x, err := s.Settings(ctx)
	if err != nil || x.ArchiveLaneID == nil || *x.ArchiveLaneID != lanes[4].ID || x.ArchiveAfterMinutes != 7*24*60 {
		t.Fatalf("defaults = %+v, %v", x, err)
	}
	l, _ := s.CreateLane(ctx, "Shipped", "")
	if err := s.SaveSettings(ctx, Settings{ArchiveLaneID: &l.ID, ArchiveAfterMinutes: 90}); err != nil {
		t.Fatal(err)
	}
	if x, _ := s.Settings(ctx); *x.ArchiveLaneID != l.ID || x.ArchiveAfterMinutes != 90 {
		t.Fatalf("saved = %+v", x)
	}
	// Deleting the lane turns auto-archive off instead of failing.
	if err := s.DeleteLane(ctx, l.ID); err != nil {
		t.Fatal(err)
	}
	if x, _ := s.Settings(ctx); x.ArchiveLaneID != nil || x.ArchiveAfterMinutes != 90 {
		t.Fatalf("after lane delete = %+v", x)
	}
}

func TestSettingsWorkdirs(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	if x, err := s.Settings(ctx); err != nil || x.Workdirs == nil || len(x.Workdirs) != 0 {
		t.Fatalf("defaults = %+v, %v", x, err)
	}
	if err := s.SaveSettings(ctx, Settings{Workdirs: []string{"/b", "/a"}}); err != nil {
		t.Fatal(err)
	}
	if x, _ := s.Settings(ctx); len(x.Workdirs) != 2 || x.Workdirs[0] != "/b" || x.Workdirs[1] != "/a" {
		t.Fatalf("saved = %+v", x)
	}
	if err := s.SaveSettings(ctx, Settings{}); err != nil {
		t.Fatal(err)
	}
	if x, _ := s.Settings(ctx); x.Workdirs == nil || len(x.Workdirs) != 0 {
		t.Fatalf("cleared = %+v", x)
	}
}

// Upgrading seeds the directory list with every workdir already in use,
// most recently used first, and keeps the other settings.
func TestWorkdirsMigrationSeedsUsedDirs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	const n = 6 // migrations before the workdirs list
	for _, m := range migrations[:n] {
		if _, err := db.Exec(m); err != nil {
			t.Fatal(err)
		}
	}
	for _, q := range []string{
		fmt.Sprintf(`PRAGMA user_version = %d`, n),
		`UPDATE settings SET archive_after_minutes = 42`,
		`INSERT INTO tasks (id, title, lane_id, position, workdir, created_at, updated_at) VALUES (1, 't', 1, 0, '/task', 0, 3000)`,
		`INSERT INTO sessions (id, task_id, status, mode, workdir, created_at, updated_at) VALUES (1, 1, 'idle', 'plan', '/old', 0, 1000)`,
		`INSERT INTO sessions (id, task_id, status, mode, workdir, created_at, updated_at) VALUES (2, 1, 'idle', 'plan', '/new', 0, 5000)`,
		`INSERT INTO sessions (id, task_id, status, mode, workdir, created_at, updated_at) VALUES (3, 1, 'idle', 'plan', '/old', 0, 2000)`,
		`INSERT INTO sessions (id, task_id, status, mode, workdir, created_at, updated_at) VALUES (4, 1, 'idle', 'plan', '', 0, 9000)`,
		`INSERT INTO transition_rules (name, to_lane_id, prompt, workdir) VALUES ('r', 1, 'p', '/rule')`,
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
	x, err := s.Settings(context.Background())
	if err != nil || x.ArchiveAfterMinutes != 42 || strings.Join(x.Workdirs, ",") != "/new,/task,/old,/rule" {
		t.Fatalf("settings = %+v, %v", x, err)
	}
}

func TestDeleteLaneWithArchivedTasks(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	lanes, _ := s.Lanes(ctx)
	l, _ := s.CreateLane(ctx, "Old", "")
	task, _ := s.CreateTask(ctx, Task{Title: "a", LaneID: l.ID})
	if err := s.DeleteLane(ctx, l.ID); !errors.Is(err, ErrLaneNotEmpty) {
		t.Fatalf("delete with board task: %v", err)
	}
	s.ArchiveTask(ctx, task.ID)
	if err := s.DeleteLane(ctx, l.ID); err != nil {
		t.Fatal(err)
	}
	got, err := s.Task(ctx, task.ID)
	if err != nil || got.LaneID != lanes[0].ID || got.ArchivedAt == nil {
		t.Fatalf("archived task = %+v, %v", got, err)
	}
}

// Upgrading a populated DB keeps tasks, seeds lane_changed_at from updated_at
// and points auto-archive at the Done lane.
func TestArchiveMigrationKeepsData(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range migrations[:4] {
		if _, err := db.Exec(m); err != nil {
			t.Fatal(err)
		}
	}
	for _, q := range []string{
		`PRAGMA user_version = 4`,
		`INSERT INTO tasks (id, title, lane_id, position, created_at, updated_at) VALUES (1, 'old', 5, 0, 1000, 5000)`,
		`INSERT INTO task_fields (task_id, key, value) VALUES (1, 'slack', 'https://slack/x')`,
		`INSERT INTO sessions (id, task_id, status, mode, created_at, updated_at) VALUES (7, 1, 'idle', 'plan', 0, 0)`,
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
	tasks, _ := s.Tasks(ctx, 0)
	if len(tasks) != 1 || tasks[0].Title != "old" || tasks[0].ArchivedAt != nil || tasks[0].LaneChangedAt.UnixMilli() != 5000 ||
		tasks[0].Fields["slack"] != "https://slack/x" {
		t.Fatalf("tasks = %+v", tasks)
	}
	if x, err := s.Session(ctx, 7); err != nil || x.TaskTitle != "old" {
		t.Fatalf("session = %+v, %v", x, err)
	}
	if x, _ := s.Settings(ctx); x.ArchiveLaneID == nil || *x.ArchiveLaneID != 5 {
		t.Fatalf("settings = %+v", x)
	}
}

// Upgrading adds the default session prompt and keeps the other settings.
func TestSessionPromptMigrationKeepsSettings(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	const n = 25 // migrations before the session prompt
	for _, m := range migrations[:n] {
		if _, err := db.Exec(m); err != nil {
			t.Fatal(err)
		}
	}
	for _, q := range []string{
		fmt.Sprintf(`PRAGMA user_version = %d`, n),
		`UPDATE settings SET archive_after_minutes = 42, workdirs = '["/a"]'`,
		`INSERT INTO tasks (id, title, lane_id, position, created_at, updated_at) VALUES (1, 'kept', 1, 0, 0, 0)`,
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
	x, err := s.Settings(ctx)
	if err != nil || x.ArchiveAfterMinutes != 42 || strings.Join(x.Workdirs, ",") != "/a" || x.SessionPrompt != DefaultSessionPrompt {
		t.Fatalf("settings = %+v, %v", x, err)
	}
	if task, err := s.Task(ctx, 1); err != nil || task.Title != "kept" {
		t.Fatalf("task = %+v, %v", task, err)
	}
}
