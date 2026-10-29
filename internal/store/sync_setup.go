package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// Setting a node up: the first node with data seeds the hub with all of it,
// and every other one joins from the hub's snapshot with an empty database.

// queryer is a *sql.DB or *sql.Tx.
type queryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// rowKeys are the keys (JSON arrays, as in sync_rows) of tbl's rows.
func rowKeys(ctx context.Context, q queryer, tbl string) (map[string]bool, error) {
	cols := make([]string, len(syncTables[tbl]))
	for i, c := range syncTables[tbl] {
		cols[i] = `"` + c + `"`
	}
	rows, err := q.QueryContext(ctx, `SELECT json_array(`+strings.Join(cols, ", ")+`) FROM "`+tbl+`"`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	keys := map[string]bool{}
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			return nil, err
		}
		keys[k] = true
	}
	return keys, rows.Err()
}

// freshKeys are the keys of the rows a new database starts with (default
// lanes, settings, ...), by synced table.
func freshKeys(ctx context.Context) (map[string]map[string]bool, error) {
	dir, err := os.MkdirTemp("", "assistant-fresh-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	fresh, err := Open(filepath.Join(dir, "fresh.db"))
	if err != nil {
		return nil, err
	}
	defer fresh.Close()
	keys := map[string]map[string]bool{}
	for tbl := range syncTables {
		if keys[tbl], err = rowKeys(ctx, fresh.db, tbl); err != nil {
			return nil, err
		}
	}
	return keys, nil
}

// Blank reports whether the database holds nothing but what a new one
// starts with: no row a new database lacks in any synced table. Edits to the
// default rows (a renamed lane) do not count.
func (s *Store) Blank(ctx context.Context) (bool, error) {
	fresh, err := freshKeys(ctx)
	if err != nil {
		return false, err
	}
	for tbl := range syncTables {
		keys, err := rowKeys(ctx, s.db, tbl)
		if err != nil {
			return false, err
		}
		for k := range keys {
			if !fresh[tbl][k] {
				return false, nil
			}
		}
	}
	return true, nil
}

// syncOrder lists the synced tables with each after the tables it refers
// to, so rows sent in this order never arrive before their parents.
func syncOrder(ctx context.Context, tx *sql.Tx) ([]string, error) {
	deps := map[string][]string{}
	for tbl := range syncTables {
		rows, err := tx.QueryContext(ctx, `SELECT DISTINCT "table" FROM pragma_foreign_key_list(?)`, tbl)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var parent string
			if err := rows.Scan(&parent); err != nil {
				rows.Close()
				return nil, err
			}
			if parent != tbl {
				deps[tbl] = append(deps[tbl], parent)
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	names := make([]string, 0, len(syncTables))
	for tbl := range syncTables {
		names = append(names, tbl)
	}
	slices.Sort(names)
	var order []string
	done := map[string]bool{}
	var visit func(string, int) error
	visit = func(tbl string, depth int) error {
		if done[tbl] {
			return nil
		}
		if depth > len(names) {
			return fmt.Errorf("foreign keys of %s go round in a cycle", tbl)
		}
		for _, p := range deps[tbl] {
			if err := visit(p, depth+1); err != nil {
				return err
			}
		}
		done[tbl] = true
		order = append(order, tbl)
		return nil
	}
	for _, tbl := range names {
		if err := visit(tbl, 0); err != nil {
			return nil, err
		}
	}
	return order, nil
}

// StartSeed makes this database node's and queues all of it to be pushed to
// a blank hub: every row of the synced tables, parents first, each stamped
// now as node's change, and a deletion for each row a new database starts
// with that this one no longer has, so the hub's own defaults go the same
// way. It turns change capture on and drops id blocks the hub did not give.
// It returns how many rows are queued.
func (s *Store) StartSeed(ctx context.Context, node int64) (int, error) {
	if node < 2 {
		return 0, fmt.Errorf("bad node %d: the hub is node 1", node)
	}
	fresh, err := freshKeys(ctx)
	if err != nil {
		return 0, err
	}
	var n int
	err = s.tx(ctx, func(tx *sql.Tx) error {
		order, err := syncOrder(ctx, tx)
		if err != nil {
			return err
		}
		for _, q := range []string{
			`DELETE FROM sync_rows`, `DELETE FROM sync_outbox`, `DELETE FROM id_blocks`,
			`UPDATE sync_state SET enabled = 1, applying = 0, pulled_seq = 0, synced_at = 0 WHERE id = 1`,
		} {
			if _, err := tx.ExecContext(ctx, q); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, `UPDATE sync_node SET node_id = ? WHERE id = 1`, node); err != nil {
			return err
		}
		now := s.ts()
		record := func(tbl, key string, deleted bool) error {
			_, err := tx.ExecContext(ctx, `INSERT INTO sync_rows (tbl, pk_json, changed_at_ms, node_id, deleted) VALUES (?, ?, ?, ?, ?)`,
				tbl, key, now, node, deleted)
			if err != nil {
				return err
			}
			op := map[bool]string{false: "insert", true: "delete"}[deleted]
			_, err = tx.ExecContext(ctx, `INSERT INTO sync_outbox (tbl, pk_json, op, changed_at_ms, node_id) VALUES (?, ?, ?, ?, ?)`,
				tbl, key, op, now, node)
			n++
			return err
		}
		// Deletions first: the hub's default rows they remove would
		// otherwise clash with renamed ones of this database by name.
		for _, tbl := range order {
			keys, err := rowKeys(ctx, tx, tbl)
			if err != nil {
				return err
			}
			gone := make([]string, 0, len(fresh[tbl]))
			for k := range fresh[tbl] {
				if !keys[k] {
					gone = append(gone, k)
				}
			}
			slices.Sort(gone)
			for _, k := range gone {
				if err := record(tbl, k, true); err != nil {
					return err
				}
			}
		}
		for _, tbl := range order {
			cols := make([]string, len(syncTables[tbl]))
			for i, c := range syncTables[tbl] {
				cols[i] = `"` + c + `"`
			}
			key := `json_array(` + strings.Join(cols, ", ") + `)`
			// In key order, so a session comes after its parent session.
			res, err := tx.ExecContext(ctx, `INSERT INTO sync_rows (tbl, pk_json, changed_at_ms, node_id, deleted)
				SELECT ?, `+key+`, ?, ?, 0 FROM "`+tbl+`" ORDER BY `+strings.Join(cols, ", "), tbl, now, node)
			if err != nil {
				return err
			}
			added, _ := res.RowsAffected()
			n += int(added)
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO sync_outbox (tbl, pk_json, op, changed_at_ms, node_id)
			SELECT tbl, pk_json, 'insert', changed_at_ms, node_id FROM sync_rows WHERE NOT deleted ORDER BY rowid`)
		return err
	})
	return n, err
}

// ErrNotBlank: a node joins only with an empty database.
var ErrNotBlank = errors.New("the database has data of its own")

// Join replaces every synced row of this database, which must be blank,
// with those of the hub's snapshot at path, and makes it node node pulling
// the hub's log from seq on, with change capture on. Local tables stay, but
// for id blocks the hub did not give and an empty outbox. It is one
// transaction: the app may be running, and a failed join leaves the
// database as it was. The snapshot must have this build's schema.
func (s *Store) Join(ctx context.Context, path string, node, seq int64) error {
	if node < 2 {
		return fmt.Errorf("bad node %d: the hub is node 1", node)
	}
	if blank, err := s.Blank(ctx); err != nil {
		return err
	} else if !blank {
		return ErrNotBlank
	}
	// ATTACH holds for a connection: keep to one, the store's only.
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `ATTACH DATABASE ? AS snap`, path); err != nil {
		return err
	}
	defer conn.ExecContext(context.Background(), `DETACH DATABASE snap`)
	var version int
	if err := conn.QueryRowContext(ctx, `PRAGMA snap.user_version`).Scan(&version); err != nil {
		return err
	}
	if version != len(migrations) {
		return fmt.Errorf("the hub's snapshot has schema %d, this build %d: run the same version on both", version, len(migrations))
	}
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, q := range []string{`PRAGMA defer_foreign_keys = ON`, `UPDATE sync_state SET applying = 1 WHERE id = 1`} {
		if _, err := tx.ExecContext(ctx, q); err != nil {
			return err
		}
	}
	order, err := syncOrder(ctx, tx)
	if err != nil {
		return err
	}
	// Children first, so no delete cascades into a row already copied.
	for _, tbl := range slices.Backward(order) {
		if _, err := tx.ExecContext(ctx, `DELETE FROM main."`+tbl+`"`); err != nil {
			return err
		}
	}
	for _, tbl := range order {
		cols, err := sharedColumns(ctx, tx, tbl)
		if err != nil {
			return err
		}
		list := `"` + strings.Join(cols, `", "`) + `"`
		if _, err := tx.ExecContext(ctx, `INSERT INTO main."`+tbl+`" (`+list+`) SELECT `+list+` FROM snap."`+tbl+`"`); err != nil {
			return fmt.Errorf("%s: %w", tbl, err)
		}
	}
	for _, q := range []string{
		`DELETE FROM main.sync_rows`,
		`INSERT INTO main.sync_rows (tbl, pk_json, changed_at_ms, node_id, deleted)
			SELECT tbl, pk_json, changed_at_ms, node_id, deleted FROM snap.sync_rows`,
		`DELETE FROM main.sync_outbox`,
		`DELETE FROM main.id_blocks`,
	} {
		if _, err := tx.ExecContext(ctx, q); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE main.sync_node SET node_id = ? WHERE id = 1`, node); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE main.sync_state SET enabled = 1, applying = 0, pulled_seq = ?, synced_at = 0 WHERE id = 1`, seq); err != nil {
		return err
	}
	return tx.Commit()
}

// sharedColumns are the columns tbl has both here and in the attached
// snapshot. They may differ at one schema version: databases that went
// through an early build lack the unused usage columns of sessions.
func sharedColumns(ctx context.Context, tx *sql.Tx, tbl string) ([]string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT m.name FROM pragma_table_info(?1, 'main') m
		JOIN pragma_table_info(?1, 'snap') s ON s.name = m.name ORDER BY m.cid`, tbl)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var cols []string
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			return nil, err
		}
		cols = append(cols, c)
	}
	return cols, rows.Err()
}

// Backup writes a consistent, integrity-checked copy of the database to
// path, a file that must not exist yet, as make backup does: a single file
// with a rollback journal. A copy that fails the check is removed.
func (s *Store) Backup(ctx context.Context, path string) error {
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("%s already exists", path)
	}
	partial := path + ".partial"
	if _, err := s.db.ExecContext(ctx, `VACUUM INTO ?`, partial); err != nil {
		os.Remove(partial)
		return err
	}
	err := func() error {
		db, err := sql.Open("sqlite", "file:"+partial)
		if err != nil {
			return err
		}
		defer db.Close()
		if _, err := db.ExecContext(ctx, `PRAGMA journal_mode = DELETE`); err != nil {
			return err
		}
		var check string
		if err := db.QueryRowContext(ctx, `PRAGMA integrity_check`).Scan(&check); err != nil {
			return err
		}
		if check != "ok" {
			return fmt.Errorf("integrity check failed, no backup written: %s", check)
		}
		return nil
	}()
	if err != nil {
		os.Remove(partial)
		return err
	}
	return os.Rename(partial, path)
}

// SyncDump is every synced row, a line each in table and key order: two
// nodes in sync have the same.
func (s *Store) SyncDump(ctx context.Context) (string, error) {
	tables := make([]string, 0, len(syncTables))
	for tbl := range syncTables {
		tables = append(tables, tbl)
	}
	slices.Sort(tables)
	var b strings.Builder
	err := s.tx(ctx, func(tx *sql.Tx) error {
		for _, tbl := range tables {
			cols, err := tableColumns(ctx, tx, tbl)
			if err != nil {
				return err
			}
			pairs := make([]string, len(cols))
			for i, c := range cols {
				pairs[i] = fmt.Sprintf(`'%s', "%s"`, c, c)
			}
			rows, err := tx.QueryContext(ctx, `SELECT json_object(`+strings.Join(pairs, ", ")+`) FROM "`+tbl+`" ORDER BY "`+
				strings.Join(syncTables[tbl], `", "`)+`"`)
			if err != nil {
				return err
			}
			for rows.Next() {
				var row string
				if err := rows.Scan(&row); err != nil {
					rows.Close()
					return err
				}
				b.WriteString(tbl + " " + row + "\n")
			}
			rows.Close()
			if err := rows.Err(); err != nil {
				return err
			}
		}
		return nil
	})
	return b.String(), err
}
