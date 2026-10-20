package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

const (
	StatusRunning     = "running"
	StatusIdle        = "idle" // finished a turn, can be continued
	StatusError       = "error"
	StatusCancelled   = "cancelled"
	StatusInterrupted = "interrupted" // app restarted while running
)

type Session struct {
	ID              int64     `json:"id"`
	TaskID          int64     `json:"task_id"` // 0: standalone session
	TaskTitle       string    `json:"task_title,omitempty"`
	Title           string    `json:"title"`
	Status          string    `json:"status"`
	Mode            string    `json:"mode"`
	Model           string    `json:"model"` // empty: CLI default
	Workdir         string    `json:"workdir"`
	Trigger         string    `json:"trigger"`
	DoneLaneID      *int64    `json:"done_lane_id"` // move the task here when a turn finishes successfully
	ClaudeSessionID string    `json:"claude_session_id"`
	CostUSD         float64   `json:"cost_usd"`
	Error           string    `json:"error"`
	LastMessage     string    `json:"last_message,omitempty"`
	Limits          Limits    `json:"limits"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
}

// Limits is the account's Claude usage as last reported by the session's
// turns. A nil window has not been reported.
type Limits struct {
	FiveHour *LimitWindow `json:"five_hour"`
	SevenDay *LimitWindow `json:"seven_day"`
}

type LimitWindow struct {
	UsedPercent float64   `json:"used_percent"` // 0–100
	ResetsAt    time.Time `json:"resets_at"`
}

type Message struct {
	ID        int64     `json:"id"`
	SessionID int64     `json:"session_id"`
	Role      string    `json:"role"` // user | assistant | system
	Kind      string    `json:"kind"` // text | tool_use | tool_result | result | error
	Content   string    `json:"content"`
	CreatedAt time.Time `json:"created_at"`
}

const sessionCols = `s.id, COALESCE(s.task_id, 0), COALESCE(t.title, ''), s.title, s.status, s.mode, s.model, s.workdir, s.trigger, s.done_lane_id, s.claude_session_id,
	s.cost_usd, s.error, s.created_at, s.updated_at,
	s.limit_5h_used, s.limit_5h_resets_at, s.limit_7d_used, s.limit_7d_resets_at,
	COALESCE((SELECT content FROM messages m WHERE m.session_id = s.id AND m.kind IN ('text', 'result', 'error') ORDER BY m.id DESC LIMIT 1), '')`

func scanSession(sc interface{ Scan(...any) error }) (Session, error) {
	var x Session
	var created, updated int64
	var doneLane, reset5h, reset7d sql.NullInt64
	var used5h, used7d sql.NullFloat64
	err := sc.Scan(&x.ID, &x.TaskID, &x.TaskTitle, &x.Title, &x.Status, &x.Mode, &x.Model, &x.Workdir, &x.Trigger,
		&doneLane, &x.ClaudeSessionID, &x.CostUSD, &x.Error, &created, &updated,
		&used5h, &reset5h, &used7d, &reset7d, &x.LastMessage)
	x.DoneLaneID = fromNullID(doneLane)
	x.Limits = Limits{FiveHour: limitWindow(used5h, reset5h), SevenDay: limitWindow(used7d, reset7d)}
	x.CreatedAt, x.UpdatedAt = fromMillis(created), fromMillis(updated)
	return x, err
}

func limitWindow(used sql.NullFloat64, resets sql.NullInt64) *LimitWindow {
	if !used.Valid || !resets.Valid {
		return nil
	}
	return &LimitWindow{UsedPercent: used.Float64, ResetsAt: fromMillis(resets.Int64)}
}

func (s *Store) CreateSession(ctx context.Context, x Session) (Session, error) {
	now := s.ts()
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO sessions (task_id, title, status, mode, model, workdir, trigger, done_lane_id, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		sql.NullInt64{Int64: x.TaskID, Valid: x.TaskID != 0}, x.Title, StatusRunning, x.Mode, x.Model, x.Workdir, x.Trigger,
		x.DoneLaneID, now, now)
	if err != nil {
		return Session{}, err
	}
	id, _ := res.LastInsertId()
	return s.Session(ctx, id)
}

func (s *Store) Session(ctx context.Context, id int64) (Session, error) {
	x, err := scanSession(s.db.QueryRowContext(ctx,
		`SELECT `+sessionCols+` FROM sessions s LEFT JOIN tasks t ON t.id = s.task_id WHERE s.id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return x, ErrNotFound
	}
	return x, err
}

