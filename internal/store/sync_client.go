package store

import (
	"context"
	"database/sql"
	"slices"
	"time"
)

// A node syncing with a hub pushes its outbox there and pulls the hub's log
// from pulled_seq on; these keep its side of that.

// PulledSeq is the since of the node's next pull: the hub's log is applied
// up to it.
func (s *Store) PulledSeq(ctx context.Context) (int64, error) {
	var seq int64
	err := s.db.QueryRowContext(ctx, `SELECT pulled_seq FROM sync_state WHERE id = 1`).Scan(&seq)
	return seq, err
}

// SetPulledSeq sets where the next pull starts, e.g. a joined snapshot's seq.
func (s *Store) SetPulledSeq(ctx context.Context, seq int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE sync_state SET pulled_seq = ? WHERE id = 1`, seq)
	return err
}

// ApplyPulled applies a page of the hub's log, as ApplyChanges, and moves
// the next pull to nextSeq in the same transaction: a page is applied once
// or, cut short, not at all.
func (s *Store) ApplyPulled(ctx context.Context, changes []Change, nextSeq int64) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		if _, err := applyTx(ctx, tx, changes); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `UPDATE sync_state SET pulled_seq = ? WHERE id = 1`, nextSeq)
		return err
	})
}

// SyncedAt is when the node last synced with the hub; zero if never.
func (s *Store) SyncedAt(ctx context.Context) (time.Time, error) {
	var ms int64
	if err := s.db.QueryRowContext(ctx, `SELECT synced_at FROM sync_state WHERE id = 1`).Scan(&ms); err != nil || ms == 0 {
		return time.Time{}, err
	}
	return fromMillis(ms), nil
}

// SetSyncedAt records a sync with the hub at t.
func (s *Store) SetSyncedAt(ctx context.Context, t time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE sync_state SET synced_at = ? WHERE id = 1`, t.UnixMilli())
	return err
}

// PendingChanges is how many rows have changes waiting to be pushed.
func (s *Store) PendingChanges(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM (SELECT DISTINCT tbl, pk_json FROM sync_outbox)`).Scan(&n)
	return n, err
}

// LowOnIDs lists the tables with less than a block's worth of ids left in
// their blocks: leasing one more keeps a full block in hand for offline use.
func (s *Store) LowOnIDs(ctx context.Context) ([]string, error) {
	left := map[string]int64{}
	rows, err := s.db.QueryContext(ctx, `SELECT tbl, SUM(end_id - next_id + 1) FROM id_blocks GROUP BY tbl`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var tbl string
		var n int64
		if err := rows.Scan(&tbl, &n); err != nil {
			rows.Close()
			return nil, err
		}
		left[tbl] = n
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var low []string
	for _, tbl := range idTableNames() {
		if left[tbl] < leaseSize(tbl) {
			low = append(low, tbl)
		}
	}
	return low, nil
}

// idTableNames are the idTables in name order.
func idTableNames() []string {
	names := make([]string, 0, len(idTables))
	for tbl := range idTables {
		names = append(names, tbl)
	}
	slices.Sort(names)
	return names
}
