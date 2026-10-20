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
	`
ALTER TABLE tasks ADD COLUMN due_at INTEGER;
ALTER TABLE tasks ADD COLUMN reminded_at INTEGER;
CREATE INDEX tasks_due ON tasks(due_at) WHERE reminded_at IS NULL;
`,
	// Sessions without a task (task_id NULL) and a per-session model.
	// SQLite can't drop NOT NULL in place, so the table is rebuilt.
	`
CREATE TABLE sessions_new (
	id INTEGER PRIMARY KEY,
	task_id INTEGER REFERENCES tasks(id) ON DELETE CASCADE,
	title TEXT NOT NULL DEFAULT '',
	status TEXT NOT NULL,
	mode TEXT NOT NULL,
	model TEXT NOT NULL DEFAULT '',
	workdir TEXT NOT NULL DEFAULT '',
	trigger TEXT NOT NULL DEFAULT 'manual',
	claude_session_id TEXT NOT NULL DEFAULT '',
	cost_usd REAL NOT NULL DEFAULT 0,
	error TEXT NOT NULL DEFAULT '',
	created_at INTEGER NOT NULL,
	updated_at INTEGER NOT NULL
);
INSERT INTO sessions_new (id, task_id, title, status, mode, workdir, trigger, claude_session_id, cost_usd, error, created_at, updated_at)
	SELECT id, task_id, title, status, mode, workdir, trigger, claude_session_id, cost_usd, error, created_at, updated_at FROM sessions;
DROP TABLE sessions;
ALTER TABLE sessions_new RENAME TO sessions;
CREATE INDEX sessions_task ON sessions(task_id);
`,
	// The lane a task moves to when an agent turn finishes successfully:
	// set per session, with defaults carried by rules and schedules.
	`
ALTER TABLE sessions ADD COLUMN done_lane_id INTEGER REFERENCES lanes(id) ON DELETE SET NULL;
ALTER TABLE transition_rules ADD COLUMN done_lane_id INTEGER REFERENCES lanes(id) ON DELETE SET NULL;
ALTER TABLE schedules ADD COLUMN done_lane_id INTEGER REFERENCES lanes(id) ON DELETE SET NULL;
`,
	// Archive: archived tasks leave the board but keep their lane. lane_changed_at
	// tells how long a task has sat in its lane (best guess for existing rows:
	// their last update). App settings live in a single row; auto-archive
	// starts on for a lane named Done, after a week.
	`
ALTER TABLE tasks ADD COLUMN archived_at INTEGER;
ALTER TABLE tasks ADD COLUMN lane_changed_at INTEGER NOT NULL DEFAULT 0;
UPDATE tasks SET lane_changed_at = updated_at;
CREATE TABLE settings (
	id INTEGER PRIMARY KEY CHECK (id = 1),
	archive_lane_id INTEGER REFERENCES lanes(id) ON DELETE SET NULL,
	archive_after_minutes INTEGER NOT NULL DEFAULT 0
);
INSERT INTO settings (id, archive_lane_id, archive_after_minutes)
	VALUES (1, (SELECT id FROM lanes WHERE lower(name) = 'done' ORDER BY position, id LIMIT 1), 10080);
`,
	// Unused: usage limits are live, kept in memory only. Databases that went
	// through an earlier build have a rate_limits table at this version instead.
	`
ALTER TABLE sessions ADD COLUMN limit_5h_used REAL;
ALTER TABLE sessions ADD COLUMN limit_5h_resets_at INTEGER;
ALTER TABLE sessions ADD COLUMN limit_7d_used REAL;
ALTER TABLE sessions ADD COLUMN limit_7d_resets_at INTEGER;
`,
	// Working directories offered when starting agents, a JSON array of paths.
	// Starts with every directory already used, most recent first.
	`
ALTER TABLE settings ADD COLUMN workdirs TEXT NOT NULL DEFAULT '[]';
UPDATE settings SET workdirs = (SELECT json_group_array(workdir) FROM (
	SELECT workdir FROM (
		SELECT workdir, updated_at FROM sessions
		UNION ALL SELECT workdir, updated_at FROM tasks
		UNION ALL SELECT workdir, 0 FROM transition_rules
	) WHERE workdir != '' GROUP BY workdir ORDER BY MAX(updated_at) DESC, workdir
));
`,
	// Projects: every task belongs to one; all boards share the lanes. Existing
	// tasks go to an Assistant project. Positions now count within a project's lane.
	// (Adding a REFERENCES column with a non-NULL default needs foreign keys off,
	// which they are while migrating.)
	`
CREATE TABLE projects (
	id INTEGER PRIMARY KEY,
	name TEXT NOT NULL,
	color TEXT NOT NULL DEFAULT '',
	position INTEGER NOT NULL
);
INSERT INTO projects (id, name, position) VALUES (1, 'Assistant', 0);
ALTER TABLE tasks ADD COLUMN project_id INTEGER NOT NULL DEFAULT 1 REFERENCES projects(id);
CREATE INDEX tasks_project ON tasks(project_id, lane_id, position);
`,
	// A session handed off to a fresh one points back at it.
	`
ALTER TABLE sessions ADD COLUMN parent_id INTEGER REFERENCES sessions(id) ON DELETE SET NULL;
CREATE INDEX sessions_parent ON sessions(parent_id);
`,
}

func (s *Store) migrate(ctx context.Context) error {
	var version int
	if err := s.db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version); err != nil {
		return err
	}
	if version == len(migrations) {
		return nil
	}
	// Table rebuilds drop the old table; with foreign keys on that would cascade
	// (e.g. dropping sessions deletes all messages). The pragma is a no-op inside
	// a transaction, so toggle it around the migrations and check afterwards.
	if _, err := s.db.ExecContext(ctx, `PRAGMA foreign_keys = OFF`); err != nil {
		return err
	}
	defer s.db.ExecContext(ctx, `PRAGMA foreign_keys = ON`)
	for i := version; i < len(migrations); i++ {
		err := s.tx(ctx, func(tx *sql.Tx) error {
			if _, err := tx.ExecContext(ctx, migrations[i]); err != nil {
				return err
			}
			rows, err := tx.QueryContext(ctx, `PRAGMA foreign_key_check`)
			if err != nil {
				return err
			}
			broken := rows.Next()
			rows.Close()
			if broken {
				return fmt.Errorf("foreign key violations")
			}
			_, err = tx.ExecContext(ctx, fmt.Sprintf(`PRAGMA user_version = %d`, i+1))
			return err
		})
		if err != nil {
			return fmt.Errorf("migration %d: %w", i+1, err)
		}
	}
	return nil
}
