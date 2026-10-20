package store

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode"
)

// Memory is knowledge kept across tasks: a Markdown document that the agents
// of the tasks linked to it read, and that is updated from their sessions.
type Memory struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Body        string `json:"body"`
	// Revision is the id of the latest revision, 0 while the memory has none.
	Revision  int64     `json:"revision"`
	Tasks     int       `json:"tasks"` // how many tasks link to it
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	// LastUsed is its latest revision or the latest activity in a session of
	// a task linked to it, whichever is newer; its creation without either.
	LastUsed time.Time `json:"last_used"`
	// Score ranks a search result: the more of the words and the more
	// telling the place (name, description, body), the higher.
	Score int `json:"score,omitempty"`
}

// Sources of a memory revision.
const (
	SourceUser  = "user"  // written by hand
	SourceAgent = "agent" // updated from an agent session
	SourceNote  = "note"  // a note appended to the memory
)

// MemoryRevision is one version of a memory's body.
type MemoryRevision struct {
	ID        int64     `json:"id"`
	MemoryID  int64     `json:"memory_id"`
	Body      string    `json:"body,omitempty"`
	Source    string    `json:"source"`
	SessionID *int64    `json:"session_id"`
	TaskID    *int64    `json:"task_id"`
	CreatedAt time.Time `json:"created_at"`
}

var (
	ErrMemoryExists  = errors.New("a memory with this name already exists")
	ErrMemoryChanged = errors.New("the memory has changed since it was read")
	ErrBadMemory     = errors.New("unknown memory")
)

const memoryCols = `m.id, m.name, m.description, m.body, m.revision, m.created_at, m.updated_at,
	(SELECT COUNT(*) FROM task_memories tm WHERE tm.memory_id = m.id),
	MAX(m.created_at,
		COALESCE((SELECT MAX(r.created_at) FROM memory_revisions r WHERE r.memory_id = m.id), 0),
		COALESCE((SELECT MAX(x.updated_at) FROM task_memories tm JOIN sessions x ON x.task_id = tm.task_id WHERE tm.memory_id = m.id), 0))`

func scanMemory(sc interface{ Scan(...any) error }) (Memory, error) {
	var x Memory
	var created, updated, used int64
	err := sc.Scan(&x.ID, &x.Name, &x.Description, &x.Body, &x.Revision, &created, &updated, &x.Tasks, &used)
	x.CreatedAt, x.UpdatedAt, x.LastUsed = fromMillis(created), fromMillis(updated), fromMillis(used)
	return x, err
}

