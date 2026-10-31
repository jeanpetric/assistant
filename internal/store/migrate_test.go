package store

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
)

// Removing the Trello sync drops its tables, columns, field and sync
// bookkeeping; tasks it imported stay, with their other fields.
func TestTrelloRemovalMigration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	n := migrationWith(t, "DROP TABLE trello_cards")
	for _, m := range migrations[:n] {
		if _, err := db.Exec(m); err != nil {
			t.Fatal(err)
		}
	}
	for _, q := range []string{
		fmt.Sprintf(`PRAGMA user_version = %d`, n),
		`UPDATE settings SET trello_board = 'b', trello_lane_id = 1 WHERE id = 1`,
		`INSERT INTO tasks (id, title, lane_id, position, project_id, created_at, updated_at) VALUES (5, 'Card', 1, 0, 1, 0, 0)`,
		`INSERT INTO task_fields (task_id, key, value) VALUES (5, 'trello', 'https://trello.com/c/1'), (5, 'slack', 'https://a.slack.com/archives/C1')`,
		`INSERT INTO trello_boards (id, board, lane_id) VALUES (1, 'b', 1)`,
		`INSERT INTO trello_cards (card_id, task_id, board_id) VALUES ('c1', 5, 1)`,
		`INSERT INTO sync_rows (tbl, pk_json, changed_at_ms, node_id) VALUES ('trello_cards', '["c1"]', 1, 1), ('tasks', '[5]', 1, 1)`,
		`INSERT INTO sync_outbox (tbl, pk_json, op, changed_at_ms, node_id) VALUES ('trello_boards', '[1]', 'insert', 1, 1), ('tasks', '[5]', 'insert', 1, 1)`,
		`INSERT INTO id_blocks (tbl, next_id, end_id) VALUES ('trello_boards', 2, 999), ('tasks', 6, 999)`,
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
	task, err := s.Task(ctx, 5)
	if err != nil || task.Title != "Card" {
		t.Fatalf("task = %+v, %v", task, err)
	}
	if len(task.Fields) != 1 || task.Fields["slack"] == "" {
		t.Fatalf("fields = %v", task.Fields)
	}
	for q, want := range map[string]int{
		`SELECT COUNT(*) FROM sqlite_master WHERE name LIKE 'trello%'`:                 0,
		`SELECT COUNT(*) FROM pragma_table_info('settings') WHERE name LIKE 'trello%'`: 0,
		`SELECT COUNT(*) FROM field_defs WHERE key = 'trello'`:                         0,
		`SELECT COUNT(*) FROM sync_rows WHERE tbl LIKE 'trello%'`:                      0,
		`SELECT COUNT(*) FROM sync_outbox WHERE tbl LIKE 'trello%'`:                    0,
		`SELECT COUNT(*) FROM id_blocks WHERE tbl LIKE 'trello%'`:                      0,
		`SELECT COUNT(*) FROM sync_rows WHERE tbl = 'tasks'`:                           1,
		`SELECT COUNT(*) FROM sync_outbox WHERE tbl = 'tasks'`:                         1,
		`SELECT COUNT(*) FROM id_blocks WHERE tbl = 'tasks'`:                           1,
	} {
		var got int
		if err := s.db.QueryRow(q).Scan(&got); err != nil || got != want {
			t.Errorf("%s = %d, %v; want %d", q, got, err, want)
		}
	}
}
