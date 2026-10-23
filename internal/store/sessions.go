package store

import (
	"context"
	"database/sql"
	"encoding/json"
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
	ID              int64   `json:"id"`
	TaskID          int64   `json:"task_id"` // 0: standalone session
	TaskTitle       string  `json:"task_title,omitempty"`
	ProjectID       int64   `json:"project_id,omitempty"` // the task's project, or the one a standalone session started in
	Title           string  `json:"title"`
	Status          string  `json:"status"`
	Mode            string  `json:"mode"`
	Agent           string  `json:"agent"` // the CLI the session runs on, e.g. claude or gemini
	Model           string  `json:"model"` // empty: CLI default
	Workdir         string  `json:"workdir"`
	Trigger         string  `json:"trigger"`
	DoneLaneID      *int64  `json:"done_lane_id"` // move the task here when a turn finishes successfully
	ParentID        *int64  `json:"parent_id"`    // the session this one was handed off from
	ChildID         *int64  `json:"child_id"`     // the latest session this one was handed off to
	ClaudeSessionID string  `json:"claude_session_id"`
	CostUSD         float64 `json:"cost_usd"`
	Error           string  `json:"error"`
	LastMessage     string  `json:"last_message,omitempty"`
	// NeedsInput says why the agent is waiting for the user, e.g. it asked a
	// question; empty when it is not. Cleared when the user continues the session.
	NeedsInput string `json:"needs_input"`
	// Seen: the user has taken in how the last turn ended (dismissed it or
	// moved the task). A new turn starts unseen.
	Seen bool `json:"seen"`
	// ContextTokens is how many tokens the session's context held after the
	// agent's latest message, out of the model's ContextWindow; 0 when not known.
	ContextTokens int `json:"context_tokens"`
	ContextWindow int `json:"context_window"`
	// Summary of the session for agents that build on its task; empty until one
	// is written. It covers messages up to SummaryUpto; later ones make it stale.
	Summary      string     `json:"summary"`
	SummaryUpto  int64      `json:"summary_upto"`
	SummaryAt    *time.Time `json:"summary_at"`
	SummaryStale bool       `json:"summary_stale"`
	// Queued: messages sent while the agent worked, oldest first; each starts
	// the next turn once the one before ends.
	Queued    []QueuedMessage `json:"queued"`
	CreatedAt time.Time       `json:"created_at"`
	UpdatedAt time.Time       `json:"updated_at"`
}

type QueuedMessage struct {
	ID      int64  `json:"id"`
	Content string `json:"content"`
}

type Message struct {
	ID        int64     `json:"id"`
	SessionID int64     `json:"session_id"`
	Role      string    `json:"role"` // user | assistant | tool | system
	Kind      string    `json:"kind"` // text | context | tool_use | tool_result | result | error
	Content   string    `json:"content"`
	CreatedAt time.Time `json:"created_at"`
}

const sessionCols = `s.id, COALESCE(s.task_id, 0), COALESCE(t.title, ''), COALESCE(t.project_id, s.project_id, 0), s.title, s.status, s.mode, s.agent, s.model, s.workdir, s.trigger, s.done_lane_id, s.parent_id,
	(SELECT MAX(c.id) FROM sessions c WHERE c.parent_id = s.id), s.claude_session_id, s.cost_usd, s.error, s.created_at, s.updated_at,
	COALESCE((SELECT content FROM messages m WHERE m.session_id = s.id AND m.kind IN ('text', 'result', 'error') ORDER BY m.id DESC LIMIT 1), ''),
	s.summary, s.summary_upto, s.summary_at, COALESCE((SELECT MAX(m.id) FROM messages m WHERE m.session_id = s.id), 0), s.needs_input, s.seen,
	s.context_tokens, s.context_window,
	(SELECT json_group_array(json_object('id', q.id, 'content', q.content)) FROM
		(SELECT id, content FROM queued_messages WHERE session_id = s.id ORDER BY id) q)`

func scanSession(sc interface{ Scan(...any) error }) (Session, error) {
	var x Session
	var created, updated int64
	var doneLane, parent, child, summaryAt sql.NullInt64
	var lastID int64
	var queued string
	err := sc.Scan(&x.ID, &x.TaskID, &x.TaskTitle, &x.ProjectID, &x.Title, &x.Status, &x.Mode, &x.Agent, &x.Model, &x.Workdir, &x.Trigger,
		&doneLane, &parent, &child, &x.ClaudeSessionID, &x.CostUSD, &x.Error, &created, &updated, &x.LastMessage,
		&x.Summary, &x.SummaryUpto, &summaryAt, &lastID, &x.NeedsInput, &x.Seen,
		&x.ContextTokens, &x.ContextWindow, &queued)
	if err == nil {
		err = json.Unmarshal([]byte(queued), &x.Queued)
	}
	x.DoneLaneID, x.ParentID, x.ChildID = fromNullID(doneLane), fromNullID(parent), fromNullID(child)
	x.SummaryAt = fromNullMillis(summaryAt)
	x.SummaryStale = x.Summary != "" && lastID > x.SummaryUpto
	x.CreatedAt, x.UpdatedAt = fromMillis(created), fromMillis(updated)
	return x, err
}

