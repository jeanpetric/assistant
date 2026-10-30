package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"testing"
)

// Upgrading keeps every task's, rule's and automation's kind and lists the
// kinds there were, then any other kind they have.
func TestTaskKindsMigrationKeepsData(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	n := migrationWith(t, "CREATE TABLE task_kinds")
	for _, m := range migrations[:n] {
		if _, err := db.Exec(m); err != nil {
			t.Fatal(err)
		}
	}
	for _, q := range []string{
		fmt.Sprintf(`PRAGMA user_version = %d`, n),
		`INSERT INTO tasks (id, title, kind, lane_id, position, project_id, created_at, updated_at) VALUES (5, 'Fix', 'coding', 1, 0, 1, 0, 0)`,
		`INSERT INTO tasks (id, title, kind, lane_id, position, project_id, created_at, updated_at) VALUES (6, 'Read', 'research', 1, 1, 1, 0, 0)`,
		`INSERT INTO tasks (id, title, kind, lane_id, position, project_id, created_at, updated_at) VALUES (7, 'Odd', 'spike', 1, 2, 1, 0, 0)`,
		`INSERT INTO transition_rules (id, name, to_lane_id, prompt, kind) VALUES (3, 'r', 2, 'p', 'coding')`,
		`INSERT INTO transition_rules (id, name, to_lane_id, prompt, kind) VALUES (4, 'b', 2, 'p', 'bug')`,
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
	for id, want := range map[int64]string{5: "coding", 6: "research", 7: "spike"} {
		if task, err := s.Task(ctx, id); err != nil || task.Kind != want {
			t.Fatalf("task %d = %+v, %v", id, task, err)
		}
	}
	rules, err := s.Rules(ctx)
	if err != nil || len(rules) != 2 || rules[0].Kind != "coding" || rules[1].Kind != "bug" {
		t.Fatalf("rules = %+v, %v", rules, err)
	}
	// The seeded research automation keeps its kind too.
	kinds, err := s.Kinds(ctx)
	want := []string{"task", "coding", "research", "discussion", "reminder", "bug", "spike"}
	if err != nil || !slices.Equal(kinds, want) {
		t.Fatalf("kinds = %v, %v; want %v", kinds, err, want)
	}
}

func TestKinds(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	if err := s.AddKind(ctx, "bug"); err != nil {
		t.Fatal(err)
	}
	if err := s.AddKind(ctx, "bug"); !errors.Is(err, ErrKindExists) || err.Error() != "kind bug exists" {
		t.Fatalf("duplicate: %v", err)
	}
	if ok, err := s.HasKind(ctx, "bug"); err != nil || !ok {
		t.Fatalf("has bug: %v, %v", ok, err)
	}
	if ok, err := s.HasKind(ctx, "nope"); err != nil || ok {
		t.Fatalf("has nope: %v, %v", ok, err)
	}
	if kinds, _ := s.Kinds(ctx); !slices.Equal(kinds, []string{"task", "coding", "research", "discussion", "reminder", "bug"}) {
		t.Fatalf("kinds = %v", kinds)
	}

	for _, k := range []string{"task", "reminder"} {
		if err := s.DeleteKind(ctx, k); !errors.Is(err, ErrKindBuiltIn) || err.Error() != "kind "+k+" is built in" {
			t.Fatalf("delete %s: %v", k, err)
		}
	}
	if err := s.DeleteKind(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("delete nope: %v", err)
	}

	// Used by tasks only: they become kind task, archived ones too.
	a, _ := s.CreateTask(ctx, Task{Title: "a", Kind: "bug"})
	b, _ := s.CreateTask(ctx, Task{Title: "b", Kind: "bug"})
	if err := s.ArchiveTask(ctx, b.ID); err != nil {
		t.Fatal(err)
	}
	c, _ := s.CreateTask(ctx, Task{Title: "c", Kind: "coding"})
	uses, err := s.KindUses(ctx)
	if err != nil || uses[5] != (Kind{Name: "bug", Tasks: 2}) || uses[0] != (Kind{Name: "task", BuiltIn: true}) {
		t.Fatalf("uses = %+v, %v", uses, err)
	}
	if err := s.DeleteKind(ctx, "bug"); err != nil {
		t.Fatal(err)
	}
	for _, id := range []int64{a.ID, b.ID} {
		if task, _ := s.Task(ctx, id); task.Kind != "task" {
			t.Fatalf("task %d kind %q", id, task.Kind)
		}
	}
	if task, _ := s.Task(ctx, c.ID); task.Kind != "coding" {
		t.Fatalf("other task kind %q", task.Kind)
	}

	// Used by a rule or an automation: refused, nothing changes.
	if _, err := s.SaveRule(ctx, Rule{Name: "r", Kind: "coding", ToLaneID: 2, Prompt: "p"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SaveRule(ctx, Rule{Name: "r2", Kind: "coding", ToLaneID: 2, Prompt: "p"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SaveSkill(ctx, Skill{Name: "sk", Body: "b", Automations: []SkillAutomation{{Kind: "coding", Enabled: true}}}); err != nil {
		t.Fatal(err)
	}
	err = s.DeleteKind(ctx, "coding")
	if !errors.Is(err, ErrKindInUse) || err.Error() != "kind coding is used by 2 transition rules and 1 skill automation" {
		t.Fatalf("delete coding: %v", err)
	}
	if task, _ := s.Task(ctx, c.ID); task.Kind != "coding" {
		t.Fatalf("task kind %q after a refused delete", task.Kind)
	}
	if uses, _ := s.KindUses(ctx); uses[1] != (Kind{Name: "coding", Tasks: 1, Rules: 2, Automations: 1}) {
		t.Fatalf("coding uses = %+v", uses[1])
	}
	if ok, _ := s.HasKind(ctx, "coding"); !ok {
		t.Fatal("coding gone")
	}
	if err := s.DeleteKind(ctx, "discussion"); err != nil {
		t.Fatal(err)
	}
	if kinds, _ := s.Kinds(ctx); !slices.Equal(kinds, []string{"task", "coding", "research", "reminder"}) {
		t.Fatalf("kinds = %v", kinds)
	}
	// A new one goes at the end.
	if err := s.AddKind(ctx, "discussion"); err != nil {
		t.Fatal(err)
	}
	if kinds, _ := s.Kinds(ctx); !slices.Equal(kinds, []string{"task", "coding", "research", "reminder", "discussion"}) {
		t.Fatalf("kinds = %v", kinds)
	}
}
