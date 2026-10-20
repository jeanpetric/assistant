package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

type Lane struct {
	ID       int64  `json:"id"`
	Name     string `json:"name"`
	Color    string `json:"color"`
	Position int    `json:"position"`
}

type Task struct {
	ID          int64             `json:"id"`
	Title       string            `json:"title"`
	Description string            `json:"description"`
	Kind        string            `json:"kind"`
	LaneID      int64             `json:"lane_id"`
	Position    int               `json:"position"`
	Workdir     string            `json:"workdir"`
	Fields      map[string]string `json:"fields"`
	DueAt       *time.Time        `json:"due_at"`      // notify when reached
	RemindedAt  *time.Time        `json:"reminded_at"` // set once the due notification fired
	CreatedAt   time.Time         `json:"created_at"`
	UpdatedAt   time.Time         `json:"updated_at"`
}

type FieldDef struct {
	Key      string `json:"key"`
	Label    string `json:"label"`
	Type     string `json:"type"` // text | url
	Position int    `json:"position"`
}

var ErrLaneNotEmpty = errors.New("lane still has tasks")

// ---- lanes ----

func (s *Store) Lanes(ctx context.Context) ([]Lane, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, name, color, position FROM lanes ORDER BY position, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Lane{}
	for rows.Next() {
		var l Lane
		if err := rows.Scan(&l.ID, &l.Name, &l.Color, &l.Position); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

func (s *Store) Lane(ctx context.Context, id int64) (Lane, error) {
	var l Lane
	err := s.db.QueryRowContext(ctx, `SELECT id, name, color, position FROM lanes WHERE id = ?`, id).
		Scan(&l.ID, &l.Name, &l.Color, &l.Position)
	if errors.Is(err, sql.ErrNoRows) {
		return l, ErrNotFound
	}
	return l, err
}

func (s *Store) CreateLane(ctx context.Context, name, color string) (Lane, error) {
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO lanes (name, color, position) VALUES (?, ?, (SELECT COALESCE(MAX(position), -1) + 1 FROM lanes))`,
		name, color)
	if err != nil {
		return Lane{}, err
	}
	id, _ := res.LastInsertId()
	return s.Lane(ctx, id)
}

func (s *Store) UpdateLane(ctx context.Context, id int64, name, color string) error {
	return affected(s.db.ExecContext(ctx, `UPDATE lanes SET name = ?, color = ? WHERE id = ?`, name, color, id))
}

func (s *Store) DeleteLane(ctx context.Context, id int64) error {
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM tasks WHERE lane_id = ?`, id).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return ErrLaneNotEmpty
	}
	return affected(s.db.ExecContext(ctx, `DELETE FROM lanes WHERE id = ?`, id))
}

// ReorderLanes sets lane positions to the order of ids.
func (s *Store) ReorderLanes(ctx context.Context, ids []int64) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		for i, id := range ids {
			if _, err := tx.ExecContext(ctx, `UPDATE lanes SET position = ? WHERE id = ?`, i, id); err != nil {
				return err
			}
		}
		return nil
	})
}

// ---- field definitions ----

func (s *Store) FieldDefs(ctx context.Context) ([]FieldDef, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT key, label, type, position FROM field_defs ORDER BY position, key`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []FieldDef{}
	for rows.Next() {
		var f FieldDef
		if err := rows.Scan(&f.Key, &f.Label, &f.Type, &f.Position); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

func (s *Store) UpsertFieldDef(ctx context.Context, f FieldDef) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO field_defs (key, label, type, position)
		VALUES (?, ?, ?, (SELECT COALESCE(MAX(position), -1) + 1 FROM field_defs))
		ON CONFLICT(key) DO UPDATE SET label = excluded.label, type = excluded.type`,
		f.Key, f.Label, f.Type)
	return err
}

func (s *Store) DeleteFieldDef(ctx context.Context, key string) error {
	return affected(s.db.ExecContext(ctx, `DELETE FROM field_defs WHERE key = ?`, key))
}

// ---- tasks ----

const taskCols = `id, title, description, kind, lane_id, position, workdir, due_at, reminded_at, created_at, updated_at`

func scanTask(sc interface{ Scan(...any) error }) (Task, error) {
	var t Task
	var created, updated int64
	var due, reminded sql.NullInt64
	err := sc.Scan(&t.ID, &t.Title, &t.Description, &t.Kind, &t.LaneID, &t.Position, &t.Workdir, &due, &reminded, &created, &updated)
	t.CreatedAt, t.UpdatedAt = fromMillis(created), fromMillis(updated)
	t.DueAt, t.RemindedAt = fromNullMillis(due), fromNullMillis(reminded)
	t.Fields = map[string]string{}
	return t, err
}

func (s *Store) Tasks(ctx context.Context) ([]Task, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+taskCols+` FROM tasks ORDER BY lane_id, position, id`)
	if err != nil {
		return nil, err
	}
	out := []Task{}
	idx := map[int64]int{}
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		idx[t.ID] = len(out)
		out = append(out, t)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	frows, err := s.db.QueryContext(ctx, `SELECT task_id, key, value FROM task_fields`)
	if err != nil {
		return nil, err
	}
	defer frows.Close()
	for frows.Next() {
		var id int64
		var k, v string
		if err := frows.Scan(&id, &k, &v); err != nil {
			return nil, err
		}
		if i, ok := idx[id]; ok {
			out[i].Fields[k] = v
		}
	}
	return out, frows.Err()
}