func (s *Store) CreateSession(ctx context.Context, x Session) (Session, error) {
	now := s.ts()
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO sessions (task_id, project_id, title, status, mode, agent, model, workdir, trigger, done_lane_id, parent_id, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		sql.NullInt64{Int64: x.TaskID, Valid: x.TaskID != 0}, sql.NullInt64{Int64: x.ProjectID, Valid: x.TaskID == 0 && x.ProjectID != 0}, x.Title, StatusRunning, x.Mode, agentOrClaude(x.Agent), x.Model, x.Workdir, x.Trigger,
		x.DoneLaneID, x.ParentID, now, now)
	if err != nil {
		return Session{}, err
	}
	id, _ := res.LastInsertId()
	return s.Session(ctx, id)
}

func agentOrClaude(a string) string {
	if a == "" {
		return "claude"
	}
	return a
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

func (s *Store) SetSessionStatus(ctx context.Context, id int64, status, errMsg string) error {
	return affected(s.db.ExecContext(ctx,
		`UPDATE sessions SET status = ?, error = ?, seen = 0, updated_at = ? WHERE id = ?`, status, errMsg, s.ts(), id))
}

// SetSessionNeedsInput records why the session's agent waits for the user;
// an empty reason clears it.
func (s *Store) SetSessionNeedsInput(ctx context.Context, id int64, reason string) error {
	return affected(s.db.ExecContext(ctx,
		`UPDATE sessions SET needs_input = ?, updated_at = ? WHERE id = ?`, reason, s.ts(), id))
}

// NeedsInputByTask counts, per task, the sessions whose agent waits for the user.
func (s *Store) NeedsInputByTask(ctx context.Context) (map[int64]int, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT task_id, COUNT(*) FROM sessions
		WHERE needs_input != '' AND task_id IS NOT NULL GROUP BY task_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]int{}
	for rows.Next() {
		var task int64
		var n int
		if err := rows.Scan(&task, &n); err != nil {
			return nil, err
		}
		out[task] = n
	}
	return out, rows.Err()
}

// DismissSession clears the session's wait for the user and marks how its
// last turn ended as seen.
func (s *Store) DismissSession(ctx context.Context, id int64) error {
	return affected(s.db.ExecContext(ctx, `UPDATE sessions SET needs_input = '', seen = 1 WHERE id = ?`, id))
}

