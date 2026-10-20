package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

// Skill is a set of instructions an agent applies to the work of its session:
// by hand, or after a successful turn when one of its automations matches.
type Skill struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Body        string `json:"body"` // prompt template, like a rule's
	// Artifact keeps the agent's reply to the skill on the task.
	Artifact bool `json:"artifact"`
	// Model runs the skill's turn with this model; empty keeps the session's.
	Model       string            `json:"model"`
	Automations []SkillAutomation `json:"automations"`
	CreatedAt   time.Time         `json:"created_at"`
	UpdatedAt   time.Time         `json:"updated_at"`
}

// SkillAutomation runs its skill after an agent turn on a task finishes
// successfully, if the task matches: an empty kind and nil project or lane
// match any. The lane is the one the task is in when the turn ends.
type SkillAutomation struct {
	ID        int64  `json:"id"`
	Kind      string `json:"kind"`
	ProjectID *int64 `json:"project_id"`
	LaneID    *int64 `json:"lane_id"`
	Enabled   bool   `json:"enabled"`
}

// Artifact is what an agent replied to a skill, kept on the task.
type Artifact struct {
	ID        int64     `json:"id"`
	TaskID    int64     `json:"task_id"`
	SessionID *int64    `json:"session_id"`
	Skill     string    `json:"skill"` // the skill's name at the time
	Content   string    `json:"content"`
	CreatedAt time.Time `json:"created_at"`
}

var ErrSkillExists = errors.New("a skill with this name already exists")

// ---- skills ----

const skillCols = `id, name, description, body, artifact, model, created_at, updated_at`

func scanSkill(sc interface{ Scan(...any) error }) (Skill, error) {
	var x Skill
	var artifact int
	var created, updated int64
	err := sc.Scan(&x.ID, &x.Name, &x.Description, &x.Body, &artifact, &x.Model, &created, &updated)
	x.Artifact = artifact == 1
	x.CreatedAt, x.UpdatedAt = fromMillis(created), fromMillis(updated)
	return x, err
}

// Skills lists the skills by name, each with its automations.
func (s *Store) Skills(ctx context.Context) ([]Skill, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+skillCols+` FROM skills ORDER BY name, id`)
	if err != nil {
		return nil, err
	}
	out := []Skill{}
	for rows.Next() {
		x, err := scanSkill(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, x)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// Automations are read after the rows are closed: the DB has one connection.
	for i := range out {
		if out[i].Automations, err = s.skillAutomations(ctx, out[i].ID); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (s *Store) Skill(ctx context.Context, id int64) (Skill, error) {
	x, err := scanSkill(s.db.QueryRowContext(ctx, `SELECT `+skillCols+` FROM skills WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return x, ErrNotFound
	}
	if err != nil {
		return x, err
	}
	x.Automations, err = s.skillAutomations(ctx, id)
	return x, err
}

func (s *Store) skillAutomations(ctx context.Context, skillID int64) ([]SkillAutomation, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, kind, project_id, lane_id, enabled FROM skill_automations
		WHERE skill_id = ? ORDER BY id`, skillID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []SkillAutomation{}
	for rows.Next() {
		var x SkillAutomation
		var project, lane sql.NullInt64
		var enabled int
		if err := rows.Scan(&x.ID, &x.Kind, &project, &lane, &enabled); err != nil {
			return nil, err
		}
		x.ProjectID, x.LaneID, x.Enabled = fromNullID(project), fromNullID(lane), enabled == 1
		out = append(out, x)
	}
	return out, rows.Err()
}

// SaveSkill creates (ID 0) or updates a skill; its automations replace the saved ones.
func (s *Store) SaveSkill(ctx context.Context, x Skill) (Skill, error) {
	err := s.tx(ctx, func(tx *sql.Tx) error {
		now := s.ts()
		if x.ID == 0 {
			res, err := tx.ExecContext(ctx, `INSERT INTO skills (name, description, body, artifact, model, created_at, updated_at)
				VALUES (?, ?, ?, ?, ?, ?, ?)`, x.Name, x.Description, x.Body, boolInt(x.Artifact), x.Model, now, now)
			if err != nil {
				return err
			}
			x.ID, _ = res.LastInsertId()
		} else {
			err := affected(tx.ExecContext(ctx, `UPDATE skills SET name = ?, description = ?, body = ?, artifact = ?, model = ?, updated_at = ?
				WHERE id = ?`, x.Name, x.Description, x.Body, boolInt(x.Artifact), x.Model, now, x.ID))
			if err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `DELETE FROM skill_automations WHERE skill_id = ?`, x.ID); err != nil {
				return err
			}
		}
		for _, a := range x.Automations {
			_, err := tx.ExecContext(ctx, `INSERT INTO skill_automations (skill_id, kind, project_id, lane_id, enabled) VALUES (?, ?, ?, ?, ?)`,
				x.ID, a.Kind, a.ProjectID, a.LaneID, boolInt(a.Enabled))
			if err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed: skills.name") {
			err = ErrSkillExists
		}
		return x, err
	}
	return s.Skill(ctx, x.ID)
}

func (s *Store) DeleteSkill(ctx context.Context, id int64) error {
	return affected(s.db.ExecContext(ctx, `DELETE FROM skills WHERE id = ?`, id))
}

// MatchingSkills returns the skills with an enabled automation matching a
// task of this kind and project in this lane, each once, in automation order.
func (s *Store) MatchingSkills(ctx context.Context, kind string, projectID, laneID int64) ([]Skill, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT s.id FROM skills s JOIN skill_automations a ON a.skill_id = s.id
		WHERE a.enabled = 1 AND (a.kind = '' OR a.kind = ?) AND (a.project_id IS NULL OR a.project_id = ?)
			AND (a.lane_id IS NULL OR a.lane_id = ?)
		GROUP BY s.id ORDER BY MIN(a.id)`, kind, projectID, laneID)
	if err != nil {
		return nil, err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := []Skill{}
	for _, id := range ids {
		x, err := s.Skill(ctx, id)
		if err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, nil
}

// ---- artifacts ----

func (s *Store) AddArtifact(ctx context.Context, x Artifact) (Artifact, error) {
	now := s.ts()
	res, err := s.db.ExecContext(ctx, `INSERT INTO artifacts (task_id, session_id, skill, content, created_at) VALUES (?, ?, ?, ?, ?)`,
		x.TaskID, x.SessionID, x.Skill, x.Content, now)
	if err != nil {
		return x, err
	}
	x.ID, _ = res.LastInsertId()
	x.CreatedAt = fromMillis(now)
	return x, nil
}

// Artifacts lists the task's artifacts, newest first.
func (s *Store) Artifacts(ctx context.Context, taskID int64) ([]Artifact, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, task_id, session_id, skill, content, created_at FROM artifacts
		WHERE task_id = ? ORDER BY id DESC`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Artifact{}
	for rows.Next() {
		var x Artifact
		var sess sql.NullInt64
		var created int64
		if err := rows.Scan(&x.ID, &x.TaskID, &sess, &x.Skill, &x.Content, &created); err != nil {
			return nil, err
		}
		x.SessionID, x.CreatedAt = fromNullID(sess), fromMillis(created)
		out = append(out, x)
	}
	return out, rows.Err()
}

func (s *Store) DeleteArtifact(ctx context.Context, id int64) error {
	return affected(s.db.ExecContext(ctx, `DELETE FROM artifacts WHERE id = ?`, id))
}
