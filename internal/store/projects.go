package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// Project is a board of its own: it shows only its tasks, on the shared lanes.
type Project struct {
	ID       int64  `json:"id"`
	Name     string `json:"name"`
	Color    string `json:"color"`
	Position int    `json:"position"`
	Workdir  string `json:"workdir"` // agents on its tasks run here unless the task or session says otherwise
	// The workdir it was created with, shared by every computer; Workdir is
	// this computer's own if it has one (see local_paths.go).
	WorkdirDefault string `json:"workdir_default"`
	Watched        int    `json:"watched"` // its watched tasks on the board
	// Lanes its board leaves out (in lane order), unless tasks of the project are in them.
	HiddenLanes []int64 `json:"hidden_lanes"`
}

var projectCols = `id, name, color, position, ` + localWorkdir("project", "projects") + `,
	(SELECT COUNT(*) FROM tasks WHERE tasks.project_id = projects.id AND tasks.watched AND tasks.archived_at IS NULL)`

var (
	ErrProjectNotEmpty = errors.New("project still has tasks")
	ErrLastProject     = errors.New("the last project can't be deleted")
	ErrUnknownLane     = errors.New("unknown lane")
	ErrNoLaneShown     = errors.New("a project must show at least one lane")
)

func (s *Store) Projects(ctx context.Context) ([]Project, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+projectCols+` FROM projects ORDER BY position, id`)
	if err != nil {
		return nil, err
	}
	out := []Project{}
	for rows.Next() {
		var p Project
		if err := rows.Scan(&p.ID, &p.Name, &p.Color, &p.Position, &p.Workdir, &p.WorkdirDefault, &p.Watched); err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	hidden, err := s.hiddenLanes(ctx, 0)
	if err != nil {
		return nil, err
	}
	for i := range out {
		out[i].HiddenLanes = append([]int64{}, hidden[out[i].ID]...)
	}
	return out, nil
}

func (s *Store) Project(ctx context.Context, id int64) (Project, error) {
	var p Project
	err := s.db.QueryRowContext(ctx, `SELECT `+projectCols+` FROM projects WHERE id = ?`, id).
		Scan(&p.ID, &p.Name, &p.Color, &p.Position, &p.Workdir, &p.WorkdirDefault, &p.Watched)
	if errors.Is(err, sql.ErrNoRows) {
		return p, ErrNotFound
	}
	if err != nil {
		return p, err
	}
	hidden, err := s.hiddenLanes(ctx, id)
	p.HiddenLanes = append([]int64{}, hidden[id]...)
	return p, err
}

// hiddenLanes returns the hidden lanes of a project (0: of every project) by
// project, each in lane order.
func (s *Store) hiddenLanes(ctx context.Context, projectID int64) (map[int64][]int64, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT h.project_id, h.lane_id FROM project_hidden_lanes h JOIN lanes l ON l.id = h.lane_id
		WHERE ? IN (0, h.project_id) ORDER BY h.project_id, l.position, l.id`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64][]int64{}
	for rows.Next() {
		var p, l int64
		if err := rows.Scan(&p, &l); err != nil {
			return nil, err
		}
		out[p] = append(out[p], l)
	}
	return out, rows.Err()
}

// CreateProject adds the project with its workdir both as the default and as
// this computer's own.
func (s *Store) CreateProject(ctx context.Context, p Project) (Project, error) {
	var id int64
	err := s.tx(ctx, func(tx *sql.Tx) error {
		var err error
		id, err = s.insertTx(ctx, tx, "projects",
			`INSERT INTO projects (id, name, color, workdir, position) VALUES (?, ?, ?, ?, (SELECT COALESCE(MAX(position), -1) + 1 FROM projects))`,
			p.Name, p.Color, p.Workdir)
		if err != nil {
			return err
		}
		return putLocalWorkdir(ctx, tx, "project", id, p.Workdir)
	})
	if err != nil {
		return Project{}, err
	}
	return s.Project(ctx, id)
}

// UpdateProject saves name, colour, workdir (this computer's, see
// setLocalWorkdir) and hidden lanes; the position stays. Every hidden lane
// must exist and at least one lane stay shown.
func (s *Store) UpdateProject(ctx context.Context, p Project) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		if err := affected(tx.ExecContext(ctx, `UPDATE projects SET name = ?, color = ? WHERE id = ?`, p.Name, p.Color, p.ID)); err != nil {
			return err
		}
		if err := setLocalWorkdir(ctx, tx, "project", "projects", p.ID, p.Workdir); err != nil {
			return err
		}
		return setHiddenLanes(ctx, tx, p.ID, p.HiddenLanes)
	})
}

// setHiddenLanes makes lanes the project's hidden lanes, touching only the
// rows that change.
func setHiddenLanes(ctx context.Context, tx *sql.Tx, projectID int64, lanes []int64) error {
	hide := map[int64]bool{}
	for _, id := range lanes {
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM lanes WHERE id = ?`, id).Scan(&n); err != nil {
			return err
		}
		if n == 0 {
			return fmt.Errorf("%w %d", ErrUnknownLane, id)
		}
		hide[id] = true
	}
	var total int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM lanes`).Scan(&total); err != nil {
		return err
	}
	if len(hide) >= total {
		return ErrNoLaneShown
	}
	rows, err := tx.QueryContext(ctx, `SELECT lane_id FROM project_hidden_lanes WHERE project_id = ?`, projectID)
	if err != nil {
		return err
	}
	var drop []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		if hide[id] {
			delete(hide, id) // already hidden
		} else {
			drop = append(drop, id)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, id := range drop {
		if _, err := tx.ExecContext(ctx, `DELETE FROM project_hidden_lanes WHERE project_id = ? AND lane_id = ?`, projectID, id); err != nil {
			return err
		}
	}
	for id := range hide {
		if _, err := tx.ExecContext(ctx, `INSERT INTO project_hidden_lanes (project_id, lane_id) VALUES (?, ?)`, projectID, id); err != nil {
			return err
		}
	}
	return nil
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
