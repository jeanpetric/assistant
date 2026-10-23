package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
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
	ProjectID   int64             `json:"project_id"`
	LaneID      int64             `json:"lane_id"`
	Position    int               `json:"position"`
	Workdir     string            `json:"workdir"`
	Fields      map[string]string `json:"fields"`
	// Earlier tasks this one builds on, in order: their agents' results go
	// into the context of this task's agents.
	ContextFrom []int64 `json:"context_from"`
	// Memories its agents read and update. Nil on create: those of the
	// tasks it builds on.
	Memories   []int64    `json:"memories"`
	DueAt      *time.Time `json:"due_at"`      // notify when reached
	RemindedAt *time.Time `json:"reminded_at"` // set once the due notification fired
	ArchivedAt *time.Time `json:"archived_at"` // archived tasks are off the board
	// Watched tasks are counted per project in the ⌥K picker; moving the task
	// into a done lane (see doneLanes) stops watching it.
	Watched bool `json:"watched"`
	// When the task entered its lane; auto-archive counts from here.
	LaneChangedAt time.Time `json:"lane_changed_at"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

type FieldDef struct {
	Key      string `json:"key"`
	Label    string `json:"label"`
	Type     string `json:"type"` // text | url
	Position int    `json:"position"`
}

var (
	ErrLaneNotEmpty = errors.New("lane still has tasks")
	ErrBadContext   = errors.New("a task can only build on other existing tasks")
)

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

// DeleteLane deletes a lane without tasks on the board. Archived tasks in it
// move to the first remaining lane, which is where a restore puts them.
func (s *Store) DeleteLane(ctx context.Context, id int64) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		var active, archived int
		err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FILTER (WHERE archived_at IS NULL), COUNT(*) FILTER (WHERE archived_at IS NOT NULL)
			FROM tasks WHERE lane_id = ?`, id).Scan(&active, &archived)
		if err != nil {
			return err
		}
		if active > 0 {
			return ErrLaneNotEmpty
		}
		if archived > 0 {
			var other int64
			err := tx.QueryRowContext(ctx, `SELECT id FROM lanes WHERE id != ? ORDER BY position, id LIMIT 1`, id).Scan(&other)
			if errors.Is(err, sql.ErrNoRows) {
				return ErrLaneNotEmpty
			}
			if err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE tasks SET lane_id = ? WHERE lane_id = ?`, other, id); err != nil {
				return err
			}
		}
		return affected(tx.ExecContext(ctx, `DELETE FROM lanes WHERE id = ?`, id))
	})
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

const taskCols = `id, title, description, kind, project_id, lane_id, position, workdir, due_at, reminded_at, archived_at, lane_changed_at, created_at, updated_at, watched`

func scanTask(sc interface{ Scan(...any) error }) (Task, error) {
	var t Task
	var laneChanged, created, updated int64
	var due, reminded, archived sql.NullInt64
	err := sc.Scan(&t.ID, &t.Title, &t.Description, &t.Kind, &t.ProjectID, &t.LaneID, &t.Position, &t.Workdir, &due, &reminded, &archived,
		&laneChanged, &created, &updated, &t.Watched)
	t.CreatedAt, t.UpdatedAt, t.LaneChangedAt = fromMillis(created), fromMillis(updated), fromMillis(laneChanged)
	t.DueAt, t.RemindedAt, t.ArchivedAt = fromNullMillis(due), fromNullMillis(reminded), fromNullMillis(archived)
	t.Fields = map[string]string{}
	t.ContextFrom = []int64{}
	t.Memories = []int64{}
	return t, err
}

// laneSort is the order of tasks within a lane: those with a due time first,
// soonest first, then the rest in their manual order.
const laneSort = `due_at IS NULL, due_at, position, id`

// Tasks returns the tasks on the board, i.e. not archived, of one project
// (projectID 0: of all projects), each lane's in laneSort.
func (s *Store) Tasks(ctx context.Context, projectID int64) ([]Task, error) {
	return s.queryTasks(ctx, `SELECT `+taskCols+` FROM tasks WHERE archived_at IS NULL AND (? = 0 OR project_id = ?)
		ORDER BY lane_id, `+laneSort, projectID, projectID)
}

// WatchedTasks returns the watched tasks on the board of every project, by
// project, then lane, then laneSort within the lane.
func (s *Store) WatchedTasks(ctx context.Context) ([]Task, error) {
	return s.queryTasks(ctx, `SELECT `+taskCols+` FROM tasks WHERE watched AND archived_at IS NULL
		ORDER BY (SELECT position FROM projects WHERE projects.id = project_id), project_id,
		(SELECT position FROM lanes WHERE lanes.id = lane_id), lane_id, `+laneSort)
}

// ArchivedTasks returns archived tasks of one project (projectID 0: of all),
// most recently archived first.
func (s *Store) ArchivedTasks(ctx context.Context, projectID int64) ([]Task, error) {
	return s.queryTasks(ctx, `SELECT `+taskCols+` FROM tasks WHERE archived_at IS NOT NULL AND (? = 0 OR project_id = ?)
		ORDER BY archived_at DESC, id DESC`, projectID, projectID)
}

// SearchTasks finds tasks of every project, archived ones included, whose
// title or description holds each word of the query (case-insensitive), or
// whose id is the query ("12" or "#12"). Tasks on a board come first, then
// the most recently updated; at most limit. An empty query lists the most
// recently updated tasks.
func (s *Store) SearchTasks(ctx context.Context, query string, limit int) ([]Task, error) {
	where, args := []string{"1"}, []any{}
	for _, w := range strings.Fields(query) {
		like := "%" + likeEscaper.Replace(w) + "%"
		cond := `(title LIKE ? ESCAPE '\' OR description LIKE ? ESCAPE '\'`
		args = append(args, like, like)
		if id, err := strconv.ParseInt(strings.TrimPrefix(w, "#"), 10, 64); err == nil {
			cond += ` OR id = ?`
			args = append(args, id)
		}
		where = append(where, cond+")")
	}
	args = append(args, limit)
	return s.queryTasks(ctx, `SELECT `+taskCols+` FROM tasks WHERE `+strings.Join(where, " AND ")+`
		ORDER BY archived_at IS NOT NULL, updated_at DESC, id DESC LIMIT ?`, args...)
}

