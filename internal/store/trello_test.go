package store

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

func TestTrelloCards(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	lanes, _ := s.Lanes(ctx)
	set := TrelloSettings{Board: "AbC", LaneID: &lanes[1].ID, DoneLaneID: &lanes[3].ID}
	if err := s.SaveTrelloSettings(ctx, set); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.TrelloSettings(ctx); got.Board != "AbC" || got.ProjectID != nil || *got.LaneID != lanes[1].ID || *got.DoneLaneID != lanes[3].ID {
		t.Fatalf("settings = %+v", got)
	}

	due := time.UnixMilli(1_000_000)
	task, _ := s.CreateTask(ctx, Task{Title: "card", LaneID: lanes[1].ID, DueAt: &due})
	if err := s.AddTrelloCard(ctx, "c1", task.ID, &due); err != nil {
		t.Fatal(err)
	}
	s.MarkReminded(ctx, task.ID, due)
	later := due.Add(time.Hour)
	if err := s.SetTrelloDue(ctx, "c1", &later); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Task(ctx, task.ID); !got.DueAt.Equal(later) || got.RemindedAt != nil {
		t.Fatalf("task = %+v", got)
	}
	if err := s.SetTrelloClosed(ctx, "c1", true); err != nil {
		t.Fatal(err)
	}
	cards, _ := s.TrelloCards(ctx)
	if c := cards["c1"]; *c.TaskID != task.ID || c.LaneID != lanes[1].ID || !c.DueAt.Equal(later) || !c.Closed {
		t.Fatalf("card = %+v", c)
	}

	// A deleted task keeps its card's record, so the card is not imported again.
	s.DeleteTask(ctx, task.ID)
	cards, _ = s.TrelloCards(ctx)
	if c, ok := cards["c1"]; !ok || c.TaskID != nil || c.LaneID != 0 {
		t.Fatalf("card after delete = %+v, %v", c, ok)
	}
	// Removing a lane clears it from the settings.
	s.DeleteLane(ctx, lanes[3].ID)
	if got, _ := s.TrelloSettings(ctx); got.DoneLaneID != nil {
		t.Fatalf("done lane after delete = %v", *got.DoneLaneID)
	}
}

func TestTrelloMigrationKeepsData(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	const n = 20 // migrations before Trello
	for _, m := range migrations[:n] {
		if _, err := db.Exec(m); err != nil {
			t.Fatal(err)
		}
	}
	for _, q := range []string{
		fmt.Sprintf(`PRAGMA user_version = %d`, n),
		`UPDATE settings SET archive_lane_id = 5, archive_after_minutes = 60, workdirs = '["/w"]'`,
		`INSERT INTO field_defs (key, label, type, position) VALUES ('jira', 'Jira', 'url', 3)`,
		`INSERT INTO tasks (id, title, lane_id, position, due_at, created_at, updated_at) VALUES (1, 't', 2, 0, 777, 0, 0)`,
		`INSERT INTO task_fields (task_id, key, value) VALUES (1, 'slack', 'https://slack/x')`,
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
	set, err := s.Settings(ctx)
	if err != nil || *set.ArchiveLaneID != 5 || set.ArchiveAfterMinutes != 60 || len(set.Workdirs) != 1 {
		t.Fatalf("settings = %+v, %v", set, err)
	}
	if tr, err := s.TrelloSettings(ctx); err != nil || tr.Board != "" || tr.DoneLaneID != nil {
		t.Fatalf("trello settings = %+v, %v", tr, err)
	}
	task, err := s.Task(ctx, 1)
	if err != nil || task.Title != "t" || task.LaneID != 2 || task.DueAt.UnixMilli() != 777 || task.Fields["slack"] != "https://slack/x" {
		t.Fatalf("task = %+v, %v", task, err)
	}
	defs, _ := s.FieldDefs(ctx)
	if len(defs) != 5 || defs[3].Key != "jira" || defs[4].Key != "trello" || defs[4].Position != 4 {
		t.Fatalf("field defs = %+v", defs)
	}
}
