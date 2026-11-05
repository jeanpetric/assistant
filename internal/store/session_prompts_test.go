package store

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"slices"
	"testing"
)

// Upgrading turns the one session prompt there was into the Default prompt
// for every project, and keeps the rest of the data; an empty one makes none.
func TestSessionPromptsMigration(t *testing.T) {
	for _, tc := range []struct{ old, want string }{{"Mine for {{.Project.Name}}", "Mine for {{.Project.Name}}"}, {"  ", ""}} {
		path := filepath.Join(t.TempDir(), "test.db")
		db, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(1)")
		if err != nil {
			t.Fatal(err)
		}
		n := migrationWith(t, "CREATE TABLE session_prompts")
		for _, m := range migrations[:n] {
			if _, err := db.Exec(m); err != nil {
				t.Fatal(err)
			}
		}
		for _, q := range []string{
			fmt.Sprintf(`PRAGMA user_version = %d`, n),
			`UPDATE settings SET archive_after_minutes = 42, session_prompt = '` + tc.old + `'`,
			`INSERT INTO projects (id, name, position) VALUES (7, 'Web', 1)`,
			`INSERT INTO tasks (id, title, project_id, lane_id, position, created_at, updated_at) VALUES (1, 'kept', 7, 1, 0, 0, 0)`,
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
		ctx := context.Background()
		ps, err := s.SessionPrompts(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if tc.want == "" && len(ps) != 0 {
			t.Fatalf("prompts from an empty one = %+v", ps)
		}
		if tc.want != "" && (len(ps) != 1 || ps[0].Name != "Default" || ps[0].Prompt != tc.want || !ps[0].AllProjects || !ps[0].AppliesTo(7)) {
			t.Fatalf("prompts = %+v", ps)
		}
		if x, _ := s.Settings(ctx); x.ArchiveAfterMinutes != 42 {
			t.Fatalf("settings = %+v", x)
		}
		if task, err := s.Task(ctx, 1); err != nil || task.Title != "kept" || task.ProjectID != 7 {
			t.Fatalf("task = %+v, %v", task, err)
		}
		// A new prompt takes an id after the carried-over one.
		x, err := s.SaveSessionPrompt(ctx, SessionPrompt{Name: "Web", Prompt: "w", ProjectIDs: []int64{7}})
		if err != nil || x.ID == 0 || (tc.want != "" && x.ID == ps[0].ID) || x.AllProjects || !x.AppliesTo(7) || x.AppliesTo(1) {
			t.Fatalf("saved = %+v, %v", x, err)
		}
		s.Close()
	}
}

func TestSessionPromptsCRUD(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	p, _ := s.CreateProject(ctx, Project{Name: "Web"})
	x, err := s.SaveSessionPrompt(ctx, SessionPrompt{Name: "a", Prompt: "x"})
	if err != nil || x.ProjectIDs == nil {
		t.Fatalf("saved = %+v, %v", x, err)
	}
	x.ProjectIDs, x.Prompt = []int64{p.ID}, "y"
	if x, err = s.SaveSessionPrompt(ctx, x); err != nil || x.Prompt != "y" || !slices.Equal(x.ProjectIDs, []int64{p.ID}) {
		t.Fatalf("updated = %+v, %v", x, err)
	}
	if err := s.DeleteProject(ctx, p.ID); err != nil {
		t.Fatal(err)
	}
	if x, err = s.SessionPrompt(ctx, x.ID); err != nil || len(x.ProjectIDs) != 0 {
		t.Fatalf("after project delete = %+v, %v", x, err)
	}
	if _, err := s.SaveSessionPrompt(ctx, SessionPrompt{ID: 999, Name: "n", Prompt: "p"}); err != ErrNotFound {
		t.Fatalf("update unknown = %v", err)
	}
	if err := s.DeleteSessionPrompt(ctx, x.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SessionPrompt(ctx, x.ID); err != ErrNotFound {
		t.Fatalf("deleted = %v", err)
	}
}