var likeEscaper = strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)

// queryTasks runs a task query and loads the fields of the tasks it returns.
func (s *Store) queryTasks(ctx context.Context, q string, args ...any) ([]Task, error) {
	rows, err := s.db.QueryContext(ctx, q, args...)
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
	if err := frows.Err(); err != nil {
		return nil, err
	}
	frows.Close()

	crows, err := s.db.QueryContext(ctx, `SELECT task_id, source_id FROM task_context ORDER BY task_id, position`)
	if err != nil {
		return nil, err
	}
	defer crows.Close()
	for crows.Next() {
		var id, src int64
		if err := crows.Scan(&id, &src); err != nil {
			return nil, err
		}
		if i, ok := idx[id]; ok {
			out[i].ContextFrom = append(out[i].ContextFrom, src)
		}
	}
	if err := crows.Err(); err != nil {
		return nil, err
	}
	crows.Close()

	mrows, err := s.db.QueryContext(ctx, `SELECT task_id, memory_id FROM task_memories ORDER BY task_id, memory_id`)
	if err != nil {
		return nil, err
	}
	defer mrows.Close()
	for mrows.Next() {
		var id, mem int64
		if err := mrows.Scan(&id, &mem); err != nil {
			return nil, err
		}
		if i, ok := idx[id]; ok {
			out[i].Memories = append(out[i].Memories, mem)
		}
	}
	return out, mrows.Err()
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
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			rows.Close()
			return t, err
		}
		t.Fields[k] = v
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return t, err
	}
	crows, err := s.db.QueryContext(ctx, `SELECT source_id FROM task_context WHERE task_id = ? ORDER BY position`, id)
	if err != nil {
		return t, err
	}
	defer crows.Close()
	for crows.Next() {
		var src int64
		if err := crows.Scan(&src); err != nil {
			return t, err
		}
		t.ContextFrom = append(t.ContextFrom, src)
	}
	if err := crows.Err(); err != nil {
		return t, err
	}
	crows.Close()
	mrows, err := s.db.QueryContext(ctx, `SELECT memory_id FROM task_memories WHERE task_id = ? ORDER BY memory_id`, id)
	if err != nil {
		return t, err
	}
	defer mrows.Close()
	for mrows.Next() {
		var mem int64
		if err := mrows.Scan(&mem); err != nil {
			return t, err
		}
		t.Memories = append(t.Memories, mem)
	}
	return t, mrows.Err()
}

