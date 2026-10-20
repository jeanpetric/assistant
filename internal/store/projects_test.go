package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
)

func titles(tasks []Task) []string {
	out := []string{}
	for _, x := range tasks {
		out = append(out, x.Title)
	}
	return out
}

// Upgrading puts every existing task, archived ones included, into an
// Assistant project and keeps their lanes, order and sessions.
func TestProjectsMigrationMovesTasksToAssistant(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	const n = 7 // migrations before projects
	for _, m := range migrations[:n] {
		if _, err := db.Exec(m); err != nil {
			t.Fatal(err)
		}
	}
	for _, q := range []string{
		fmt.Sprintf(`PRAGMA user_version = %d`, n),
		`INSERT INTO tasks (id, title, lane_id, position, created_at, updated_at) VALUES (1, 'b', 2, 1, 0, 0)`,
		`INSERT INTO tasks (id, title, lane_id, position, created_at, updated_at) VALUES (2, 'a', 2, 0, 0, 0)`,
		`INSERT INTO tasks (id, title, lane_id, position, archived_at, created_at, updated_at) VALUES (3, 'old', 5, 0, 9, 0, 0)`,
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
	projects, err := s.Projects(ctx)
	if err != nil || len(projects) != 1 || projects[0].Name != "Assistant" {
		t.Fatalf("projects = %+v, %v", projects, err)
	}
	pid := projects[0].ID
	tasks, _ := s.Tasks(ctx, pid)
	if got := fmt.Sprint(titles(tasks)); got != "[a b]" || tasks[1].Fields["slack"] != "https://slack/x" || tasks[0].LaneID != 2 {
		t.Fatalf("tasks = %+v", tasks)
	}
	if arch, _ := s.ArchivedTasks(ctx, pid); len(arch) != 1 || arch[0].ProjectID != pid {
		t.Fatalf("archived = %+v", arch)
	}
	if x, err := s.Session(ctx, 7); err != nil || x.TaskTitle != "b" {
		t.Fatalf("session = %+v, %v", x, err)
	}
	// New tasks without a project land in the first one.
	if task, err := s.CreateTask(ctx, Task{Title: "new"}); err != nil || task.ProjectID != pid {
		t.Fatalf("new task = %+v, %v", task, err)
	}
}

func TestProjectsScopeTheBoard(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	lanes, _ := s.Lanes(ctx)
	l0, l1 := lanes[0].ID, lanes[1].ID
	projects, _ := s.Projects(ctx)
	work := projects[0].ID
	private, err := s.CreateProject(ctx, Project{Name: "Private", Color: "#0a0"})
	if err != nil || private.Position != 1 {
		t.Fatalf("project %+v, %v", private, err)
	}
	home := private.ID

	a, _ := s.CreateTask(ctx, Task{Title: "a", LaneID: l0, ProjectID: work})
	b, _ := s.CreateTask(ctx, Task{Title: "b", LaneID: l0, ProjectID: work})
	x, _ := s.CreateTask(ctx, Task{Title: "x", LaneID: l0, ProjectID: home})
	// Positions count within the project's lane.
	if a.Position != 0 || b.Position != 1 || x.Position != 0 {
		t.Fatalf("positions a=%d b=%d x=%d", a.Position, b.Position, x.Position)
	}
	if got, _ := s.Tasks(ctx, home); fmt.Sprint(titles(got)) != "[x]" {
		t.Fatalf("private board = %v", titles(got))
	}
	if got, _ := s.Tasks(ctx, 0); len(got) != 3 {
		t.Fatalf("all tasks = %v", titles(got))
	}

	// A move's index is within the project; the other project's lane is untouched.
	if _, err := s.MoveTask(ctx, x.ID, l0, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := s.MoveTask(ctx, b.ID, l0, 0); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Tasks(ctx, work); fmt.Sprint(titles(got)) != "[b a]" {
		t.Fatalf("work board = %v", titles(got))
	}
	if got, _ := s.Task(ctx, x.ID); got.Position != 0 {
		t.Fatalf("x position = %d", got.Position)
	}

	// Moving to another project appends to the same lane there and closes the gap.
	if err := s.MoveTaskToProject(ctx, b.ID, home); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Task(ctx, b.ID); got.ProjectID != home || got.LaneID != l0 || got.Position != 1 {
		t.Fatalf("moved b = %+v", got)
	}
	if got, _ := s.Task(ctx, a.ID); got.Position != 0 {
		t.Fatalf("a position = %d", got.Position)
	}
	if err := s.MoveTaskToProject(ctx, b.ID, 999); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown project: %v", err)
	}
	if err := s.MoveTaskToProject(ctx, 999, home); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown task: %v", err)
	}

	// Archive and restore keep to the project's lane.
	s.MoveTask(ctx, a.ID, l1, 0)
	s.ArchiveTask(ctx, a.ID)
	s.MoveTaskToProject(ctx, a.ID, home)
	if arch, _ := s.ArchivedTasks(ctx, home); len(arch) != 1 || arch[0].ID != a.ID {
		t.Fatalf("archived in private = %v", titles(arch))
	}
	if arch, _ := s.ArchivedTasks(ctx, work); len(arch) != 0 {
		t.Fatalf("archived in work = %v", titles(arch))
	}
	s.CreateTask(ctx, Task{Title: "y", LaneID: l1, ProjectID: home})
	s.UnarchiveTask(ctx, a.ID)
	if got, _ := s.Task(ctx, a.ID); got.Position != 1 || got.ArchivedAt != nil {
		t.Fatalf("restored a = %+v", got)
	}
}