// Memories lists the memories, the most recently used first.
func (s *Store) Memories(ctx context.Context) ([]Memory, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+memoryCols+` FROM memories m`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Memory{}
	for rows.Next() {
		x, err := scanMemory(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	slices.SortStableFunc(out, func(a, b Memory) int {
		if c := b.LastUsed.Compare(a.LastUsed); c != 0 {
			return c
		}
		return cmp.Compare(b.ID, a.ID)
	})
	return out, nil
}

func (s *Store) Memory(ctx context.Context, id int64) (Memory, error) {
	x, err := scanMemory(s.db.QueryRowContext(ctx, `SELECT `+memoryCols+` FROM memories m WHERE m.id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return x, ErrNotFound
	}
	return x, err
}

// CreateMemory adds a memory; a body, if given, is its first revision.
func (s *Store) CreateMemory(ctx context.Context, x Memory) (Memory, error) {
	var id int64
	err := s.tx(ctx, func(tx *sql.Tx) error {
		now := s.ts()
		res, err := tx.ExecContext(ctx, `INSERT INTO memories (name, description, created_at, updated_at) VALUES (?, ?, ?, ?)`,
			x.Name, x.Description, now, now)
		if err != nil {
			return memoryErr(err)
		}
		id, _ = res.LastInsertId()
		if strings.TrimSpace(x.Body) == "" {
			return nil
		}
		_, err = setMemoryBody(ctx, tx, MemoryRevision{MemoryID: id, Body: x.Body, Source: SourceUser}, now)
		return err
	})
	if err != nil {
		return Memory{}, err
	}
	return s.Memory(ctx, id)
}

// UpdateMemory saves name and description; the body changes with
// SetMemoryBody.
func (s *Store) UpdateMemory(ctx context.Context, x Memory) error {
	return memoryErr(affected(s.db.ExecContext(ctx, `UPDATE memories SET name = ?, description = ?, updated_at = ? WHERE id = ?`,
		x.Name, x.Description, s.ts(), x.ID)))
}

func memoryErr(err error) error {
	if err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed: memories.name") {
		return ErrMemoryExists
	}
	return err
}

func (s *Store) DeleteMemory(ctx context.Context, id int64) error {
	return affected(s.db.ExecContext(ctx, `DELETE FROM memories WHERE id = ?`, id))
}

// SetMemoryBody makes rev.Body the memory's body, as a new revision. With
// base set, the memory must still be at that revision, else ErrMemoryChanged.
// A body equal to the current one adds no revision; changed tells.
func (s *Store) SetMemoryBody(ctx context.Context, rev MemoryRevision, base *int64) (out MemoryRevision, changed bool, err error) {
	err = s.tx(ctx, func(tx *sql.Tx) error {
		var body string
		var cur int64
		err := tx.QueryRowContext(ctx, `SELECT body, revision FROM memories WHERE id = ?`, rev.MemoryID).Scan(&body, &cur)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if base != nil && *base != cur {
			return ErrMemoryChanged
		}
		if strings.TrimSpace(body) == strings.TrimSpace(rev.Body) {
			return nil
		}
		changed = true
		out, err = setMemoryBody(ctx, tx, rev, s.ts())
		return err
	})
	return out, changed, err
}

// AppendMemoryNote adds text at the end of the memory's body, as a revision.
func (s *Store) AppendMemoryNote(ctx context.Context, rev MemoryRevision) (MemoryRevision, error) {
	var out MemoryRevision
	err := s.tx(ctx, func(tx *sql.Tx) error {
		var body string
		err := tx.QueryRowContext(ctx, `SELECT body FROM memories WHERE id = ?`, rev.MemoryID).Scan(&body)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if body = strings.TrimSpace(body); body != "" {
			body += "\n\n"
		}
		rev.Body = body + strings.TrimSpace(rev.Body)
		out, err = setMemoryBody(ctx, tx, rev, s.ts())
		return err
	})
	return out, err
}

func setMemoryBody(ctx context.Context, tx *sql.Tx, rev MemoryRevision, now int64) (MemoryRevision, error) {
	rev.Body = strings.TrimSpace(rev.Body)
	res, err := tx.ExecContext(ctx, `INSERT INTO memory_revisions (memory_id, body, source, session_id, task_id, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
		rev.MemoryID, rev.Body, rev.Source, rev.SessionID, rev.TaskID, now)
	if err != nil {
		return rev, err
	}
	rev.ID, _ = res.LastInsertId()
	rev.CreatedAt = fromMillis(now)
	_, err = tx.ExecContext(ctx, `UPDATE memories SET body = ?, revision = ?, updated_at = ? WHERE id = ?`, rev.Body, rev.ID, now, rev.MemoryID)
	return rev, err
}

// MemoryRevisions lists a memory's revisions without their bodies, newest first.
func (s *Store) MemoryRevisions(ctx context.Context, memoryID int64) ([]MemoryRevision, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, memory_id, '', source, session_id, task_id, created_at FROM memory_revisions
		WHERE memory_id = ? ORDER BY id DESC`, memoryID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []MemoryRevision{}
	for rows.Next() {
		x, err := scanRevision(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

// MemoryRevision returns a revision with its body, and the body of the
// revision before it ("" for the first).
func (s *Store) MemoryRevision(ctx context.Context, id int64) (MemoryRevision, string, error) {
	x, err := scanRevision(s.db.QueryRowContext(ctx, `SELECT id, memory_id, body, source, session_id, task_id, created_at
		FROM memory_revisions WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return x, "", ErrNotFound
	}
	if err != nil {
		return x, "", err
	}
	var prev string
	err = s.db.QueryRowContext(ctx, `SELECT body FROM memory_revisions WHERE memory_id = ? AND id < ? ORDER BY id DESC LIMIT 1`,
		x.MemoryID, x.ID).Scan(&prev)
	if errors.Is(err, sql.ErrNoRows) {
		err = nil
	}
	return x, prev, err
}

func scanRevision(sc interface{ Scan(...any) error }) (MemoryRevision, error) {
	var x MemoryRevision
	var sess, task sql.NullInt64
	var created int64
	err := sc.Scan(&x.ID, &x.MemoryID, &x.Body, &x.Source, &sess, &task, &created)
	x.SessionID, x.TaskID, x.CreatedAt = fromNullID(sess), fromNullID(task), fromMillis(created)
	return x, err
}

// MemoryTasks returns the ids of the tasks linked to the memory, newest first.
func (s *Store) MemoryTasks(ctx context.Context, memoryID int64) ([]int64, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT task_id FROM task_memories WHERE memory_id = ? ORDER BY task_id DESC`, memoryID)
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

// SetTaskMemories replaces the memories the task is linked to.
func (s *Store) SetTaskMemories(ctx context.Context, taskID int64, ids []int64) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM task_memories WHERE task_id = ?`, taskID); err != nil {
			return err
		}
		return setTaskMemories(ctx, tx, taskID, ids)
	})
}