func (s *Store) Task(ctx context.Context, id int64) (Task, error) {
	t, err := scanTask(s.db.QueryRowContext(ctx, `SELECT `+taskCols+` FROM tasks WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return t, ErrNotFound
	}
	if err != nil {
		return t, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT key, value FROM task_fields WHERE task_id = ?`, id)
	if err != nil {
		return t, err
	}
	defer rows.Close()
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return t, err
		}
		t.Fields[k] = v
	}
	return t, rows.Err()
}

// CreateTask appends the task to the end of its lane. LaneID 0 means the first lane.
func (s *Store) CreateTask(ctx context.Context, t Task) (Task, error) {
	if t.Kind == "" {
		t.Kind = "task"
	}
	var id int64
	err := s.tx(ctx, func(tx *sql.Tx) error {
		if t.LaneID == 0 {
			if err := tx.QueryRowContext(ctx, `SELECT id FROM lanes ORDER BY position, id LIMIT 1`).Scan(&t.LaneID); err != nil {
				return err
			}
		}
		now := s.ts()
		res, err := tx.ExecContext(ctx, `
			INSERT INTO tasks (title, description, kind, lane_id, position, workdir, due_at, created_at, updated_at)
			VALUES (?, ?, ?, ?, (SELECT COALESCE(MAX(position), -1) + 1 FROM tasks WHERE lane_id = ?), ?, ?, ?, ?)`,
			t.Title, t.Description, t.Kind, t.LaneID, t.LaneID, t.Workdir, toNullMillis(t.DueAt), now, now)
		if err != nil {
			return err
		}
		id, _ = res.LastInsertId()
		return setFields(ctx, tx, id, t.Fields)
	})
	if err != nil {
		return Task{}, err
	}
	return s.Task(ctx, id)
}

// UpdateTask overwrites the editable attributes (not lane/position) and merges fields:
// an empty field value removes that field. Changing the due time re-arms its reminder.
func (s *Store) UpdateTask(ctx context.Context, t Task) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		due := toNullMillis(t.DueAt)
		err := affected(tx.ExecContext(ctx,
			`UPDATE tasks SET title = ?, description = ?, kind = ?, workdir = ?,
				reminded_at = CASE WHEN due_at IS ? THEN reminded_at END, due_at = ?, updated_at = ? WHERE id = ?`,
			t.Title, t.Description, t.Kind, t.Workdir, due, due, s.ts(), t.ID))
		if err != nil {
			return err
		}
		return setFields(ctx, tx, t.ID, t.Fields)
	})
}

func setFields(ctx context.Context, tx *sql.Tx, taskID int64, fields map[string]string) error {
	for k, v := range fields {
		v = strings.TrimSpace(v)
		var err error
		if v == "" {
			_, err = tx.ExecContext(ctx, `DELETE FROM task_fields WHERE task_id = ? AND key = ?`, taskID, k)
		} else {
			_, err = tx.ExecContext(ctx, `
				INSERT INTO task_fields (task_id, key, value) VALUES (?, ?, ?)
				ON CONFLICT(task_id, key) DO UPDATE SET value = excluded.value`, taskID, k, v)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// DueTasks returns tasks whose due time is at or before now and that haven't been reminded yet.
// Fields are not loaded.
func (s *Store) DueTasks(ctx context.Context, now time.Time) ([]Task, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+taskCols+` FROM tasks
		WHERE reminded_at IS NULL AND due_at IS NOT NULL AND due_at <= ? ORDER BY due_at, id`, now.UnixMilli())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Task{}
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *Store) MarkReminded(ctx context.Context, id int64, at time.Time) error {
	return affected(s.db.ExecContext(ctx, `UPDATE tasks SET reminded_at = ? WHERE id = ?`, at.UnixMilli(), id))
}

func (s *Store) DeleteTask(ctx context.Context, id int64) error {
	return affected(s.db.ExecContext(ctx, `DELETE FROM tasks WHERE id = ?`, id))
}

// MoveTask places the task at index within laneID (clamped) and renumbers both lanes.
// It returns the lane the task came from.
func (s *Store) MoveTask(ctx context.Context, id, laneID int64, index int) (fromLane int64, err error) {
	err = s.tx(ctx, func(tx *sql.Tx) error {
		if err := tx.QueryRowContext(ctx, `SELECT lane_id FROM tasks WHERE id = ?`, id).Scan(&fromLane); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		var exists int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM lanes WHERE id = ?`, laneID).Scan(&exists); err != nil {
			return err
		}
		if exists == 0 {
			return ErrNotFound
		}
		ids, err := laneTaskIDs(ctx, tx, laneID, id)
		if err != nil {
			return err
		}
		if index < 0 || index > len(ids) {
			index = len(ids)
		}
		ids = append(ids[:index], append([]int64{id}, ids[index:]...)...)
		if _, err := tx.ExecContext(ctx, `UPDATE tasks SET lane_id = ?, updated_at = ? WHERE id = ?`, laneID, s.ts(), id); err != nil {
			return err
		}
		if err := renumber(ctx, tx, ids); err != nil {
			return err
		}
		if fromLane != laneID {
			old, err := laneTaskIDs(ctx, tx, fromLane, id)
			if err != nil {
				return err
			}
			return renumber(ctx, tx, old)
		}
		return nil
	})
	return fromLane, err
}

func laneTaskIDs(ctx context.Context, tx *sql.Tx, laneID, exclude int64) ([]int64, error) {
	rows, err := tx.QueryContext(ctx, `SELECT id FROM tasks WHERE lane_id = ? AND id != ? ORDER BY position, id`, laneID, exclude)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func renumber(ctx context.Context, tx *sql.Tx, ids []int64) error {
	for i, id := range ids {
		if _, err := tx.ExecContext(ctx, `UPDATE tasks SET position = ? WHERE id = ?`, i, id); err != nil {
			return err
		}
	}
	return nil
}
