package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
)

var (
	ErrKindExists  = errors.New("kind exists")
	ErrKindBuiltIn = errors.New("kind is built in")
	ErrKindInUse   = errors.New("kind is in use")
)

// BuiltInKinds can't be deleted: task is a new task's kind, reminder the
// Remind dialog's.
var BuiltInKinds = []string{"task", "reminder"}

// Kinds are the task kinds, in order.
func (s *Store) Kinds(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT name FROM task_kinds ORDER BY position, name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// HasKind tells whether name is a task kind.
func (s *Store) HasKind(ctx context.Context, name string) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM task_kinds WHERE name = ?`, name).Scan(&n)
	return n > 0, err
}

// AddKind puts a kind at the end of the list; ErrKindExists if it is there.
func (s *Store) AddKind(ctx context.Context, name string) error {
	res, err := s.db.ExecContext(ctx, `
		INSERT OR IGNORE INTO task_kinds (name, position)
		VALUES (?, (SELECT COALESCE(MAX(position), -1) + 1 FROM task_kinds))`, name)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n == 0 {
		return kindErr{ErrKindExists, "kind " + name + " exists"}
	}
	return nil
}

// Kind is a task kind with what has it.
type Kind struct {
	Name        string `json:"name"`
	BuiltIn     bool   `json:"built_in"`
	Tasks       int    `json:"tasks"` // archived ones included
	Rules       int    `json:"rules"`
	Automations int    `json:"automations"`
}

// KindUses are the task kinds, in order, each with what has it.
func (s *Store) KindUses(ctx context.Context) ([]Kind, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT k.name,
		(SELECT COUNT(*) FROM tasks t WHERE t.kind = k.name),
		(SELECT COUNT(*) FROM transition_rules r WHERE r.kind = k.name),
		(SELECT COUNT(*) FROM skill_automations a WHERE a.kind = k.name)
		FROM task_kinds k ORDER BY k.position, k.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Kind{}
	for rows.Next() {
		var k Kind
		if err := rows.Scan(&k.Name, &k.Tasks, &k.Rules, &k.Automations); err != nil {
			return nil, err
		}
		k.BuiltIn = slices.Contains(BuiltInKinds, k.Name)
		out = append(out, k)
	}
	return out, rows.Err()
}

// DeleteKind deletes a kind, turning its tasks into kind task. A built-in
// kind (ErrKindBuiltIn) and one transition rules or skill automations have
// (ErrKindInUse) stay; ErrNotFound if there is none.
func (s *Store) DeleteKind(ctx context.Context, name string) error {
	if slices.Contains(BuiltInKinds, name) {
		return kindErr{ErrKindBuiltIn, "kind " + name + " is built in"}
	}
	return s.tx(ctx, func(tx *sql.Tx) error {
		var rules, automations int
		err := tx.QueryRowContext(ctx, `SELECT
			(SELECT COUNT(*) FROM transition_rules WHERE kind = ?1),
			(SELECT COUNT(*) FROM skill_automations WHERE kind = ?1)`, name).Scan(&rules, &automations)
		if err != nil {
			return err
		}
		if rules > 0 || automations > 0 {
			return kindErr{ErrKindInUse, fmt.Sprintf("kind %s is used by %s and %s", name,
				plural(rules, "transition rule"), plural(automations, "skill automation"))}
		}
		if err := affected(tx.ExecContext(ctx, `DELETE FROM task_kinds WHERE name = ?`, name)); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE tasks SET kind = 'task', updated_at = ? WHERE kind = ?`, s.ts(), name)
		return err
	})
}

func plural(n int, what string) string {
	if n == 1 {
		return fmt.Sprintf("1 %s", what)
	}
	return fmt.Sprintf("%d %ss", n, what)
}

// kindErr is one of the kind errors with its message.
type kindErr struct {
	err error
	msg string
}

func (e kindErr) Error() string { return e.msg }
func (e kindErr) Unwrap() error { return e.err }
