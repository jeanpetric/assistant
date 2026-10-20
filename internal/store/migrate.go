package store

import (
	"context"
	"database/sql"
	"fmt"
)

// migrations are applied in order; PRAGMA user_version tracks progress.
// Append new entries, never edit applied ones.
var migrations = []string{
	`
CREATE TABLE lanes (
	id INTEGER PRIMARY KEY,
	name TEXT NOT NULL,
	color TEXT NOT NULL DEFAULT '',
	position INTEGER NOT NULL
);
CREATE TABLE tasks (
	id INTEGER PRIMARY KEY,
	title TEXT NOT NULL,
	description TEXT NOT NULL DEFAULT '',
	kind TEXT NOT NULL DEFAULT 'task',
	lane_id INTEGER NOT NULL REFERENCES lanes(id),
	position INTEGER NOT NULL,
	workdir TEXT NOT NULL DEFAULT '',
	created_at INTEGER NOT NULL,
	updated_at INTEGER NOT NULL
);
CREATE INDEX tasks_lane ON tasks(lane_id, position);
CREATE TABLE field_defs (
	key TEXT PRIMARY KEY,
	label TEXT NOT NULL,
	type TEXT NOT NULL DEFAULT 'text',
	position INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE task_fields (
	task_id INTEGER NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
	key TEXT NOT NULL REFERENCES field_defs(key) ON DELETE CASCADE,
	value TEXT NOT NULL,
	PRIMARY KEY (task_id, key)
);
CREATE TABLE sessions (
	id INTEGER PRIMARY KEY,
	task_id INTEGER NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
	title TEXT NOT NULL DEFAULT '',
	status TEXT NOT NULL,
	mode TEXT NOT NULL,
	workdir TEXT NOT NULL DEFAULT '',
	trigger TEXT NOT NULL DEFAULT 'manual',
	claude_session_id TEXT NOT NULL DEFAULT '',
	cost_usd REAL NOT NULL DEFAULT 0,
	error TEXT NOT NULL DEFAULT '',
	created_at INTEGER NOT NULL,
	updated_at INTEGER NOT NULL
);
CREATE INDEX sessions_task ON sessions(task_id);
CREATE TABLE messages (
	id INTEGER PRIMARY KEY,
	session_id INTEGER NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
	role TEXT NOT NULL,
	kind TEXT NOT NULL,
	content TEXT NOT NULL,
	created_at INTEGER NOT NULL
);
CREATE INDEX messages_session ON messages(session_id, id);
CREATE TABLE transition_rules (
	id INTEGER PRIMARY KEY,
	name TEXT NOT NULL,
	from_lane_id INTEGER REFERENCES lanes(id) ON DELETE CASCADE,
	to_lane_id INTEGER NOT NULL REFERENCES lanes(id) ON DELETE CASCADE,
	prompt TEXT NOT NULL,
	mode TEXT NOT NULL DEFAULT '',
	workdir TEXT NOT NULL DEFAULT '',
	enabled INTEGER NOT NULL DEFAULT 1
);
CREATE TABLE schedules (
	id INTEGER PRIMARY KEY,
	task_id INTEGER NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
	cron TEXT NOT NULL,
	action TEXT NOT NULL,
	prompt TEXT NOT NULL DEFAULT '',
	mode TEXT NOT NULL DEFAULT '',
	enabled INTEGER NOT NULL DEFAULT 1,
	next_run_at INTEGER,
	last_run_at INTEGER
);
CREATE TABLE notifications (
	id INTEGER PRIMARY KEY,
	task_id INTEGER REFERENCES tasks(id) ON DELETE CASCADE,
	message TEXT NOT NULL,
	created_at INTEGER NOT NULL,
	read_at INTEGER
);
INSERT INTO lanes (name, position) VALUES ('Backlog', 0), ('Todo', 1), ('In progress', 2), ('Review', 3), ('Done', 4);
INSERT INTO field_defs (key, label, type, position) VALUES
	('productive', 'Productive task', 'url', 0),
	('pull_request', 'Pull request', 'url', 1),
	('slack', 'Slack thread', 'url', 2);
`,
}

func (s *Store) migrate(ctx context.Context) error {
	var version int
	if err := s.db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version); err != nil {
		return err
	}
	for i := version; i < len(migrations); i++ {
		err := s.tx(ctx, func(tx *sql.Tx) error {
			if _, err := tx.ExecContext(ctx, migrations[i]); err != nil {
				return err
			}
			_, err := tx.ExecContext(ctx, fmt.Sprintf(`PRAGMA user_version = %d`, i+1))
			return err
		})
		if err != nil {
			return fmt.Errorf("migration %d: %w", i+1, err)
		}
	}
	return nil
}
