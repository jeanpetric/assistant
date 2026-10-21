package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// Rule acts when a task of ProjectID (any project when nil) moves into
// ToLaneID (from FromLaneID, or from any lane when FromLaneID is nil): it
// starts an agent session with Prompt, runs SkillID in the task's latest
// session, or, with neither, moves the task to DoneLaneID once an agent turn
// on it finishes.
type Rule struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	ProjectID  *int64 `json:"project_id"`
	FromLaneID *int64 `json:"from_lane_id"`
	ToLaneID   int64  `json:"to_lane_id"`
	Prompt     string `json:"prompt"`
	SkillID    *int64 `json:"skill_id"`
	Mode       string `json:"mode"`
	Workdir    string `json:"workdir"`
	DoneLaneID *int64 `json:"done_lane_id"` // copied to the sessions the rule starts
	Enabled    bool   `json:"enabled"`
}

const (
	ActionAgent  = "agent"
	ActionRemind = "remind"
)

type Schedule struct {
	ID         int64      `json:"id"`
	TaskID     int64      `json:"task_id"`
	Cron       string     `json:"cron"`
	Action     string     `json:"action"` // agent | remind
	Prompt     string     `json:"prompt"`
	Mode       string     `json:"mode"`
	DoneLaneID *int64     `json:"done_lane_id"` // copied to the sessions the schedule starts
	Enabled    bool       `json:"enabled"`
	NextRunAt  *time.Time `json:"next_run_at"`
	LastRunAt  *time.Time `json:"last_run_at"`
}

type Notification struct {
	ID        int64      `json:"id"`
	TaskID    *int64     `json:"task_id"`
	TaskTitle string     `json:"task_title"`
	SessionID *int64     `json:"session_id"` // the session it is about, if any
	Message   string     `json:"message"`
	CreatedAt time.Time  `json:"created_at"`
	ReadAt    *time.Time `json:"read_at"`
}

// ---- rules ----

const ruleCols = `id, name, project_id, from_lane_id, to_lane_id, prompt, skill_id, mode, workdir, done_lane_id, enabled`

func scanRule(sc interface{ Scan(...any) error }) (Rule, error) {
	var r Rule
	var project, from, skill, doneLane sql.NullInt64
	var enabled int
	err := sc.Scan(&r.ID, &r.Name, &project, &from, &r.ToLaneID, &r.Prompt, &skill, &r.Mode, &r.Workdir, &doneLane, &enabled)
	r.ProjectID, r.FromLaneID, r.SkillID, r.DoneLaneID = fromNullID(project), fromNullID(from), fromNullID(skill), fromNullID(doneLane)
	r.Enabled = enabled == 1
	return r, err
}

