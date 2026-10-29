package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
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
	const old = 8 // migrations before the projects' workdir
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

// A project hides lanes of its own; the list is replaced on update, every
// lane must exist and one must stay shown, and deleting a lane or the
// project drops its rows.
func TestProjectHiddenLanes(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	lanes, _ := s.Lanes(ctx)
	a, _ := s.CreateProject(ctx, Project{Name: "A"})
	b, _ := s.CreateProject(ctx, Project{Name: "B"})
	if a.HiddenLanes == nil || len(a.HiddenLanes) != 0 {
		t.Fatalf("new project hides %v", a.HiddenLanes)
	}
	spec, _ := s.CreateLane(ctx, "Spec review", "")
	a.HiddenLanes = []int64{spec.ID, lanes[0].ID, spec.ID}
	if err := s.UpdateProject(ctx, a); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Project(ctx, a.ID); !slices.Equal(got.HiddenLanes, []int64{lanes[0].ID, spec.ID}) {
		t.Fatalf("A hides %v", got.HiddenLanes)
	}
	if got, _ := s.Project(ctx, b.ID); len(got.HiddenLanes) != 0 {
		t.Fatalf("B hides %v", got.HiddenLanes)
	}
	a.HiddenLanes = []int64{spec.ID}
	if err := s.UpdateProject(ctx, a); err != nil {
		t.Fatal(err)
	}
	ps, _ := s.Projects(ctx)
	for _, p := range ps {
		want := []int64{}
		if p.ID == a.ID {
			want = []int64{spec.ID}
		}
		if !slices.Equal(p.HiddenLanes, want) {
			t.Fatalf("%s hides %v", p.Name, p.HiddenLanes)
		}
	}

	a.HiddenLanes = []int64{999}
	if err := s.UpdateProject(ctx, a); !errors.Is(err, ErrUnknownLane) {
		t.Fatalf("unknown lane: %v", err)
	}
	all := []int64{spec.ID}
	for _, l := range lanes {
		all = append(all, l.ID)
	}
	a.HiddenLanes = all
	if err := s.UpdateProject(ctx, a); !errors.Is(err, ErrNoLaneShown) {
		t.Fatalf("every lane hidden: %v", err)
	}
	if got, _ := s.Project(ctx, a.ID); got.Name != "A" || !slices.Equal(got.HiddenLanes, []int64{spec.ID}) {
		t.Fatalf("a refused update changed %+v", got)
	}

	b.HiddenLanes = []int64{spec.ID}
	s.UpdateProject(ctx, b)
	if err := s.DeleteProject(ctx, b.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteLane(ctx, spec.ID); err != nil {
		t.Fatal(err)
	}
	var n int
	s.db.QueryRow(`SELECT COUNT(*) FROM project_hidden_lanes`).Scan(&n)
	if n != 0 {
		t.Fatalf("%d hidden lane rows left", n)
	}
}

// Upgrading leaves every project showing every lane, its data kept.
func TestHiddenLanesMigrationKeepsProjects(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	n := migrationWith(t, "CREATE TABLE project_hidden_lanes")
	for _, m := range migrations[:n] {
		if _, err := db.Exec(m); err != nil {
			t.Fatal(err)
		}
	}
	for _, q := range []string{
		fmt.Sprintf(`PRAGMA user_version = %d`, n),
		`INSERT INTO projects (id, name, color, position, workdir) VALUES (2, 'Work', '#f00', 1, '/w')`,
		`INSERT INTO lanes (id, name, color, position) VALUES (9, 'Spec review', '', 9)`,
		`INSERT INTO tasks (id, title, lane_id, position, project_id, created_at, updated_at) VALUES (5, 't', 9, 0, 2, 0, 0)`,
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
	ps, err := s.Projects(ctx)
	if err != nil || len(ps) != 2 {
		t.Fatalf("projects = %+v, %v", ps, err)
	}
	for _, p := range ps {
		if len(p.HiddenLanes) != 0 {
			t.Fatalf("%s hides %v", p.Name, p.HiddenLanes)
		}
	}
	if p := ps[1]; p.ID != 2 || p.Name != "Work" || p.Color != "#f00" || p.Workdir != "/w" {
		t.Fatalf("project = %+v", p)
	}
	if task, err := s.Task(ctx, 5); err != nil || task.LaneID != 9 || task.ProjectID != 2 {
		t.Fatalf("task = %+v, %v", task, err)
	}
	p := ps[1]
	p.HiddenLanes = []int64{9}
	if err := s.UpdateProject(ctx, p); err != nil {
		t.Fatal(err)
	}
}

// Hiding and showing a lane are synced changes; unchanged rows stay untouched.
func TestHiddenLanesSync(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	s.SetSyncEnabled(ctx, true)
	p, _ := s.Project(ctx, 1)
	p.HiddenLanes = []int64{2}
	s.UpdateProject(ctx, p)
	p.HiddenLanes = []int64{2, 3}
	s.UpdateProject(ctx, p)
	p.HiddenLanes = []int64{3}
	s.UpdateProject(ctx, p)
	var got []string
	for _, x := range outbox(t, s) {
		if strings.Contains(x, "project_hidden_lanes") {
			got = append(got, x)
		}
	}
	want := []string{"insert project_hidden_lanes [1,2]", "insert project_hidden_lanes [1,3]", "delete project_hidden_lanes [1,2]"}
	if !slices.Equal(got, want) {
		t.Fatalf("outbox = %v", got)
	}
}