// Sessions lists sessions, newest activity first. taskID 0 means all tasks;
// an empty status means any status.
func (s *Store) Sessions(ctx context.Context, taskID int64, status string) ([]Session, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+sessionCols+` FROM sessions s LEFT JOIN tasks t ON t.id = s.task_id
		WHERE (? = 0 OR s.task_id = ?) AND (? = '' OR s.status = ?)
		ORDER BY s.updated_at DESC, s.id DESC`, taskID, taskID, status, status)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Session{}
	for rows.Next() {
		x, err := scanSession(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

// RecentWorkdirs returns distinct working directories of sessions and tasks,
// most recently used first.
func (s *Store) RecentWorkdirs(ctx context.Context, limit int) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT workdir FROM (
			SELECT workdir, updated_at FROM sessions UNION ALL SELECT workdir, updated_at FROM tasks
		) WHERE workdir != '' GROUP BY workdir ORDER BY MAX(updated_at) DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var d string
		if err := rows.Scan(&d); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

func (s *Store) SetSessionStatus(ctx context.Context, id int64, status, errMsg string) error {
	return affected(s.db.ExecContext(ctx,
		`UPDATE sessions SET status = ?, error = ?, updated_at = ? WHERE id = ?`, status, errMsg, s.ts(), id))
}

// SetSessionDoneLane changes where the session's task moves when a turn
// finishes; nil keeps the task where it is.
func (s *Store) SetSessionDoneLane(ctx context.Context, id int64, laneID *int64) error {
	return affected(s.db.ExecContext(ctx,
		`UPDATE sessions SET done_lane_id = ?, updated_at = ? WHERE id = ?`, laneID, s.ts(), id))
}

func (s *Store) SetClaudeSessionID(ctx context.Context, id int64, claudeID string) error {
	return affected(s.db.ExecContext(ctx,
		`UPDATE sessions SET claude_session_id = ?, updated_at = ? WHERE id = ?`, claudeID, s.ts(), id))
}

func (s *Store) AddSessionCost(ctx context.Context, id int64, usd float64) error {
	return affected(s.db.ExecContext(ctx,
		`UPDATE sessions SET cost_usd = cost_usd + ?, updated_at = ? WHERE id = ?`, usd, s.ts(), id))
}

// SetSessionLimits records the usage windows a turn reported. A nil window
// keeps the session's previous value for it.
func (s *Store) SetSessionLimits(ctx context.Context, id int64, l Limits) error {
	used := func(w *LimitWindow) any {
		if w == nil {
			return nil
		}
		return w.UsedPercent
	}
	resets := func(w *LimitWindow) any {
		if w == nil {
			return nil
		}
		return w.ResetsAt.UnixMilli()
	}
	return affected(s.db.ExecContext(ctx, `
		UPDATE sessions SET
			limit_5h_used = COALESCE(?, limit_5h_used), limit_5h_resets_at = COALESCE(?, limit_5h_resets_at),
			limit_7d_used = COALESCE(?, limit_7d_used), limit_7d_resets_at = COALESCE(?, limit_7d_resets_at),
			updated_at = ?
		WHERE id = ?`,
		used(l.FiveHour), resets(l.FiveHour), used(l.SevenDay), resets(l.SevenDay), s.ts(), id))
}

// MarkInterrupted flags sessions left running by a previous process.
func (s *Store) MarkInterrupted(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `UPDATE sessions SET status = ?, updated_at = ? WHERE status = ?`,
		StatusInterrupted, s.ts(), StatusRunning)
	return err
}

func (s *Store) AddMessage(ctx context.Context, m Message) (Message, error) {
	now := s.ts()
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO messages (session_id, role, kind, content, created_at) VALUES (?, ?, ?, ?, ?)`,
		m.SessionID, m.Role, m.Kind, m.Content, now)
	if err != nil {
		return m, err
	}
	m.ID, _ = res.LastInsertId()
	m.CreatedAt = fromMillis(now)
	_, err = s.db.ExecContext(ctx, `UPDATE sessions SET updated_at = ? WHERE id = ?`, now, m.SessionID)
	return m, err
}

// Messages returns the session's messages with id > after, oldest first.
func (s *Store) Messages(ctx context.Context, sessionID, after int64) ([]Message, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, session_id, role, kind, content, created_at FROM messages WHERE session_id = ? AND id > ? ORDER BY id`,
		sessionID, after)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Message{}
	for rows.Next() {
		var m Message
		var created int64
		if err := rows.Scan(&m.ID, &m.SessionID, &m.Role, &m.Kind, &m.Content, &created); err != nil {
			return nil, err
		}
		m.CreatedAt = fromMillis(created)
		out = append(out, m)
	}
	return out, rows.Err()
}
