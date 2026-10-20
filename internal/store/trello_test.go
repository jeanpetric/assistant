package store

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

func TestTrelloBoards(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	lanes, _ := s.Lanes(ctx)
	work, _ := s.CreateProject(ctx, Project{Name: "Work"})
	a, err := s.AddTrelloBoard(ctx, TrelloBoard{Board: "AbC", LaneID: &lanes[1].ID, DoneLaneID: &lanes[3].ID})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := s.AddTrelloBoard(ctx, TrelloBoard{Board: "XyZ", ProjectID: &work.ID})
	b.DoneLaneID = &lanes[4].ID
	if err := s.UpdateTrelloBoard(ctx, b); err != nil {
		t.Fatal(err)
	}
	boards, _ := s.TrelloBoards(ctx)
	if len(boards) != 2 || boards[0].Board != "AbC" || boards[0].ProjectID != nil || *boards[0].LaneID != lanes[1].ID ||
		*boards[0].DoneLaneID != lanes[3].ID || boards[1].ID != b.ID || *boards[1].ProjectID != work.ID || *boards[1].DoneLaneID != lanes[4].ID {
		t.Fatalf("boards = %+v", boards)
	}
	if got, err := s.TrelloBoard(ctx, b.ID); err != nil || got.Board != "XyZ" {
		t.Fatalf("board = %+v, %v", got, err)
	}
	if _, err := s.TrelloBoard(ctx, 99); err != ErrNotFound {
		t.Fatalf("missing board: %v", err)
	}

	due := time.UnixMilli(1_000_000)
	task, _ := s.CreateTask(ctx, Task{Title: "card", LaneID: lanes[1].ID, DueAt: &due})
	if err := s.AddTrelloCard(ctx, a.ID, "c1", task.ID, &due); err != nil {
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
	if c := cards["c1"]; *c.BoardID != a.ID || *c.TaskID != task.ID || c.LaneID != lanes[1].ID || !c.DueAt.Equal(later) || !c.Closed {
		t.Fatalf("card = %+v", c)
	}
	if err := s.SetTrelloCardBoard(ctx, "c1", b.ID); err != nil {
		t.Fatal(err)
	}
	if cards, _ = s.TrelloCards(ctx); *cards["c1"].BoardID != b.ID {
		t.Fatalf("card board = %d", *cards["c1"].BoardID)
	}

	// A removed board keeps its cards' records, so they are not imported again.
	if err := s.DeleteTrelloBoard(ctx, b.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteTrelloBoard(ctx, b.ID); err != ErrNotFound {
		t.Fatalf("delete again: %v", err)
	}
	if cards, _ = s.TrelloCards(ctx); cards["c1"].BoardID != nil || *cards["c1"].TaskID != task.ID {
		t.Fatalf("card after board removed = %+v", cards["c1"])
	}
	// So does a deleted task.
	s.DeleteTask(ctx, task.ID)
	cards, _ = s.TrelloCards(ctx)
	if c, ok := cards["c1"]; !ok || c.TaskID != nil || c.LaneID != 0 {
		t.Fatalf("card after delete = %+v, %v", c, ok)
	}
	// Removing a lane clears it from the boards.
	s.DeleteLane(ctx, lanes[3].ID)
	if got, _ := s.TrelloBoard(ctx, a.ID); got.DoneLaneID != nil {
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
	if boards, err := s.TrelloBoards(ctx); err != nil || len(boards) != 0 {
		t.Fatalf("trello boards = %+v, %v", boards, err)
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

// oldTrelloDB is a database with the single-board Trello settings, before
// boards of their own.
func oldTrelloDB(t *testing.T, qs ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	const n = 21 // migrations before Trello boards
	for _, m := range migrations[:n] {
		if _, err := db.Exec(m); err != nil {
			t.Fatal(err)
		}
	}
	for _, q := range append([]string{fmt.Sprintf(`PRAGMA user_version = %d`, n)}, qs...) {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(q, err)
		}
	}
	db.Close()
	return path
}

func TestTrelloBoardsMigrationKeepsBoard(t *testing.T) {
	path := oldTrelloDB(t,
		`INSERT INTO projects (id, name, position) VALUES (2, 'Private', 1)`,
		`UPDATE settings SET trello_board = 'https://trello.com/b/StAn/stanista', trello_project_id = 2, trello_lane_id = 2, trello_done_lane_id = 5,
			archive_lane_id = 5, archive_after_minutes = 60`,
		`INSERT INTO tasks (id, title, lane_id, project_id, position, due_at, created_at, updated_at) VALUES (1, 'Paint', 2, 2, 0, 777, 0, 0), (2, 'Fix', 5, 2, 0, NULL, 0, 0)`,
		`INSERT INTO task_fields (task_id, key, value) VALUES (1, 'trello', 'https://trello.com/c/1')`,
		`INSERT INTO trello_cards (card_id, task_id, due_at, closed) VALUES ('c1', 1, 777, 0), ('c2', 2, NULL, 1), ('c3', NULL, NULL, 0)`,
	)
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	boards, err := s.TrelloBoards(ctx)
	if err != nil || len(boards) != 1 {
		t.Fatalf("boards = %+v, %v", boards, err)
	}
	b := boards[0]
	if b.Board != "https://trello.com/b/StAn/stanista" || *b.ProjectID != 2 || *b.LaneID != 2 || *b.DoneLaneID != 5 {
		t.Fatalf("board = %+v", b)
	}
	cards, _ := s.TrelloCards(ctx)
	if len(cards) != 3 {
		t.Fatalf("cards = %+v", cards)
	}
	for id, c := range cards {
		if c.BoardID == nil || *c.BoardID != b.ID {
			t.Fatalf("card %s = %+v", id, c)
		}
	}
	if c := cards["c1"]; *c.TaskID != 1 || c.LaneID != 2 || c.DueAt.UnixMilli() != 777 || c.Closed {
		t.Fatalf("c1 = %+v", c)
	}
	if c := cards["c2"]; *c.TaskID != 2 || !c.Closed {
		t.Fatalf("c2 = %+v", c)
	}
	if c := cards["c3"]; c.TaskID != nil {
		t.Fatalf("c3 = %+v", c)
	}
	task, err := s.Task(ctx, 1)
	if err != nil || task.Title != "Paint" || task.ProjectID != 2 || task.DueAt.UnixMilli() != 777 || task.Fields["trello"] != "https://trello.com/c/1" {
		t.Fatalf("task = %+v, %v", task, err)
	}
	if set, _ := s.Settings(ctx); *set.ArchiveLaneID != 5 || set.ArchiveAfterMinutes != 60 {
		t.Fatalf("settings = %+v", set)
	}
	// Ids go on after the migrated board.
	if nb, err := s.AddTrelloBoard(ctx, TrelloBoard{Board: "other"}); err != nil || nb.ID == b.ID {
		t.Fatalf("new board = %+v, %v", nb, err)
	}
}

// Without a board in the settings there is none to carry over; cards imported
// before the board was cleared are kept, without a board.
func TestTrelloBoardsMigrationWithoutBoard(t *testing.T) {
	path := oldTrelloDB(t,
		`INSERT INTO tasks (id, title, lane_id, position, created_at, updated_at) VALUES (1, 't', 2, 0, 0, 0)`,
		`INSERT INTO trello_cards (card_id, task_id) VALUES ('c1', 1)`,
	)
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	if boards, err := s.TrelloBoards(ctx); err != nil || len(boards) != 0 {
		t.Fatalf("boards = %+v, %v", boards, err)
	}
	if cards, _ := s.TrelloCards(ctx); cards["c1"].BoardID != nil || *cards["c1"].TaskID != 1 {
		t.Fatalf("cards = %+v", cards)
	}
}