func (s *Store) queryRules(ctx context.Context, q string, args ...any) ([]Rule, error) {
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Rule{}
	for rows.Next() {
		r, err := scanRule(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) Rules(ctx context.Context) ([]Rule, error) {
	return s.queryRules(ctx, `SELECT `+ruleCols+` FROM transition_rules ORDER BY id`)
}

// MatchingRules returns enabled rules for a move from -> to of a task in the project.
// A project's rule overrides any-project rules for the same transition (same
// from and to lanes), so the move doesn't start both.
func (s *Store) MatchingRules(ctx context.Context, projectID, from, to int64) ([]Rule, error) {
	return s.queryRules(ctx, `SELECT `+ruleCols+` FROM transition_rules r
		WHERE enabled = 1 AND to_lane_id = ? AND (from_lane_id IS NULL OR from_lane_id = ?)
			AND (project_id = ? OR project_id IS NULL AND NOT EXISTS (SELECT 1 FROM transition_rules o
				WHERE o.enabled = 1 AND o.project_id = ? AND o.to_lane_id = r.to_lane_id AND o.from_lane_id IS r.from_lane_id))
		ORDER BY id`, to, from, projectID, projectID)
}

// DoneLaneInto returns the done lane of the first enabled rule into lane `to`
// that has one, from any lane, preferring the project's rules; nil if none.
func (s *Store) DoneLaneInto(ctx context.Context, projectID, to int64) (*int64, error) {
	var lane int64
	err := s.db.QueryRowContext(ctx, `SELECT done_lane_id FROM transition_rules
		WHERE enabled = 1 AND to_lane_id = ? AND done_lane_id IS NOT NULL AND (project_id = ? OR project_id IS NULL)
		ORDER BY project_id IS NULL, id LIMIT 1`, to, projectID).Scan(&lane)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &lane, nil
}

func (s *Store) Rule(ctx context.Context, id int64) (Rule, error) {
	r, err := scanRule(s.db.QueryRowContext(ctx, `SELECT `+ruleCols+` FROM transition_rules WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return r, ErrNotFound
	}
	return r, err
}

func (s *Store) SaveRule(ctx context.Context, r Rule) (Rule, error) {
	if r.ID == 0 {
		res, err := s.db.ExecContext(ctx, `INSERT INTO transition_rules (name, project_id, from_lane_id, to_lane_id, prompt, skill_id, mode, workdir,
			done_lane_id, enabled) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, r.Name, r.ProjectID, r.FromLaneID, r.ToLaneID, r.Prompt, r.SkillID,
			r.Mode, r.Workdir, r.DoneLaneID, boolInt(r.Enabled))
		if err != nil {
			return r, err
		}
		r.ID, _ = res.LastInsertId()
	} else {
		err := affected(s.db.ExecContext(ctx, `UPDATE transition_rules SET name = ?, project_id = ?, from_lane_id = ?, to_lane_id = ?,
			prompt = ?, skill_id = ?, mode = ?, workdir = ?, done_lane_id = ?, enabled = ? WHERE id = ?`,
			r.Name, r.ProjectID, r.FromLaneID, r.ToLaneID, r.Prompt, r.SkillID, r.Mode, r.Workdir, r.DoneLaneID, boolInt(r.Enabled), r.ID))
		if err != nil {
			return r, err
		}
	}
	return s.Rule(ctx, r.ID)
}

func (s *Store) DeleteRule(ctx context.Context, id int64) error {
	return affected(s.db.ExecContext(ctx, `DELETE FROM transition_rules WHERE id = ?`, id))
}

// ---- schedules ----

const scheduleCols = `id, task_id, cron, action, prompt, mode, done_lane_id, enabled, next_run_at, last_run_at`

func scanSchedule(sc interface{ Scan(...any) error }) (Schedule, error) {
	var x Schedule
	var enabled int
	var doneLane, next, last sql.NullInt64
	err := sc.Scan(&x.ID, &x.TaskID, &x.Cron, &x.Action, &x.Prompt, &x.Mode, &doneLane, &enabled, &next, &last)
	x.DoneLaneID = fromNullID(doneLane)
	x.Enabled = enabled == 1
	x.NextRunAt, x.LastRunAt = fromNullMillis(next), fromNullMillis(last)
	return x, err
}

func (s *Store) querySchedules(ctx context.Context, q string, args ...any) ([]Schedule, error) {
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Schedule{}
	for rows.Next() {
		x, err := scanSchedule(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

func (s *Store) Schedules(ctx context.Context, taskID int64) ([]Schedule, error) {
	return s.querySchedules(ctx, `SELECT `+scheduleCols+` FROM schedules WHERE task_id = ? ORDER BY id`, taskID)
}

// DueSchedules returns enabled schedules whose next run is at or before now.
func (s *Store) DueSchedules(ctx context.Context, now time.Time) ([]Schedule, error) {
	return s.querySchedules(ctx, `SELECT `+scheduleCols+` FROM schedules
		WHERE enabled = 1 AND next_run_at IS NOT NULL AND next_run_at <= ? ORDER BY next_run_at`, now.UnixMilli())
}

func (s *Store) Schedule(ctx context.Context, id int64) (Schedule, error) {
	x, err := scanSchedule(s.db.QueryRowContext(ctx, `SELECT `+scheduleCols+` FROM schedules WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return x, ErrNotFound
	}
	return x, err
}

func (s *Store) SaveSchedule(ctx context.Context, x Schedule) (Schedule, error) {
	if x.ID == 0 {
		res, err := s.db.ExecContext(ctx, `INSERT INTO schedules (task_id, cron, action, prompt, mode, done_lane_id, enabled, next_run_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, x.TaskID, x.Cron, x.Action, x.Prompt, x.Mode, x.DoneLaneID, boolInt(x.Enabled), toNullMillis(x.NextRunAt))
		if err != nil {
			return x, err
		}
		x.ID, _ = res.LastInsertId()
	} else {
		err := affected(s.db.ExecContext(ctx, `UPDATE schedules SET cron = ?, action = ?, prompt = ?, mode = ?, done_lane_id = ?, enabled = ?,
			next_run_at = ? WHERE id = ?`, x.Cron, x.Action, x.Prompt, x.Mode, x.DoneLaneID, boolInt(x.Enabled), toNullMillis(x.NextRunAt), x.ID))
		if err != nil {
			return x, err
		}
	}
	return s.Schedule(ctx, x.ID)
}

func (s *Store) MarkScheduleRun(ctx context.Context, id int64, ranAt time.Time, next *time.Time) error {
	return affected(s.db.ExecContext(ctx, `UPDATE schedules SET last_run_at = ?, next_run_at = ? WHERE id = ?`,
		ranAt.UnixMilli(), toNullMillis(next), id))
}

func (s *Store) DeleteSchedule(ctx context.Context, id int64) error {
	return affected(s.db.ExecContext(ctx, `DELETE FROM schedules WHERE id = ?`, id))
}

// ---- notifications ----

func (s *Store) Notify(ctx context.Context, taskID *int64, msg string) error {
	return s.NotifySession(ctx, taskID, nil, msg)
}

// NotifySession is Notify about one of the task's sessions.
func (s *Store) NotifySession(ctx context.Context, taskID, sessionID *int64, msg string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO notifications (task_id, session_id, message, created_at) VALUES (?, ?, ?, ?)`,
		taskID, sessionID, msg, s.ts())
	return err
}

func (s *Store) Notifications(ctx context.Context, unreadOnly bool) ([]Notification, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT n.id, n.task_id, COALESCE(t.title, ''), n.session_id, n.message, n.created_at, n.read_at
		FROM notifications n LEFT JOIN tasks t ON t.id = n.task_id
		WHERE n.archived_at IS NULL AND (? = 0 OR n.read_at IS NULL) ORDER BY n.id DESC LIMIT 200`, boolInt(unreadOnly))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Notification{}
	for rows.Next() {
		var n Notification
		var task, sess, read sql.NullInt64
		var created int64
		if err := rows.Scan(&n.ID, &task, &n.TaskTitle, &sess, &n.Message, &created, &read); err != nil {
			return nil, err
		}
		if task.Valid {
			n.TaskID = &task.Int64
		}
		n.SessionID = fromNullID(sess)
		n.CreatedAt, n.ReadAt = fromMillis(created), fromNullMillis(read)
		out = append(out, n)
	}
	return out, rows.Err()
}

// ArchiveNotifications takes every notification off the panel, marking the unread ones read.
func (s *Store) ArchiveNotifications(ctx context.Context) error {
	now := s.ts()
	_, err := s.db.ExecContext(ctx, `UPDATE notifications SET archived_at = ?, read_at = COALESCE(read_at, ?) WHERE archived_at IS NULL`, now, now)
	return err
}

func (s *Store) MarkNotificationRead(ctx context.Context, id int64) error {
	return affected(s.db.ExecContext(ctx, `UPDATE notifications SET read_at = ? WHERE id = ?`, s.ts(), id))
}
