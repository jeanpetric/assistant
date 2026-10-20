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
	// A project's default working directory for agents on its tasks.
	`
ALTER TABLE projects ADD COLUMN workdir TEXT NOT NULL DEFAULT '';
`,
	// A task can build on earlier tasks: their agents' results go into the
	// context of its agents.
	`
CREATE TABLE task_context (
	task_id INTEGER NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
	source_id INTEGER NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
	position INTEGER NOT NULL,
	PRIMARY KEY (task_id, source_id),
	CHECK (task_id != source_id)
);
CREATE INDEX task_context_source ON task_context(source_id);
`,
	// Skills: instructions an agent applies to the work of its session, run
	// after a successful turn when one of the skill's automations matches the
	// task. A skill's reply can be kept as an artifact on the task. Starts with
	// an example research summary whose automation is off.
	`
CREATE TABLE skills (
	id INTEGER PRIMARY KEY,
	name TEXT NOT NULL UNIQUE COLLATE NOCASE,
	description TEXT NOT NULL DEFAULT '',
	body TEXT NOT NULL,
	artifact INTEGER NOT NULL DEFAULT 0,
	created_at INTEGER NOT NULL,
	updated_at INTEGER NOT NULL
);
CREATE TABLE skill_automations (
	id INTEGER PRIMARY KEY,
	skill_id INTEGER NOT NULL REFERENCES skills(id) ON DELETE CASCADE,
	kind TEXT NOT NULL DEFAULT '',
	project_id INTEGER REFERENCES projects(id) ON DELETE CASCADE,
	lane_id INTEGER REFERENCES lanes(id) ON DELETE CASCADE,
	enabled INTEGER NOT NULL DEFAULT 1
);
CREATE INDEX skill_automations_skill ON skill_automations(skill_id);
CREATE TABLE artifacts (
	id INTEGER PRIMARY KEY,
	task_id INTEGER NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
	session_id INTEGER REFERENCES sessions(id) ON DELETE SET NULL,
	skill TEXT NOT NULL DEFAULT '',
	content TEXT NOT NULL,
	created_at INTEGER NOT NULL
);
CREATE INDEX artifacts_task ON artifacts(task_id, id);
INSERT INTO skills (id, name, description, body, artifact, created_at, updated_at) VALUES (1, 'Research summary',
	'Sums up a research session as a short report',
	'Write a summary of the research done in this session, for someone who did not follow it.

Use this format:

# {{.Task.Title}}

## TL;DR
The answer in two or three sentences.

## Findings
- One bullet per finding, each with its source (link, file or command).

## Open questions

## Recommended next steps', 1, strftime('%s', 'now') * 1000, strftime('%s', 'now') * 1000);
INSERT INTO skill_automations (skill_id, kind, enabled) VALUES (1, 'research', 0);
`,
	// A session's summary, written for the agents of tasks that build on its
	// task. summary_upto is the last message it covers: newer messages make it stale.
	`
ALTER TABLE sessions ADD COLUMN summary TEXT NOT NULL DEFAULT '';
ALTER TABLE sessions ADD COLUMN summary_upto INTEGER NOT NULL DEFAULT 0;
ALTER TABLE sessions ADD COLUMN summary_at INTEGER;
`,
	// The model a skill's turn runs with; empty keeps the session's.
	`
ALTER TABLE skills ADD COLUMN model TEXT NOT NULL DEFAULT '';
`,
	// Memories: knowledge kept across tasks, a Markdown document each. Tasks
	// link to them; their agents read them and update them after each turn.
	// Every change is a revision; memories.revision is the latest one's id.
	// A memory with a project_id is linked to that project's new tasks.
	`
CREATE TABLE memories (
	id INTEGER PRIMARY KEY,
	name TEXT NOT NULL UNIQUE COLLATE NOCASE,
	description TEXT NOT NULL DEFAULT '',
	body TEXT NOT NULL DEFAULT '',
	revision INTEGER NOT NULL DEFAULT 0,
	project_id INTEGER REFERENCES projects(id) ON DELETE SET NULL,
	created_at INTEGER NOT NULL,
	updated_at INTEGER NOT NULL
);
CREATE TABLE memory_revisions (
	id INTEGER PRIMARY KEY,
	memory_id INTEGER NOT NULL REFERENCES memories(id) ON DELETE CASCADE,
	body TEXT NOT NULL,
	source TEXT NOT NULL,
	session_id INTEGER REFERENCES sessions(id) ON DELETE SET NULL,
	task_id INTEGER REFERENCES tasks(id) ON DELETE SET NULL,
	created_at INTEGER NOT NULL
);
CREATE INDEX memory_revisions_memory ON memory_revisions(memory_id, id);
CREATE TABLE task_memories (
	task_id INTEGER NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
	memory_id INTEGER NOT NULL REFERENCES memories(id) ON DELETE CASCADE,
	PRIMARY KEY (task_id, memory_id)
);
CREATE INDEX task_memories_memory ON task_memories(memory_id);
`,
	// Why a session's agent is waiting for the user; empty when it is not.
	// A notification can point at the session it is about.
	`
ALTER TABLE sessions ADD COLUMN needs_input TEXT NOT NULL DEFAULT '';
ALTER TABLE notifications ADD COLUMN session_id INTEGER REFERENCES sessions(id) ON DELETE SET NULL;
`,
	// A memory is a topic of its own, not tied to a project: project_id goes,
	// the memories, their revisions and task links stay.
	`
CREATE TABLE memories_new (
	id INTEGER PRIMARY KEY,
	name TEXT NOT NULL UNIQUE COLLATE NOCASE,
	description TEXT NOT NULL DEFAULT '',
	body TEXT NOT NULL DEFAULT '',
	revision INTEGER NOT NULL DEFAULT 0,
	created_at INTEGER NOT NULL,
	updated_at INTEGER NOT NULL
);
INSERT INTO memories_new (id, name, description, body, revision, created_at, updated_at)
	SELECT id, name, description, body, revision, created_at, updated_at FROM memories;
DROP TABLE memories;
ALTER TABLE memories_new RENAME TO memories;
`,
	// seen: the user has taken in how the session's last turn ended. Sessions
	// that ended before this existed count as seen, so the board does not light
	// up with old outcomes; ones waiting for the user keep showing.
	`
ALTER TABLE sessions ADD COLUMN seen INTEGER NOT NULL DEFAULT 0;
UPDATE sessions SET seen = 1 WHERE status != 'running' AND needs_input = '';
`,
	// The project a standalone session was started in (a task's session follows
	// its task's project). Older ones get the project whose directory they ran in.
	`
ALTER TABLE sessions ADD COLUMN project_id INTEGER REFERENCES projects(id) ON DELETE SET NULL;
UPDATE sessions SET project_id = (SELECT p.id FROM projects p WHERE p.workdir != '' AND p.workdir = sessions.workdir ORDER BY p.position, p.id LIMIT 1)
	WHERE task_id IS NULL;
`,
	// How full the session's context is: tokens in it, and the model's window (0: unknown).
	`
ALTER TABLE sessions ADD COLUMN context_tokens INTEGER NOT NULL DEFAULT 0;
ALTER TABLE sessions ADD COLUMN context_window INTEGER NOT NULL DEFAULT 0;
`,
	// Trello sync: the board, where its cards land, which lane archives them,
	// and the cards already imported.
	`
ALTER TABLE settings ADD COLUMN trello_board TEXT NOT NULL DEFAULT '';
ALTER TABLE settings ADD COLUMN trello_project_id INTEGER REFERENCES projects(id) ON DELETE SET NULL;
ALTER TABLE settings ADD COLUMN trello_lane_id INTEGER REFERENCES lanes(id) ON DELETE SET NULL;
ALTER TABLE settings ADD COLUMN trello_done_lane_id INTEGER REFERENCES lanes(id) ON DELETE SET NULL;
CREATE TABLE trello_cards (
	card_id TEXT PRIMARY KEY,
	task_id INTEGER REFERENCES tasks(id) ON DELETE SET NULL,
	due_at INTEGER,
	closed INTEGER NOT NULL DEFAULT 0
);
INSERT OR IGNORE INTO field_defs (key, label, type, position)
	VALUES ('trello', 'Trello card', 'url', (SELECT COALESCE(MAX(position), -1) + 1 FROM field_defs));
`,
	// Several Trello boards, each with its own project and lanes. The board set
	// in the settings becomes the first; its cards stay imported. A card belongs
	// to the board it was last seen on (NULL: its board was removed). The
	// settings' trello_* columns are left unused.
	`
CREATE TABLE trello_boards (
	id INTEGER PRIMARY KEY,
	board TEXT NOT NULL,
	project_id INTEGER REFERENCES projects(id) ON DELETE SET NULL,
	lane_id INTEGER REFERENCES lanes(id) ON DELETE SET NULL,
	done_lane_id INTEGER REFERENCES lanes(id) ON DELETE SET NULL
);
INSERT INTO trello_boards (id, board, project_id, lane_id, done_lane_id)
	SELECT 1, trello_board, trello_project_id, trello_lane_id, trello_done_lane_id FROM settings WHERE id = 1 AND trello_board != '';
ALTER TABLE trello_cards ADD COLUMN board_id INTEGER REFERENCES trello_boards(id) ON DELETE SET NULL;
UPDATE trello_cards SET board_id = (SELECT id FROM trello_boards WHERE id = 1);
CREATE INDEX trello_cards_board ON trello_cards(board_id);
`,
	// Archived notifications leave the panel; their rows stay.
	`
ALTER TABLE notifications ADD COLUMN archived_at INTEGER;
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
