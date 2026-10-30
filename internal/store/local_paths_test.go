package store

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"slices"
	"testing"
)

// localPaths returns local_paths as "kind id workdir" rows.
func localPaths(t *testing.T, s *Store) []string {
	t.Helper()
	rows, err := s.db.Query(`SELECT kind, ref_id, workdir FROM local_paths ORDER BY kind, ref_id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var kind, dir string
		var id int64
		if err := rows.Scan(&kind, &id, &dir); err != nil {
			t.Fatal(err)
		}
		out = append(out, fmt.Sprintf("%s %d %s", kind, id, dir))
	}
	return out
}

// syncedWorkdir is the workdir column of a row of table.
func syncedWorkdir(t *testing.T, s *Store, table string, id int64) string {
	t.Helper()
	var dir string
	if err := s.db.QueryRow(`SELECT workdir FROM `+table+` WHERE id = ?`, id).Scan(&dir); err != nil {
		t.Fatal(err)
	}
	return dir
}

// Upgrading gives this computer the workdirs it had as its own, the synced
// columns kept as they were.
func TestLocalPathsMigrationCopiesWorkdirs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	n := migrationWith(t, "CREATE TABLE local_paths")
	for _, m := range migrations[:n] {
		if _, err := db.Exec(m); err != nil {
			t.Fatal(err)
		}
	}
	for _, q := range []string{
		fmt.Sprintf(`PRAGMA user_version = %d`, n),
		`UPDATE projects SET workdir = '/p1' WHERE id = 1`,
		`INSERT INTO projects (id, name, color, position, workdir) VALUES (2, 'Bare', '', 1, '')`,
		`INSERT INTO tasks (id, title, lane_id, position, project_id, workdir, created_at, updated_at) VALUES (5, 'With', 1, 0, 1, '/t5', 0, 0)`,
		`INSERT INTO tasks (id, title, lane_id, position, project_id, workdir, created_at, updated_at) VALUES (6, 'Without', 1, 1, 2, '', 0, 0)`,
		`INSERT INTO transition_rules (id, name, to_lane_id, prompt, mode, workdir, enabled) VALUES (3, 'r', 2, 'p', 'plan', '/r3', 1)`,
		`INSERT INTO transition_rules (id, name, to_lane_id, prompt, mode, workdir, enabled) VALUES (4, 'bare', 2, 'p', 'plan', '', 1)`,
		`UPDATE settings SET workdirs = '["/a","/b"]' WHERE id = 1`,
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
	if got, want := localPaths(t, s), []string{"project 1 /p1", "rule 3 /r3", "task 5 /t5"}; !slices.Equal(got, want) {
		t.Fatalf("local_paths = %v, want %v", got, want)
	}
	for _, c := range []struct {
		table string
		id    int64
		want  string
	}{{"projects", 1, "/p1"}, {"projects", 2, ""}, {"tasks", 5, "/t5"}, {"tasks", 6, ""}, {"transition_rules", 3, "/r3"}, {"transition_rules", 4, ""}} {
		if got := syncedWorkdir(t, s, c.table, c.id); got != c.want {
			t.Errorf("%s %d workdir = %q, want %q", c.table, c.id, got, c.want)
		}
	}
	if p, err := s.Project(ctx, 1); err != nil || p.Workdir != "/p1" || p.WorkdirDefault != "/p1" {
		t.Fatalf("project = %+v, %v", p, err)
	}
	if task, err := s.Task(ctx, 6); err != nil || task.Workdir != "" || task.Title != "Without" {
		t.Fatalf("task = %+v, %v", task, err)
	}
	x, err := s.Settings(ctx)
	if err != nil || !slices.Equal(x.Workdirs, []string{"/a", "/b"}) {
		t.Fatalf("settings = %+v, %v", x, err)
	}
	var synced string
	s.db.QueryRow(`SELECT workdirs FROM settings`).Scan(&synced)
	if synced != `["/a","/b"]` {
		t.Fatalf("settings.workdirs = %s", synced)
	}
}

// Creating sets the default and this computer's workdir; editing changes only
// this computer's, and clearing it brings the default back.
func TestLocalWorkdirs(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	p, err := s.CreateProject(ctx, Project{Name: "P", Workdir: "/p"})
	if err != nil || p.Workdir != "/p" || p.WorkdirDefault != "/p" {
		t.Fatalf("project = %+v, %v", p, err)
	}
	task, err := s.CreateTask(ctx, Task{Title: "t", ProjectID: p.ID, Workdir: "/t"})
	if err != nil || task.Workdir != "/t" || task.WorkdirDefault != "/t" {
		t.Fatalf("task = %+v, %v", task, err)
	}
	r, err := s.SaveRule(ctx, Rule{Name: "r", ToLaneID: 3, Prompt: "p", Workdir: "/r", Enabled: true})
	if err != nil || r.Workdir != "/r" || r.WorkdirDefault != "/r" {
		t.Fatalf("rule = %+v, %v", r, err)
	}
	bare, _ := s.CreateTask(ctx, Task{Title: "bare"})
	want := []string{fmt.Sprintf("project %d /p", p.ID), fmt.Sprintf("rule %d /r", r.ID), fmt.Sprintf("task %d /t", task.ID)}
	if got := localPaths(t, s); !slices.Equal(got, want) {
		t.Fatalf("local_paths = %v, want %v", got, want)
	}

	p.Workdir = "/here/p"
	if err := s.UpdateProject(ctx, p); err != nil {
		t.Fatal(err)
	}
	task.Workdir = "/here/t"
	if err := s.UpdateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	r.Workdir = "/here/r"
	if _, err := s.SaveRule(ctx, r); err != nil {
		t.Fatal(err)
	}
	bare.Workdir = "/here/bare"
	if err := s.UpdateTask(ctx, bare); err != nil {
		t.Fatal(err)
	}
	if p, _ := s.Project(ctx, p.ID); p.Workdir != "/here/p" || p.WorkdirDefault != "/p" {
		t.Fatalf("project = %+v", p)
	}
	if got, _ := s.Task(ctx, task.ID); got.Workdir != "/here/t" || got.WorkdirDefault != "/t" {
		t.Fatalf("task = %+v", got)
	}
	if got, _ := s.Task(ctx, bare.ID); got.Workdir != "/here/bare" || got.WorkdirDefault != "" {
		t.Fatalf("bare task = %+v", got)
	}
	if got, _ := s.Rule(ctx, r.ID); got.Workdir != "/here/r" || got.WorkdirDefault != "/r" {
		t.Fatalf("rule = %+v", got)
	}
	if rules, _ := s.MatchingRules(ctx, "task", p.ID, 2, 3); len(rules) != 1 || rules[0].Workdir != "/here/r" {
		t.Fatalf("matching rules = %+v", rules)
	}
	if ps, _ := s.Projects(ctx); ps[len(ps)-1].Workdir != "/here/p" {
		t.Fatalf("projects = %+v", ps)
	}
	if ts, _ := s.Tasks(ctx, p.ID); len(ts) != 1 || ts[0].Workdir != "/here/t" {
		t.Fatalf("tasks = %+v", ts)
	}
	for _, c := range []struct {
		table   string
		id      int64
		workdir string
	}{{"projects", p.ID, "/p"}, {"tasks", task.ID, "/t"}, {"transition_rules", r.ID, "/r"}, {"tasks", bare.ID, ""}} {
		if got := syncedWorkdir(t, s, c.table, c.id); got != c.workdir {
			t.Errorf("%s %d workdir = %q, want %q", c.table, c.id, got, c.workdir)
		}
	}
	// A remote change to the default doesn't override this computer's.
	exec(t, s, fmt.Sprintf(`UPDATE tasks SET workdir = '/elsewhere' WHERE id = %d`, task.ID))
	if got, _ := s.Task(ctx, task.ID); got.Workdir != "/here/t" || got.WorkdirDefault != "/elsewhere" {
		t.Fatalf("task = %+v", got)
	}

	// Clearing brings the default back.
	p.Workdir = ""
	if err := s.UpdateProject(ctx, p); err != nil {
		t.Fatal(err)
	}
	task.Workdir = ""
	if err := s.UpdateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	if p, _ := s.Project(ctx, p.ID); p.Workdir != "/p" {
		t.Fatalf("cleared project = %+v", p)
	}
	if got, _ := s.Task(ctx, task.ID); got.Workdir != "/elsewhere" {
		t.Fatalf("cleared task = %+v", got)
	}
	// Saving the default as read doesn't make it this computer's.
	got, _ := s.Task(ctx, task.ID)
	got.Title = "renamed"
	if err := s.UpdateTask(ctx, got); err != nil {
		t.Fatal(err)
	}
	want = []string{fmt.Sprintf("rule %d /here/r", r.ID), fmt.Sprintf("task %d /here/bare", bare.ID)}
	if got := localPaths(t, s); !slices.Equal(got, want) {
		t.Fatalf("local_paths = %v, want %v", got, want)
	}
}

// Deleting a project, task or rule drops its local path, however it goes.
func TestLocalPathsGoWithTheirRows(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	p, _ := s.CreateProject(ctx, Project{Name: "P", Workdir: "/p"})
	pid := p.ID
	r, _ := s.SaveRule(ctx, Rule{Name: "r", ProjectID: &pid, ToLaneID: 3, Prompt: "p", Workdir: "/r", Enabled: true})
	other, _ := s.SaveRule(ctx, Rule{Name: "other", ToLaneID: 3, Prompt: "p", Workdir: "/o", Enabled: true})
	task, _ := s.CreateTask(ctx, Task{Title: "t", ProjectID: p.ID, Workdir: "/t"})
	gone, _ := s.CreateTask(ctx, Task{Title: "gone", Workdir: "/g"})

	if err := s.DeleteTask(ctx, task.ID); err != nil {
		t.Fatal(err)
	}
	exec(t, s, fmt.Sprintf(`DELETE FROM tasks WHERE id = %d`, gone.ID))
	if err := s.DeleteProject(ctx, p.ID); err != nil { // its rule goes with it
		t.Fatal(err)
	}
	if _, err := s.Rule(ctx, r.ID); err != ErrNotFound {
		t.Fatalf("rule after its project: %v", err)
	}
	want := []string{fmt.Sprintf("rule %d /o", other.ID)}
	if got := localPaths(t, s); !slices.Equal(got, want) {
		t.Fatalf("local_paths = %v, want %v", got, want)
	}
	if err := s.DeleteRule(ctx, other.ID); err != nil {
		t.Fatal(err)
	}
	if got := localPaths(t, s); len(got) != 0 {
		t.Fatalf("local_paths = %v", got)
	}
}

// The workdirs offered are this computer's: saving them leaves the synced
// settings column alone.
func TestSettingsWorkdirsAreLocal(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	x, _ := s.Settings(ctx)
	x.Workdirs = []string{"/x", "/y"}
	x.ArchiveAfterMinutes = 7
	if err := s.SaveSettings(ctx, x); err != nil {
		t.Fatal(err)
	}
	got, err := s.Settings(ctx)
	if err != nil || !slices.Equal(got.Workdirs, x.Workdirs) || got.ArchiveAfterMinutes != 7 {
		t.Fatalf("settings = %+v, %v", got, err)
	}
	var synced, local string
	s.db.QueryRow(`SELECT workdirs FROM settings`).Scan(&synced)
	s.db.QueryRow(`SELECT workdirs FROM local_settings`).Scan(&local)
	if synced != "[]" || local != `["/x","/y"]` {
		t.Fatalf("settings.workdirs = %s, local_settings.workdirs = %s", synced, local)
	}
}