// CreateTask appends the task to the end of its lane. LaneID 0 means the first
// lane, ProjectID 0 the first project; nil Memories inherits them (see Task).
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
		if t.ProjectID == 0 {
			if err := tx.QueryRowContext(ctx, `SELECT id FROM projects ORDER BY position, id LIMIT 1`).Scan(&t.ProjectID); err != nil {
				return err
			}
		}
		now := s.ts()
		res, err := tx.ExecContext(ctx, `
			INSERT INTO tasks (title, description, kind, project_id, lane_id, position, workdir, due_at, lane_changed_at, created_at, updated_at, watched)
			VALUES (?, ?, ?, ?, ?, `+nextPosition+`, ?, ?, ?, ?, ?, ?)`,
			t.Title, t.Description, t.Kind, t.ProjectID, t.LaneID, t.ProjectID, t.LaneID, t.Workdir, toNullMillis(t.DueAt), now, now, now, boolInt(t.Watched))
		if err != nil {
			return err
		}
		id, _ = res.LastInsertId()
		if err := setContextFrom(ctx, tx, id, t.ContextFrom); err != nil {
			return err
		}
		mems := t.Memories
		if mems == nil {
			if mems, err = inheritedMemories(ctx, tx, t.ContextFrom); err != nil {
				return err
			}
		}
		if err := setTaskMemories(ctx, tx, id, mems); err != nil {
			return err
		}
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

