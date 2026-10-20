package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
)

func TestSkillsCRUDAndMatching(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	lanes, _ := s.Lanes(ctx)
	review := lanes[3].ID

	// The example skill ships with its automation off.
	all, err := s.Skills(ctx)
	if err != nil || len(all) != 1 || all[0].Name != "Research summary" || !all[0].Artifact ||
		len(all[0].Automations) != 1 || all[0].Automations[0].Kind != "research" || all[0].Automations[0].Enabled {
		t.Fatalf("seeded skills = %+v, %v", all, err)
	}
	if got, _ := s.MatchingSkills(ctx, "research", 1, lanes[0].ID); len(got) != 0 {
		t.Fatalf("a disabled automation must not match: %+v", got)
	}

	p := int64(1)
	sk, err := s.SaveSkill(ctx, Skill{Name: "Review notes", Body: "notes", Automations: []SkillAutomation{
		{Kind: "coding", ProjectID: &p, Enabled: true},
		{LaneID: &review, Enabled: true}, // also matches coding tasks in Review: the skill runs once
	}})
	if err != nil || sk.ID == 0 || len(sk.Automations) != 2 || *sk.Automations[1].LaneID != review {
		t.Fatalf("saved = %+v, %v", sk, err)
	}
	if _, err := s.SaveSkill(ctx, Skill{Name: "review NOTES", Body: "x"}); !errors.Is(err, ErrSkillExists) {
		t.Fatalf("duplicate name: %v", err)
	}

	names := func(kind string, project, lane int64) []string {
		got, err := s.MatchingSkills(ctx, kind, project, lane)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, x := range got {
			out = append(out, x.Name)
		}
		return out
	}
	if got := names("coding", 1, review); len(got) != 1 || got[0] != "Review notes" {
		t.Fatalf("coding in review = %v", got)
	}
	if got := names("coding", 2, lanes[0].ID); len(got) != 0 {
		t.Fatalf("other project = %v", got)
	}
	if got := names("task", 2, review); len(got) != 1 {
		t.Fatalf("any task in review = %v", got)
	}

	// Saving replaces the automations.
	sk.Automations = []SkillAutomation{{Kind: "research", Enabled: true}}
	sk.Artifact, sk.Model = true, "fable"
	if sk, err = s.SaveSkill(ctx, sk); err != nil || len(sk.Automations) != 1 || !sk.Artifact || sk.Model != "fable" {
		t.Fatalf("updated = %+v, %v", sk, err)
	}
	if got := names("coding", 1, review); len(got) != 0 {
		t.Fatalf("old automations still match: %v", got)
	}

	// Deleting the lane takes automations bound to it along.
	sk.Automations = []SkillAutomation{{LaneID: &review, Enabled: true}}
	s.SaveSkill(ctx, sk)
	if _, err := s.db.ExecContext(ctx, `DELETE FROM lanes WHERE id = ?`, review); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Skill(ctx, sk.ID); len(got.Automations) != 0 {
		t.Fatalf("automations = %+v", got.Automations)
	}

	if err := s.DeleteSkill(ctx, sk.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Skill(ctx, sk.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted skill: %v", err)
	}
}

func TestArtifacts(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	lanes, _ := s.Lanes(ctx)
	task, _ := s.CreateTask(ctx, Task{Title: "Research", LaneID: lanes[0].ID})
	sess, _ := s.CreateSession(ctx, Session{TaskID: task.ID, Title: "s", Mode: "plan"})

	a, err := s.AddArtifact(ctx, Artifact{TaskID: task.ID, SessionID: &sess.ID, Skill: "Summary", Content: "# One"})
	if err != nil || a.ID == 0 {
		t.Fatalf("add = %+v, %v", a, err)
	}
	s.AddArtifact(ctx, Artifact{TaskID: task.ID, Skill: "Summary", Content: "# Two"})
	got, err := s.Artifacts(ctx, task.ID)
	if err != nil || len(got) != 2 || got[0].Content != "# Two" || got[1].SessionID == nil || *got[1].SessionID != sess.ID {
		t.Fatalf("artifacts = %+v, %v", got, err)
	}
	if err := s.DeleteArtifact(ctx, got[0].ID); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteArtifact(ctx, got[0].ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second delete: %v", err)
	}
	if err := s.DeleteTask(ctx, task.ID); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Artifacts(ctx, task.ID); len(got) != 0 {
		t.Fatalf("artifacts outlived their task: %+v", got)
	}
}

func TestSkillsMigrationKeepsData(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	const n = 11 // migrations before skills
	for _, m := range migrations[:n] {
		if _, err := db.Exec(m); err != nil {
			t.Fatal(err)
		}
	}
	for _, q := range []string{
		`PRAGMA user_version = 11`,
		`INSERT INTO tasks (id, title, kind, lane_id, position, created_at, updated_at) VALUES (1, 'old', 'research', 1, 0, 0, 0), (2, 'next', 'task', 1, 1, 0, 0)`,
		`INSERT INTO task_context (task_id, source_id, position) VALUES (2, 1, 0)`,
		`INSERT INTO sessions (id, task_id, title, status, mode, created_at, updated_at) VALUES (7, 1, 'work', 'idle', 'plan', 0, 0)`,
		`INSERT INTO messages (session_id, role, kind, content, created_at) VALUES (7, 'user', 'text', 'hi', 0)`,
		`INSERT INTO transition_rules (name, to_lane_id, prompt) VALUES ('r', 2, 'p')`,
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
	if old, err := s.Task(ctx, 1); err != nil || old.Title != "old" || old.Kind != "research" {
		t.Fatalf("task = %+v, %v", old, err)
	}
	if next, _ := s.Task(ctx, 2); len(next.ContextFrom) != 1 {
		t.Fatalf("next = %+v", next)
	}
	if x, err := s.Session(ctx, 7); err != nil || x.LastMessage != "hi" {
		t.Fatalf("session = %+v, %v", x, err)
	}
	if rules, _ := s.Rules(ctx); len(rules) != 1 {
		t.Fatalf("rules = %+v", rules)
	}
	skills, err := s.Skills(ctx)
	if err != nil || len(skills) != 1 || skills[0].CreatedAt.Year() < 2026 {
		t.Fatalf("skills = %+v, %v", skills, err)
	}
	if _, err := s.AddArtifact(ctx, Artifact{TaskID: 1, Skill: "x", Content: "y"}); err != nil {
		t.Fatal(err)
	}
}

func TestSkillModelMigrationKeepsSkills(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	const n = 13 // migrations before the skill model
	for _, m := range migrations[:n] {
		if _, err := db.Exec(m); err != nil {
			t.Fatal(err)
		}
	}
	for _, q := range []string{
		`PRAGMA user_version = 13`,
		`INSERT INTO tasks (id, title, kind, lane_id, position, created_at, updated_at) VALUES (1, 'old', 'research', 1, 0, 0, 0)`,
		`INSERT INTO skills (id, name, description, body, artifact, created_at, updated_at) VALUES (5, 'Mine', 'd', 'b', 1, 0, 0)`,
		`INSERT INTO skill_automations (skill_id, kind, enabled) VALUES (5, 'research', 1)`,
		`INSERT INTO artifacts (task_id, skill, content, created_at) VALUES (1, 'Mine', 'kept', 0)`,
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
	sk, err := s.Skill(ctx, 5)
	if err != nil || sk.Name != "Mine" || sk.Body != "b" || !sk.Artifact || sk.Model != "" || len(sk.Automations) != 1 || !sk.Automations[0].Enabled {
		t.Fatalf("skill = %+v, %v", sk, err)
	}
	if all, _ := s.Skills(ctx); len(all) != 2 {
		t.Fatalf("skills = %+v", all)
	}
	if arts, _ := s.Artifacts(ctx, 1); len(arts) != 1 || arts[0].Content != "kept" {
		t.Fatalf("artifacts = %+v", arts)
	}
	sk.Model = "fable"
	if sk, err = s.SaveSkill(ctx, sk); err != nil || sk.Model != "fable" {
		t.Fatalf("saved = %+v, %v", sk, err)
	}
}
