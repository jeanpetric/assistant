package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// ErrNoIDs: the table's id blocks are used up and ids are leased only.
var ErrNoIDs = errors.New("no ids left in the table's blocks")

// idTables are the tables whose rows get ids from blocks: every table with an
// INTEGER PRIMARY KEY the app adds rows to (settings has its single row).
var idTables = map[string]bool{
	"lanes": true, "tasks": true, "sessions": true, "messages": true, "transition_rules": true,
	"schedules": true, "notifications": true, "projects": true, "skills": true, "skill_automations": true,
	"artifacts": true, "memories": true, "memory_revisions": true, "trello_boards": true, "queued_messages": true,
}

// NodeID is the node this database is in sync: 1 unless it joined another.
func (s *Store) NodeID(ctx context.Context) (int64, error) {
	var id int64
	err := s.db.QueryRowContext(ctx, `SELECT node_id FROM sync_node WHERE id = 1`).Scan(&id)
	return id, err
}

// SetLeasedIDs makes new rows take ids from added blocks only: once a table's
// are used up, inserts into it fail with ErrNoIDs. Off (the default), ids go
// on from the table's highest when no block is left.
func (s *Store) SetLeasedIDs(on bool) { s.leasedIDs = on }

// AddBlock gives tbl the ids from through to, inclusive, for its new rows.
// Blocks are used lowest first.
func (s *Store) AddBlock(ctx context.Context, tbl string, from, to int64) error {
	if !idTables[tbl] {
		return fmt.Errorf("no id blocks for table %q", tbl)
	}
	if from < 1 || to < from {
		return fmt.Errorf("bad id block %d-%d", from, to)
	}
	return s.tx(ctx, func(tx *sql.Tx) error {
		var overlap int
		err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM id_blocks WHERE tbl = ? AND next_id <= ? AND end_id >= ?`,
			tbl, to, from).Scan(&overlap)
		if err != nil {
			return err
		}
		if overlap > 0 {
			return fmt.Errorf("id block %d-%d overlaps one of %s", from, to, tbl)
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO id_blocks (tbl, next_id, end_id) VALUES (?, ?, ?)`, tbl, from, to)
		return err
	})
}

// nextID takes the id for a new row of tbl from its lowest block, skipping
// ids already in use. Without a block it is the table's highest plus one,
// unless ids are leased only.
func (s *Store) nextID(ctx context.Context, tx *sql.Tx, tbl string) (int64, error) {
	if !idTables[tbl] {
		return 0, fmt.Errorf("no id blocks for table %q", tbl)
	}
	for {
		var next, end int64
		err := tx.QueryRowContext(ctx, `SELECT next_id, end_id FROM id_blocks WHERE tbl = ? ORDER BY next_id LIMIT 1`, tbl).
			Scan(&next, &end)
		if errors.Is(err, sql.ErrNoRows) {
			if s.leasedIDs {
				return 0, fmt.Errorf("%s: %w", tbl, ErrNoIDs)
			}
			var id int64
			err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(id), 0) + 1 FROM `+tbl).Scan(&id)
			return id, err
		}
		if err != nil {
			return 0, err
		}
		if next == end {
			_, err = tx.ExecContext(ctx, `DELETE FROM id_blocks WHERE tbl = ? AND end_id = ?`, tbl, end)
		} else {
			_, err = tx.ExecContext(ctx, `UPDATE id_blocks SET next_id = ? WHERE tbl = ? AND end_id = ?`, next+1, tbl, end)
		}
		if err != nil {
			return 0, err
		}
		var used int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+tbl+` WHERE id = ?`, next).Scan(&used); err != nil {
			return 0, err
		}
		if used == 0 {
			return next, nil
		}
	}
}

// insertTx adds a row to tbl with an id from nextID and returns the id. The
// query names id as its first column and first parameter; args follow it.
func (s *Store) insertTx(ctx context.Context, tx *sql.Tx, tbl, query string, args ...any) (int64, error) {
	id, err := s.nextID(ctx, tx, tbl)
	if err != nil {
		return 0, err
	}
	_, err = tx.ExecContext(ctx, query, append([]any{id}, args...)...)
	return id, err
}

// insert is insertTx in a transaction of its own.
func (s *Store) insert(ctx context.Context, tbl, query string, args ...any) (int64, error) {
	var id int64
	err := s.tx(ctx, func(tx *sql.Tx) error {
		var err error
		id, err = s.insertTx(ctx, tx, tbl, query, args...)
		return err
	})
	return id, err
}