// SetContextFrom replaces the tasks the task builds on.
func (s *Store) SetContextFrom(ctx context.Context, taskID int64, sources []int64) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM task_context WHERE task_id = ?`, taskID); err != nil {
			return err
		}
		return setContextFrom(ctx, tx, taskID, sources)
	})
}

// ContextUsers returns the tasks that build on the task, oldest first.
func (s *Store) ContextUsers(ctx context.Context, sourceID int64) ([]int64, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT task_id FROM task_context WHERE source_id = ? ORDER BY task_id`, sourceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []int64{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// setContextFrom links the task to its sources in order, skipping repeats.
// Unknown tasks or the task itself are ErrBadContext.
func setContextFrom(ctx context.Context, tx *sql.Tx, taskID int64, sources []int64) error {
	seen := map[int64]bool{}
	for _, src := range sources {
		if seen[src] {
			continue
		}
		seen[src] = true
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM tasks WHERE id = ?`, src).Scan(&n); err != nil {
			return err
		}
		if n == 0 || src == taskID {
			return fmt.Errorf("%w: %d", ErrBadContext, src)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO task_context (task_id, source_id, position) VALUES (?, ?, ?)`,
			taskID, src, len(seen)-1); err != nil {
			return err
		}
	}
	return nil
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

// nextPosition is the end of a lane's board tasks within a project; its
// parameters are the project and lane ids.
const nextPosition = `(SELECT COALESCE(MAX(position), -1) + 1 FROM tasks WHERE project_id = ? AND lane_id = ? AND archived_at IS NULL)`

// MoveTask places the task at index among its project's tasks in laneID (clamped,
// counted in laneSort) and renumbers both lanes. Tasks with a due time still
// show in due order, so the index matters for the order of the rest.
// Moving an archived task brings it back to the board.
// It returns the lane the task came from.
func (s *Store) MoveTask(ctx context.Context, id, laneID int64, index int) (fromLane int64, err error) {
	err = s.tx(ctx, func(tx *sql.Tx) error {
		var project int64
		var archived bool
		if err := tx.QueryRowContext(ctx, `SELECT project_id, lane_id, archived_at IS NOT NULL FROM tasks WHERE id = ?`, id).
			Scan(&project, &fromLane, &archived); err != nil {
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
		ids, err := laneTaskIDs(ctx, tx, project, laneID, id)
		if err != nil {
			return err
		}
		if index < 0 || index > len(ids) {
			index = len(ids)
		}
		ids = append(ids[:index], append([]int64{id}, ids[index:]...)...)
		now := s.ts()
		laneChanged := fromLane != laneID || archived
		// A lane change drops the lane the task was to move to after a turn (see SetTurnLane),
		// and arriving in the done lane ends watching the task and archives its notifications.
		if _, err := tx.ExecContext(ctx, `UPDATE tasks SET lane_id = ?, archived_at = NULL, updated_at = ?,
			lane_changed_at = CASE WHEN ? THEN ? ELSE lane_changed_at END, turn_lane_id = CASE WHEN ? THEN NULL ELSE turn_lane_id END,
			watched = CASE WHEN ? AND ? IN (`+doneLanes+`) THEN 0 ELSE watched END
			WHERE id = ?`, laneID, now, laneChanged, now, laneChanged, laneChanged, laneID, id); err != nil {
			return err
		}
		if laneChanged {
			if _, err := tx.ExecContext(ctx, `UPDATE notifications SET archived_at = ?, read_at = COALESCE(read_at, ?)
				WHERE task_id = ? AND archived_at IS NULL AND ? IN (`+doneLanes+`)`, now, now, id, laneID); err != nil {
				return err
			}
		}
		if err := renumber(ctx, tx, ids); err != nil {
			return err
		}
		if fromLane != laneID {
			old, err := laneTaskIDs(ctx, tx, project, fromLane, id)
			if err != nil {
				return err
			}
			return renumber(ctx, tx, old)
		}
		return nil
	})
	return fromLane, err
}

// doneLanes are the lanes where tasks are done: the one named Done and the
// one Settings archives from.
const doneLanes = `SELECT id FROM lanes WHERE lower(name) = 'done' UNION SELECT archive_lane_id FROM settings WHERE id = 1`

// SetWatched starts or stops watching the task.
func (s *Store) SetWatched(ctx context.Context, taskID int64, watched bool) error {
	return affected(s.db.ExecContext(ctx, `UPDATE tasks SET watched = ? WHERE id = ?`, boolInt(watched), taskID))
}

// SetTurnLane sets the lane the task moves to once an agent turn on it
// finishes; nil keeps it where it is. Moving to another lane clears it.
func (s *Store) SetTurnLane(ctx context.Context, taskID int64, laneID *int64) error {
	return affected(s.db.ExecContext(ctx, `UPDATE tasks SET turn_lane_id = ? WHERE id = ?`, laneID, taskID))
}

// TurnLane returns the lane SetTurnLane set for the task, or nil.
func (s *Store) TurnLane(ctx context.Context, taskID int64) (*int64, error) {
	var lane sql.NullInt64
	err := s.db.QueryRowContext(ctx, `SELECT turn_lane_id FROM tasks WHERE id = ?`, taskID).Scan(&lane)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return fromNullID(lane), err
}

// laneTaskIDs returns the board tasks of a project's lane in laneSort, without exclude.
func laneTaskIDs(ctx context.Context, tx *sql.Tx, projectID, laneID, exclude int64) ([]int64, error) {
	rows, err := tx.QueryContext(ctx, `SELECT id FROM tasks WHERE project_id = ? AND lane_id = ? AND id != ? AND archived_at IS NULL
		ORDER BY `+laneSort, projectID, laneID, exclude)
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
