package store

import (
	"context"
	"database/sql"
	"fmt"
)

// On a hub, sync_outbox is the log every node pulls from: its seq is the
// global order of changes. The hub's own changes land there through the
// triggers; pushed changes that changed a row are added by ApplyPushed. An
// entry's node_id is the node the change came from, so a node is not sent
// back what it pushed.

// SetHub makes this node a hub: with no id block left for a table, it leases
// itself the next one, so its new rows never take ids given to other nodes.
func (s *Store) SetHub(on bool) { s.hub = on }

// leaseSize is how many ids a block of tbl has: messages come by the hundred.
func leaseSize(tbl string) int64 {
	if tbl == "messages" {
		return 100000
	}
	return 1000
}

// LeaseBlock gives node a new block of ids for tbl, above every id used or
// given out before, and returns its first and last id. A block for this
// node itself is added to its id blocks.
func (s *Store) LeaseBlock(ctx context.Context, tbl string, node int64) (from, to int64, err error) {
	err = s.tx(ctx, func(tx *sql.Tx) error {
		from, to, err = s.leaseTx(ctx, tx, tbl, node)
		return err
	})
	return from, to, err
}

// leaseTx is LeaseBlock in tx; node 0 is this node.
func (s *Store) leaseTx(ctx context.Context, tx *sql.Tx, tbl string, node int64) (int64, int64, error) {
	if !idTables[tbl] {
		return 0, 0, fmt.Errorf("no id blocks for table %q", tbl)
	}
	var self int64
	if err := tx.QueryRowContext(ctx, `SELECT node_id FROM sync_node WHERE id = 1`).Scan(&self); err != nil {
		return 0, 0, err
	}
	if node == 0 {
		node = self
	}
	var highest int64
	err := tx.QueryRowContext(ctx, `SELECT MAX(
			(SELECT COALESCE(MAX(end_id), 0) FROM id_leases WHERE tbl = ?1),
			(SELECT COALESCE(MAX(end_id), 0) FROM id_blocks WHERE tbl = ?1),
			(SELECT COALESCE(MAX(id), 0) FROM `+tbl+`))`, tbl).Scan(&highest)
	if err != nil {
		return 0, 0, err
	}
	// Blocks start at a round number, which tells whose an id is.
	size := leaseSize(tbl)
	from := (highest/size + 1) * size
	to := from + size - 1
	if _, err := tx.ExecContext(ctx, `INSERT INTO id_leases (tbl, start_id, end_id, node_id, created_at) VALUES (?, ?, ?, ?, ?)`,
		tbl, from, to, node, s.ts()); err != nil {
		return 0, 0, err
	}
	if node == self {
		if _, err := tx.ExecContext(ctx, `INSERT INTO id_blocks (tbl, next_id, end_id) VALUES (?, ?, ?)`, tbl, from, to); err != nil {
			return 0, 0, err
		}
	}
	return from, to, nil
}

// ApplyPushed applies changes pushed by node, as ApplyChanges, and logs each
// row they changed in sync_outbox as node's, in the same transaction.
func (s *Store) ApplyPushed(ctx context.Context, node int64, changes []Change) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		changed, err := applyTx(ctx, tx, changes)
		if err != nil {
			return err
		}
		for _, row := range changed {
			_, err := tx.ExecContext(ctx, `INSERT INTO sync_outbox (tbl, pk_json, op, changed_at_ms, node_id)
				SELECT tbl, pk_json, CASE WHEN deleted THEN 'delete' ELSE 'update' END, changed_at_ms, ?
				FROM sync_rows WHERE tbl = ? AND pk_json = ?`, node, row[0], row[1])
			if err != nil {
				return err
			}
		}
		return nil
	})
}

// Pull is a page of the hub's log for a node.
type Pull struct {
	Changes []Change `json:"changes"`
	NextSeq int64    `json:"next_seq"` // the since of the next page
	More    bool     `json:"more"`     // whether the log goes on after NextSeq
}

// PullChanges reads the log after since, up to limit entries (0: all): the
// rows changed, each once with its present content, except those whose
// latest entry came from node.
func (s *Store) PullChanges(ctx context.Context, since int64, limit int, node int64) (Pull, error) {
	p := Pull{Changes: []Change{}, NextSeq: since}
	err := s.tx(ctx, func(tx *sql.Tx) error {
		var latest int64
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(seq), 0) FROM sync_outbox`).Scan(&latest); err != nil {
			return err
		}
		if latest <= since {
			return nil
		}
		upto, err := outboxUpto(ctx, tx, since, limit)
		if err != nil {
			return err
		}
		upto = min(upto, latest)
		changes, err := exportTx(ctx, tx, since, upto, node)
		if err != nil {
			return err
		}
		if changes != nil {
			p.Changes = changes
		}
		p.NextSeq, p.More = upto, upto < latest
		return nil
	})
	return p, err
}

// LatestSeq is the log's last seq, 0 when empty.
func (s *Store) LatestSeq(ctx context.Context) (int64, error) {
	var seq int64
	err := s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(seq), 0) FROM sync_outbox`).Scan(&seq)
	return seq, err
}

// Snapshot writes a consistent copy of the database to path, a file that must
// not exist yet, for a node to join from, and returns the log's last seq in
// it: the since of the joined node's first pull. What is the hub's alone is
// left out of the copy: its keys, leases, id blocks and log.
func (s *Store) Snapshot(ctx context.Context, path string) (int64, error) {
	if _, err := s.db.ExecContext(ctx, `VACUUM INTO ?`, path); err != nil {
		return 0, err
	}
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		return 0, err
	}
	defer db.Close()
	var seq int64
	if err := db.QueryRowContext(ctx, `SELECT COALESCE(MAX(seq), 0) FROM sync_outbox`).Scan(&seq); err != nil {
		return 0, err
	}
	_, err = db.ExecContext(ctx, `DELETE FROM sync_keys; DELETE FROM id_leases; DELETE FROM id_blocks; DELETE FROM sync_outbox`)
	if err != nil {
		return 0, err
	}
	return seq, nil
}
