package store

import (
	"context"
	"database/sql"
	"errors"
)

// Project is a board of its own: it shows only its tasks, on the shared lanes.
type Project struct {
	ID       int64  `json:"id"`
	Name     string `json:"name"`
	Color    string `json:"color"`
	Position int    `json:"position"`
	Workdir  string `json:"workdir"` // agents on its tasks run here unless the task or session says otherwise
}

var (
	ErrProjectNotEmpty = errors.New("project still has tasks")
	ErrLastProject     = errors.New("the last project can't be deleted")
)

func (s *Store) Projects(ctx context.Context) ([]Project, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, name, color, position, workdir FROM projects ORDER BY position, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Project{}
	for rows.Next() {
		var p Project
		if err := rows.Scan(&p.ID, &p.Name, &p.Color, &p.Position, &p.Workdir); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *Store) Project(ctx context.Context, id int64) (Project, error) {
	var p Project
	err := s.db.QueryRowContext(ctx, `SELECT id, name, color, position, workdir FROM projects WHERE id = ?`, id).
		Scan(&p.ID, &p.Name, &p.Color, &p.Position, &p.Workdir)
	if errors.Is(err, sql.ErrNoRows) {
		return p, ErrNotFound
	}
	return p, err
}

func (s *Store) CreateProject(ctx context.Context, p Project) (Project, error) {
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO projects (name, color, workdir, position) VALUES (?, ?, ?, (SELECT COALESCE(MAX(position), -1) + 1 FROM projects))`,
		p.Name, p.Color, p.Workdir)
	if err != nil {
		return Project{}, err
	}
	id, _ := res.LastInsertId()
	return s.Project(ctx, id)
}

// UpdateProject saves name, colour and workdir; the position stays.
func (s *Store) UpdateProject(ctx context.Context, p Project) error {
	return affected(s.db.ExecContext(ctx, `UPDATE projects SET name = ?, color = ?, workdir = ? WHERE id = ?`, p.Name, p.Color, p.Workdir, p.ID))
}

// DeleteProject deletes a project without tasks, archived ones included,
// as long as another project remains.
func (s *Store) DeleteProject(ctx context.Context, id int64) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		var tasks, others int
		err := tx.QueryRowContext(ctx, `SELECT (SELECT COUNT(*) FROM tasks WHERE project_id = ?),
			(SELECT COUNT(*) FROM projects WHERE id != ?)`, id, id).Scan(&tasks, &others)
		if err != nil {
			return err
		}
		if tasks > 0 {
			return ErrProjectNotEmpty
		}
		if others == 0 {
			return ErrLastProject
		}
		return affected(tx.ExecContext(ctx, `DELETE FROM projects WHERE id = ?`, id))
	})
}

// ReorderProjects sets project positions to the order of ids.
func (s *Store) ReorderProjects(ctx context.Context, ids []int64) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		for i, id := range ids {
			if _, err := tx.ExecContext(ctx, `UPDATE projects SET position = ? WHERE id = ?`, i, id); err != nil {
				return err
			}
		}
		return nil
	})
}

// MoveTaskToProject puts the task at the end of its lane in another project.
// Archived tasks stay archived and take their place when restored.
func (s *Store) MoveTaskToProject(ctx context.Context, id, projectID int64) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		var from, lane int64
		var archived bool
		err := tx.QueryRowContext(ctx, `SELECT project_id, lane_id, archived_at IS NOT NULL FROM tasks WHERE id = ?`, id).
			Scan(&from, &lane, &archived)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil || from == projectID {
			return err
		}
		var exists int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM projects WHERE id = ?`, projectID).Scan(&exists); err != nil {
			return err
		}
		if exists == 0 {
			return ErrNotFound
		}
		if _, err := tx.ExecContext(ctx, `UPDATE tasks SET project_id = ?, position = `+nextPosition+`, updated_at = ? WHERE id = ?`,
			projectID, projectID, lane, s.ts(), id); err != nil {
			return err
		}
		if archived {
			return nil
		}
		ids, err := laneTaskIDs(ctx, tx, from, lane, id)
		if err != nil {
			return err
		}
		return renumber(ctx, tx, ids)
	})
}
