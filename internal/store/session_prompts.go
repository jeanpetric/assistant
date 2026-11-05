package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
)

// SessionPrompt is a template appended to the system prompt of every agent
// turn of a session in one of its projects (see app.SessionPromptData).
type SessionPrompt struct {
	ID     int64  `json:"id"`
	Name   string `json:"name"`
	Prompt string `json:"prompt"`
	// AllProjects applies the prompt to every project, ProjectIDs aside;
	// otherwise it applies to the projects in ProjectIDs only.
	AllProjects bool    `json:"all_projects"`
	ProjectIDs  []int64 `json:"project_ids"`
}

// AppliesTo reports whether the prompt is for sessions in project.
func (x SessionPrompt) AppliesTo(project int64) bool {
	return x.AllProjects || slices.Contains(x.ProjectIDs, project)
}

const sessionPromptCols = `id, name, prompt, all_projects, project_ids`

func scanSessionPrompt(row interface{ Scan(...any) error }) (SessionPrompt, error) {
	var x SessionPrompt
	var ids string
	if err := row.Scan(&x.ID, &x.Name, &x.Prompt, &x.AllProjects, &ids); err != nil {
		return x, err
	}
	x.ProjectIDs = []int64{}
	return x, json.Unmarshal([]byte(ids), &x.ProjectIDs)
}

// SessionPrompts lists the session prompts in the order they are sent: by id.
// Projects deleted since a prompt was saved are left out of its ProjectIDs.
func (s *Store) SessionPrompts(ctx context.Context) ([]SessionPrompt, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+sessionPromptCols+` FROM session_prompts ORDER BY id`)
	if err != nil {
		return nil, err
	}
	out := []SessionPrompt{}
	for rows.Next() {
		x, err := scanSessionPrompt(rows)
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
	return out, s.dropGoneProjects(ctx, out)
}

func (s *Store) SessionPrompt(ctx context.Context, id int64) (SessionPrompt, error) {
	x, err := scanSessionPrompt(s.db.QueryRowContext(ctx, `SELECT `+sessionPromptCols+` FROM session_prompts WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return x, ErrNotFound
	}
	if err != nil {
		return x, err
	}
	xs := []SessionPrompt{x}
	err = s.dropGoneProjects(ctx, xs)
	return xs[0], err
}

func (s *Store) dropGoneProjects(ctx context.Context, xs []SessionPrompt) error {
	ps, err := s.Projects(ctx)
	if err != nil {
		return err
	}
	known := map[int64]bool{}
	for _, p := range ps {
		known[p.ID] = true
	}
	for i := range xs {
		xs[i].ProjectIDs = slices.DeleteFunc(xs[i].ProjectIDs, func(id int64) bool { return !known[id] })
	}
	return nil
}

// SaveSessionPrompt creates (ID 0) or updates a session prompt.
func (s *Store) SaveSessionPrompt(ctx context.Context, x SessionPrompt) (SessionPrompt, error) {
	if x.ProjectIDs == nil {
		x.ProjectIDs = []int64{}
	}
	ids, _ := json.Marshal(x.ProjectIDs)
	err := s.tx(ctx, func(tx *sql.Tx) error {
		now := s.ts()
		if x.ID == 0 {
			id, err := s.insertTx(ctx, tx, "session_prompts", `INSERT INTO session_prompts (id, name, prompt, all_projects, project_ids, created_at, updated_at)
				VALUES (?, ?, ?, ?, ?, ?, ?)`, x.Name, x.Prompt, boolInt(x.AllProjects), string(ids), now, now)
			x.ID = id
			return err
		}
		return affected(tx.ExecContext(ctx, `UPDATE session_prompts SET name = ?, prompt = ?, all_projects = ?, project_ids = ?, updated_at = ?
			WHERE id = ?`, x.Name, x.Prompt, boolInt(x.AllProjects), string(ids), now, x.ID))
	})
	if err != nil {
		return x, err
	}
	return s.SessionPrompt(ctx, x.ID)
}

func (s *Store) DeleteSessionPrompt(ctx context.Context, id int64) error {
	return affected(s.db.ExecContext(ctx, `DELETE FROM session_prompts WHERE id = ?`, id))
}