func TestProjectsCRUD(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	projects, _ := s.Projects(ctx)
	first := projects[0].ID
	p, _ := s.CreateProject(ctx, Project{Name: "Productive", Workdir: "/tmp"})
	if p.Workdir != "/tmp" {
		t.Fatalf("created %+v", p)
	}
	if err := s.UpdateProject(ctx, Project{ID: p.ID, Name: "Productive — AI", Color: "#00f", Workdir: "/srv"}); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateProject(ctx, Project{ID: 999, Name: "x"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("update unknown: %v", err)
	}
	if err := s.ReorderProjects(ctx, []int64{p.ID, first}); err != nil {
		t.Fatal(err)
	}
	projects, _ = s.Projects(ctx)
	if projects[0].Name != "Productive — AI" || projects[0].Color != "#00f" || projects[0].Workdir != "/srv" {
		t.Fatalf("projects = %+v", projects)
	}

	task, _ := s.CreateTask(ctx, Task{Title: "t", ProjectID: p.ID})
	s.ArchiveTask(ctx, task.ID)
	if err := s.DeleteProject(ctx, p.ID); !errors.Is(err, ErrProjectNotEmpty) {
		t.Fatalf("delete with archived task: %v", err)
	}
	s.DeleteTask(ctx, task.ID)
	if err := s.DeleteProject(ctx, p.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteProject(ctx, first); !errors.Is(err, ErrLastProject) {
		t.Fatalf("delete last: %v", err)
	}
	if err := s.DeleteProject(ctx, 999); !errors.Is(err, ErrNotFound) {
		t.Fatalf("delete unknown: %v", err)
	}
}

func TestProjectWorkdirMigrationKeepsProjects(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	old := len(migrations) - 1
	for _, m := range migrations[:old] {
		if _, err := db.Exec(m); err != nil {
			t.Fatal(err)
		}
	}
	for _, q := range []string{
		fmt.Sprintf(`PRAGMA user_version = %d`, old),
		`INSERT INTO projects (id, name, color, position) VALUES (2, 'Work', '#f00', 1)`,
		`INSERT INTO tasks (title, lane_id, position, project_id, workdir, created_at, updated_at) VALUES ('t', 1, 0, 2, '/x', 0, 0)`,
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
	p, err := s.Project(ctx, 2)
	if err != nil || p.Name != "Work" || p.Color != "#f00" || p.Workdir != "" {
		t.Fatalf("project = %+v, %v", p, err)
	}
	if tasks, _ := s.Tasks(ctx, 2); len(tasks) != 1 || tasks[0].Workdir != "/x" {
		t.Fatalf("tasks = %+v", tasks)
	}
}