// SeeTaskSessions marks how the task's finished sessions ended as seen.
func (s *Store) SeeTaskSessions(ctx context.Context, taskID int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE sessions SET seen = 1 WHERE task_id = ? AND status != ?`, taskID, StatusRunning)
	return err
}

// Agent statuses of a task: what its agents are up to, i.e. what the user's
// next step is.
const (
	AgentRunning   = "running"   // an agent works on it
	AgentNeedsYou  = "needs_you" // an agent waits for the user
	AgentDone      = "done"      // the last turn ended well and the task is in its done lane
	AgentFinished  = "finished"  // the last turn ended; look at what it did
	AgentFailed    = "failed"    // the last turn failed or was interrupted
	AgentCancelled = "cancelled" // the last turn was stopped
)

// AgentStatus is a task's agent status and the session it comes from.
type AgentStatus struct {
	Status    string `json:"status"`
	SessionID int64  `json:"session_id"`
	Reason    string `json:"reason,omitempty"` // why it needs the user, or the error
}

// agentSession is what of a session goes into its task's agent status.
type agentSession struct {
	ID, TaskID, LaneID int64
	DoneLaneID         *int64
	Status, NeedsInput string
	Error              string
	Seen               bool
}

// AgentStatuses gives the agent status of every task that has one: running
// over needs you over how its latest session's last turn ended, which shows
// until it is seen.
func (s *Store) AgentStatuses(ctx context.Context) (map[int64]AgentStatus, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT s.id, s.task_id, t.lane_id, s.done_lane_id, s.status, s.needs_input, s.error, s.seen
		FROM sessions s JOIN tasks t ON t.id = s.task_id
		ORDER BY s.updated_at DESC, s.id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	byTask := map[int64][]agentSession{}
	for rows.Next() {
		var x agentSession
		var doneLane sql.NullInt64
		if err := rows.Scan(&x.ID, &x.TaskID, &x.LaneID, &doneLane, &x.Status, &x.NeedsInput, &x.Error, &x.Seen); err != nil {
			return nil, err
		}
		x.DoneLaneID = fromNullID(doneLane)
		byTask[x.TaskID] = append(byTask[x.TaskID], x)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := map[int64]AgentStatus{}
	for task, xs := range byTask {
		if st, ok := agentStatus(xs); ok {
			out[task] = st
		}
	}
	return out, nil
}

// agentStatus derives a task's agent status from its sessions, latest first.
func agentStatus(xs []agentSession) (AgentStatus, bool) {
	for _, x := range xs {
		if x.Status == StatusRunning {
			return AgentStatus{Status: AgentRunning, SessionID: x.ID}, true
		}
	}
	for _, x := range xs {
		if x.NeedsInput != "" {
			return AgentStatus{Status: AgentNeedsYou, SessionID: x.ID, Reason: x.NeedsInput}, true
		}
	}
	if len(xs) == 0 || xs[0].Seen {
		return AgentStatus{}, false
	}
	x := xs[0]
	st := AgentStatus{SessionID: x.ID}
	switch x.Status {
	case StatusIdle:
		st.Status = AgentFinished
		if x.DoneLaneID != nil && *x.DoneLaneID == x.LaneID {
			st.Status = AgentDone
		}
	case StatusCancelled:
		st.Status = AgentCancelled
	case StatusInterrupted:
		st.Status, st.Reason = AgentFailed, "interrupted by a restart"
	default:
		st.Status, st.Reason = AgentFailed, x.Error
	}
	return st, true
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

// SetSessionContext records how full the session's context is; a zero
// argument keeps that value. Like a summary, it is not activity in the session.
func (s *Store) SetSessionContext(ctx context.Context, id int64, tokens, window int) error {
	return affected(s.db.ExecContext(ctx,
		`UPDATE sessions SET context_tokens = COALESCE(NULLIF(?, 0), context_tokens),
			context_window = COALESCE(NULLIF(?, 0), context_window) WHERE id = ?`, tokens, window, id))
}

// SetSessionSummary stores the session's summary, which covers its messages
// up to upto, and adds what writing it cost. The session's activity time stays:
// a summary is not work in the session.
func (s *Store) SetSessionSummary(ctx context.Context, id int64, summary string, upto int64, costUSD float64) error {
	return affected(s.db.ExecContext(ctx,
		`UPDATE sessions SET summary = ?, summary_upto = ?, summary_at = ?, cost_usd = cost_usd + ? WHERE id = ?`,
		summary, upto, s.ts(), costUSD, id))
}

// AddBackgroundCost adds what work for the session outside its turns cost,
// e.g. updating a memory. Like a summary, it is not activity in the session.
func (s *Store) AddBackgroundCost(ctx context.Context, id int64, usd float64) error {
	return affected(s.db.ExecContext(ctx, `UPDATE sessions SET cost_usd = cost_usd + ? WHERE id = ?`, usd, id))
}

// LastMessageID is the id of the session's latest message, 0 if it has none.
func (s *Store) LastMessageID(ctx context.Context, sessionID int64) (int64, error) {
	var id int64
	err := s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(id), 0) FROM messages WHERE session_id = ?`, sessionID).Scan(&id)
	return id, err
}

// MarkInterrupted flags sessions left running by a previous process.
func (s *Store) MarkInterrupted(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `UPDATE sessions SET status = ?, seen = 0, updated_at = ? WHERE status = ?`,
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

// QueueMessage keeps a message for the session's next turn.
func (s *Store) QueueMessage(ctx context.Context, sessionID int64, content string) (QueuedMessage, error) {
	res, err := s.db.ExecContext(ctx, `INSERT INTO queued_messages (session_id, content, created_at) VALUES (?, ?, ?)`,
		sessionID, content, s.ts())
	if err != nil {
		return QueuedMessage{}, err
	}
	id, _ := res.LastInsertId()
	return QueuedMessage{ID: id, Content: content}, nil
}

// DeleteQueuedMessage drops a queued message before it is sent.
func (s *Store) DeleteQueuedMessage(ctx context.Context, id int64) error {
	return affected(s.db.ExecContext(ctx, `DELETE FROM queued_messages WHERE id = ?`, id))
}