// setTaskMemories links the task to the memories, skipping repeats. Unknown
// memories are ErrBadMemory.
func setTaskMemories(ctx context.Context, tx *sql.Tx, taskID int64, ids []int64) error {
	for _, id := range ids {
		res, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO task_memories (task_id, memory_id) SELECT ?, id FROM memories WHERE id = ?`, taskID, id)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			var exists int
			if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM memories WHERE id = ?`, id).Scan(&exists); err != nil {
				return err
			}
			if exists == 0 {
				return fmt.Errorf("%w: %d", ErrBadMemory, id)
			}
		}
	}
	return nil
}

// inheritedMemories are the memories a new task gets when it names none:
// those of the tasks it builds on.
func inheritedMemories(ctx context.Context, tx *sql.Tx, sources []int64) ([]int64, error) {
	if len(sources) == 0 {
		return nil, nil
	}
	args := []any{}
	for _, id := range sources {
		args = append(args, id)
	}
	rows, err := tx.QueryContext(ctx, `SELECT DISTINCT memory_id FROM task_memories WHERE task_id IN (?`+strings.Repeat(", ?", len(sources)-1)+`) ORDER BY memory_id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// SearchMemories finds the memories whose name, description or body has any
// of the query's words, best first: by Score, then the most recently used.
// A word matches the words that start with it, less its last two letters
// when it is long, so "memoriju" finds "memorija". Short and common words
// are left out; a query without others finds nothing.
func (s *Store) SearchMemories(ctx context.Context, query string) ([]Memory, error) {
	words := searchWords(query)
	if len(words) == 0 {
		return []Memory{}, nil
	}
	all, err := s.Memories(ctx)
	if err != nil {
		return nil, err
	}
	out := []Memory{}
	for _, m := range all {
		name, desc, body := textWords(m.Name), textWords(m.Description), textWords(m.Body)
		for _, w := range words {
			switch {
			case hasWord(name, w):
				m.Score += 3
			case hasWord(desc, w):
				m.Score += 2
			case hasWord(body, w):
				m.Score++
			}
		}
		if m.Score > 0 {
			out = append(out, m)
		}
	}
	slices.SortStableFunc(out, func(a, b Memory) int { return cmp.Compare(b.Score, a.Score) })
	return out, nil
}

// stopWords are left out of searches: they would match nearly every memory.
var stopWords = map[string]bool{}

func init() {
	for _, w := range strings.Fields(`the and for with from that this into have has are was were not but you your our can how what when why which
		all any its about should would will use using make need add also there then than them they some only
		ali ako bez biti bio bila će ćemo dok gdje ili ima isto jer još kad kada kako koja koje koji kroz mora može nakon
		nije niti oko ovaj ovo ovu pri sad sada sam samo sve što tako taj treba već više zbog želim`) {
		stopWords[w] = true
	}
}

// searchWords are the distinct words of a query worth searching for, as
// prefixes: lower case, at least three letters, long ones less their last two.
func searchWords(q string) []string {
	var out []string
	for _, w := range splitWords(q) {
		r := []rune(w)
		if len(r) < 3 || stopWords[w] {
			continue
		}
		if len(r) > 5 {
			w = string(r[:len(r)-2])
		}
		if !slices.Contains(out, w) {
			out = append(out, w)
		}
	}
	return out
}

func splitWords(s string) []string {
	return strings.FieldsFunc(strings.ToLower(s), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
}

// textWords are the distinct words of a text, sorted, for hasWord.
func textWords(s string) []string {
	ws := splitWords(s)
	slices.Sort(ws)
	return slices.Compact(ws)
}

// hasWord tells whether a sorted word list has a word starting with prefix.
func hasWord(sorted []string, prefix string) bool {
	i, _ := slices.BinarySearch(sorted, prefix)
	return i < len(sorted) && strings.HasPrefix(sorted[i], prefix)
}
