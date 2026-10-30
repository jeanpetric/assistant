package store

import (
	"context"
	"database/sql"
	"time"
)

// TaskMessage is a short message on a task's activity thread: from one of its
// agent sessions (SessionID), or from the user (SessionID nil). A question
// waits for the user's answer until AnsweredAt.
type TaskMessage struct {
	ID        int64  `json:"id"`
	TaskID    int64  `json:"task_id"`
	SessionID *int64 `json:"session_id"`
	// SessionTitle names the author session; empty for the user, or once the
	// session is deleted.
	SessionTitle string `json:"session_title"`
	// SessionNode names the computer the author session runs on when that is
	// another node, which alone can pass it an answer; empty otherwise.
	SessionNode string     `json:"session_node,omitempty"`
	Body        string     `json:"body"`
	Question    bool       `json:"question"`
	AnsweredAt  *time.Time `json:"answered_at"`
	CreatedAt   time.Time  `json:"created_at"`
}

const taskMessageCols = `m.id, m.task_id, m.session_id, COALESCE(s.title, ''),
	CASE WHEN s.node_id != (SELECT node_id FROM sync_node WHERE id = 1)
		THEN CASE WHEN s.node_name != '' THEN s.node_name ELSE 'node ' || s.node_id END ELSE '' END, m.body, m.question, m.answered_at, m.created_at
	FROM task_messages m LEFT JOIN sessions s ON s.id = m.session_id`

func scanTaskMessage(row interface{ Scan(...any) error }) (TaskMessage, error) {
	var x TaskMessage
	var sess, answered sql.NullInt64
	var created int64
	if err := row.Scan(&x.ID, &x.TaskID, &sess, &x.SessionTitle, &x.SessionNode, &x.Body, &x.Question, &answered, &created); err != nil {
		if err == sql.ErrNoRows {
			err = ErrNotFound
		}
		return x, err
	}
	x.SessionID, x.AnsweredAt, x.CreatedAt = fromNullID(sess), fromNullMillis(answered), fromMillis(created)
	return x, nil
}

// AddTaskMessage adds a message to the end of its task's thread.
func (s *Store) AddTaskMessage(ctx context.Context, x TaskMessage) (TaskMessage, error) {
	id, err := s.insert(ctx, "task_messages", `INSERT INTO task_messages (id, task_id, session_id, body, question, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
		x.TaskID, x.SessionID, x.Body, boolInt(x.Question), s.ts())
	if err != nil {
		return x, err
	}
	return s.TaskMessage(ctx, id)
}

func (s *Store) TaskMessage(ctx context.Context, id int64) (TaskMessage, error) {
	return scanTaskMessage(s.db.QueryRowContext(ctx, `SELECT `+taskMessageCols+` WHERE m.id = ?`, id))
}

// TaskMessages lists the task's thread, oldest first.
func (s *Store) TaskMessages(ctx context.Context, taskID int64) ([]TaskMessage, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+taskMessageCols+` WHERE m.task_id = ? ORDER BY m.id`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []TaskMessage{}
	for rows.Next() {
		x, err := scanTaskMessage(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

// OpenQuestion is the task's latest question not answered yet; ErrNotFound if
// none waits.
func (s *Store) OpenQuestion(ctx context.Context, taskID int64) (TaskMessage, error) {
	return scanTaskMessage(s.db.QueryRowContext(ctx, `SELECT `+taskMessageCols+`
		WHERE m.task_id = ? AND m.question AND m.answered_at IS NULL ORDER BY m.id DESC LIMIT 1`, taskID))
}

// HasOpenQuestion reports whether the session asked a question on a task's
// thread that waits for an answer.
func (s *Store) HasOpenQuestion(ctx context.Context, sessionID int64) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM task_messages WHERE session_id = ? AND question AND answered_at IS NULL`,
		sessionID).Scan(&n)
	return n > 0, err
}

// SessionOpenQuestion is the latest question the session asked on a task's
// thread that waits for an answer; ErrNotFound if none does.
func (s *Store) SessionOpenQuestion(ctx context.Context, sessionID int64) (TaskMessage, error) {
	return scanTaskMessage(s.db.QueryRowContext(ctx, `SELECT `+taskMessageCols+`
		WHERE m.session_id = ? AND m.question AND m.answered_at IS NULL ORDER BY m.id DESC LIMIT 1`, sessionID))
}

// SetQuestionAnswered marks the question answered now, or open again.
func (s *Store) SetQuestionAnswered(ctx context.Context, id int64, answered bool) error {
	var at sql.NullInt64
	if answered {
		at = sql.NullInt64{Int64: s.ts(), Valid: true}
	}
	return affected(s.db.ExecContext(ctx, `UPDATE task_messages SET answered_at = ? WHERE id = ? AND question`, at, id))
}

// AnswerSessionQuestions marks every question the session asked answered: the
// user continued the session.
func (s *Store) AnswerSessionQuestions(ctx context.Context, sessionID int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE task_messages SET answered_at = ? WHERE session_id = ? AND question AND answered_at IS NULL`,
		s.ts(), sessionID)
	return err
}
