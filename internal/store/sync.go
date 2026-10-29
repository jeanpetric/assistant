package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"sort"
	"strings"
)

// syncTables are the tables shared between nodes, each with its primary key
// columns: a row is named by its table and the JSON array of its key values.
var syncTables = map[string][]string{
	"lanes":             {"id"},
	"tasks":             {"id"},
	"field_defs":        {"key"},
	"task_fields":       {"task_id", "key"},
	"sessions":          {"id"},
	"messages":          {"id"},
	"transition_rules":  {"id"},
	"schedules":         {"id"},
	"notifications":     {"id"},
	"settings":          {"id"},
	"projects":          {"id"},
	"task_context":      {"task_id", "source_id"},
	"skills":            {"id"},
	"skill_automations": {"id"},
	"artifacts":         {"id"},
	"memories":          {"id"},
	"memory_revisions":  {"id"},
	"task_memories":     {"task_id", "memory_id"},
	"trello_cards":      {"card_id"},
	"trello_boards":     {"id"},
}

// localTables stay on their node. Every table is in exactly one of
// syncTables and localTables.
var localTables = map[string]bool{
	"sync_node":   true, // which node this database is
	"id_blocks":   true, // ids leased to this node
	"sync_state":  true,
	"sync_outbox": true,
	"sync_rows":   true,
	"sync_keys":   true, // keys a hub accepts
	"id_leases":   true, // id blocks a hub gave out
	// Messages wait for the agent process, which runs on the session's node.
	"queued_messages": true,
}

// SetSyncEnabled turns change capture on or off. Off (the default), changes
// leave no trace in sync_outbox and sync_rows.
func (s *Store) SetSyncEnabled(ctx context.Context, on bool) error {
	_, err := s.db.ExecContext(ctx, `UPDATE sync_state SET enabled = ? WHERE id = 1`, boolInt(on))
	return err
}

// syncTriggerSQL creates the triggers that record every insert, update and
// delete on the synced tables while sync is enabled and no remote changes are
// being applied: the row's latest change in sync_rows (a tombstone once
// deleted), and an entry in sync_outbox to push. A local change is stamped
// later than the one it replaces, whatever the clocks, so it wins.
func syncTriggerSQL() string {
	tables := make([]string, 0, len(syncTables))
	for tbl := range syncTables {
		tables = append(tables, tbl)
	}
	sort.Strings(tables)
	var b strings.Builder
	for _, tbl := range tables {
		cols := syncTables[tbl]
		pk := func(row string) string {
			vals := make([]string, len(cols))
			for i, c := range cols {
				vals[i] = row + "." + c
			}
			return "json_array(" + strings.Join(vals, ", ") + ")"
		}
		record := func(row, op string) string {
			deleted := boolInt(op == "delete")
			return fmt.Sprintf(`
	INSERT INTO sync_rows (tbl, pk_json, changed_at_ms, node_id, deleted)
		SELECT '%[1]s', %[2]s, CAST(unixepoch('subsec') * 1000 AS INTEGER), node_id, %[3]d FROM sync_node WHERE id = 1
		ON CONFLICT (tbl, pk_json) DO UPDATE SET changed_at_ms = MAX(excluded.changed_at_ms, sync_rows.changed_at_ms + 1),
			node_id = excluded.node_id, deleted = excluded.deleted;
	INSERT INTO sync_outbox (tbl, pk_json, op, changed_at_ms, node_id)
		SELECT tbl, pk_json, '%[4]s', changed_at_ms, node_id FROM sync_rows WHERE tbl = '%[1]s' AND pk_json = %[2]s;`,
				tbl, pk(row), deleted, op)
		}
		const when = `(SELECT enabled AND NOT applying FROM sync_state WHERE id = 1)`
		fmt.Fprintf(&b, "CREATE TRIGGER sync_%[1]s_insert AFTER INSERT ON %[1]s WHEN %[2]s BEGIN%[3]s\nEND;\n",
			tbl, when, record("NEW", "insert"))
		fmt.Fprintf(&b, "CREATE TRIGGER sync_%[1]s_update AFTER UPDATE ON %[1]s WHEN %[2]s BEGIN%[3]s\nEND;\n",
			tbl, when, record("NEW", "update"))
		// A changed key leaves the row under its old key deleted.
		fmt.Fprintf(&b, "CREATE TRIGGER sync_%[1]s_rekey AFTER UPDATE ON %[1]s WHEN %[2]s AND %[3]s IS NOT %[4]s BEGIN%[5]s\nEND;\n",
			tbl, when, pk("OLD"), pk("NEW"), record("OLD", "delete"))
		fmt.Fprintf(&b, "CREATE TRIGGER sync_%[1]s_delete AFTER DELETE ON %[1]s WHEN %[2]s BEGIN%[3]s\nEND;\n",
			tbl, when, record("OLD", "delete"))
	}
	return b.String()
}

// dropSyncTriggers removes the triggers syncTriggerSQL made.
func dropSyncTriggers(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, `SELECT name FROM sqlite_master WHERE type = 'trigger' AND name LIKE 'sync\_%' ESCAPE '\'`)
	if err != nil {
		return err
	}
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			return err
		}
		names = append(names, name)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, name := range names {
		if _, err := tx.ExecContext(ctx, `DROP TRIGGER "`+name+`"`); err != nil {
			return err
		}
	}
	return nil
}

// installSyncTriggers replaces the sync triggers with ones for the current
// tables and columns. They are not part of the migrations: made afresh on
// every open, they follow table rebuilds and newly synced tables.
func (s *Store) installSyncTriggers(ctx context.Context) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		if err := dropSyncTriggers(ctx, tx); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, syncTriggerSQL())
		return err
	})
}

// Change is a synced row's latest change as one node tells another: the
// row's content, or its deletion.
type Change struct {
	Seq         int64           `json:"seq"` // the last outbox entry it covers, for AckOutbox
	Table       string          `json:"table"`
	Key         json.RawMessage `json:"key"` // the key columns' values, a JSON array
	ChangedAtMs int64           `json:"changed_at_ms"`
	NodeID      int64           `json:"node_id"`
	Deleted     bool            `json:"deleted,omitempty"`
	Row         json.RawMessage `json:"row,omitempty"` // column name → value; none when deleted
}

// later reports whether stamp a is after stamp b: by time, then node.
func later(aMs, aNode, bMs, bNode int64) bool {
	return aMs > bMs || aMs == bMs && aNode > bNode
}

// tableColumns lists tbl's columns in order.
func tableColumns(ctx context.Context, tx *sql.Tx, tbl string) ([]string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT name FROM pragma_table_info(?) ORDER BY cid`, tbl)
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

// keyWhere matches tbl's row by the JSON array of key values in parameter ?1.
func keyWhere(tbl string) string {
	conds := make([]string, len(syncTables[tbl]))
	for i, c := range syncTables[tbl] {
		conds[i] = fmt.Sprintf(`"%s" = json_extract(?1, '$[%d]')`, c, i)
	}
	return strings.Join(conds, " AND ")
}

// ExportChanges returns the rows changed by the outbox entries after seq,
// each once with its latest change and present content, in the order of
// their last entry. limit (0: all) caps the outbox entries covered.
func (s *Store) ExportChanges(ctx context.Context, since int64, limit int) ([]Change, error) {
	var changes []Change
	err := s.tx(ctx, func(tx *sql.Tx) error {
		upto, err := outboxUpto(ctx, tx, since, limit)
		if err != nil {
			return err
		}
		changes, err = exportTx(ctx, tx, since, upto, 0)
		return err
	})
	return changes, err
}

// outboxUpto is the last outbox entry of the limit (0: all) after since.
func outboxUpto(ctx context.Context, tx *sql.Tx, since int64, limit int) (int64, error) {
	upto := int64(math.MaxInt64)
	if limit > 0 {
		err := tx.QueryRowContext(ctx, `SELECT seq FROM sync_outbox WHERE seq > ? ORDER BY seq LIMIT 1 OFFSET ?`,
			since, limit-1).Scan(&upto)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return 0, err
		}
	}
	return upto, nil
}

// exportTx is ExportChanges for the outbox entries since < seq <= upto,
// leaving out the rows whose last entry came from node except (0: none).
func exportTx(ctx context.Context, tx *sql.Tx, since, upto, except int64) ([]Change, error) {
	var changes []Change
	// With MAX, SQLite takes o.node_id from the row with the highest seq.
	rows, err := tx.QueryContext(ctx, `SELECT MAX(o.seq), o.node_id, o.tbl, o.pk_json, r.changed_at_ms, r.node_id, r.deleted
		FROM sync_outbox o JOIN sync_rows r ON r.tbl = o.tbl AND r.pk_json = o.pk_json
		WHERE o.seq > ? AND o.seq <= ? GROUP BY o.tbl, o.pk_json ORDER BY 1`, since, upto)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var c Change
		var key string
		var from int64
		if err := rows.Scan(&c.Seq, &from, &c.Table, &key, &c.ChangedAtMs, &c.NodeID, &c.Deleted); err != nil {
			rows.Close()
			return nil, err
		}
		if except != 0 && from == except {
			continue
		}
		c.Key = json.RawMessage(key)
		changes = append(changes, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	objects := map[string]string{} // table → json_object(...) of its columns
	for i := range changes {
		c := &changes[i]
		if c.Deleted {
			continue
		}
		obj, ok := objects[c.Table]
		if !ok {
			cols, err := tableColumns(ctx, tx, c.Table)
			if err != nil {
				return nil, err
			}
			pairs := make([]string, len(cols))
			for j, col := range cols {
				pairs[j] = fmt.Sprintf(`'%s', "%s"`, col, col)
			}
			obj = "json_object(" + strings.Join(pairs, ", ") + ")"
			objects[c.Table] = obj
		}
		var row string
		err := tx.QueryRowContext(ctx, `SELECT `+obj+` FROM "`+c.Table+`" WHERE `+keyWhere(c.Table), string(c.Key)).Scan(&row)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			// Gone without a trace (deleted while sync was off).
			c.Deleted = true
		case err != nil:
			return nil, err
		default:
			c.Row = json.RawMessage(row)
		}
	}
	return changes, nil
}

// AckOutbox drops the outbox entries up to and including seq: they are
// pushed.
func (s *Store) AckOutbox(ctx context.Context, seq int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM sync_outbox WHERE seq <= ?`, seq)
	return err
}

// ApplyChanges applies another node's changes in one transaction, without
// recording them. The later change to a row wins, by time and then node, and
// a row with an id (never reused) stays deleted once deleted on any node.
// Applying a change twice changes nothing. Foreign keys are checked at the
// end, so a row may come before the row it refers to; a row left referring
// to a deleted one is put right as that delete would have: deleted with it,
// its reference cleared, or (tasks) moved to the first lane or project. A
// row whose unique name another row already has is renamed. Put-right rows
// and renames are recorded as this node's own changes.
func (s *Store) ApplyChanges(ctx context.Context, changes []Change) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		_, err := applyTx(ctx, tx, changes)
		return err
	})
}

// applyTx is ApplyChanges in tx; it returns the changed rows (table, key)
// in the order of changes.
func applyTx(ctx context.Context, tx *sql.Tx, changes []Change) ([][2]string, error) {
	if _, err := tx.ExecContext(ctx, `PRAGMA defer_foreign_keys = ON`); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE sync_state SET applying = 1 WHERE id = 1`); err != nil {
		return nil, err
	}
	a := applier{ctx: ctx, tx: tx, columns: map[string]map[string]bool{}, check: map[string]bool{}}
	for _, c := range changes {
		if err := a.apply(c); err != nil {
			return nil, fmt.Errorf("%s %s: %w", c.Table, c.Key, err)
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE sync_state SET applying = 0 WHERE id = 1`); err != nil {
		return nil, err
	}
	for _, r := range a.renamed {
		if _, err := tx.ExecContext(ctx, `UPDATE "`+r.tbl+`" SET "`+r.col+`" = "`+r.col+`" WHERE `+keyWhere(r.tbl), r.key); err != nil {
			return nil, err
		}
	}
	return a.changed, a.fixReferences()
}

type renamed struct{ tbl, col, key string }

type applier struct {
	ctx     context.Context
	tx      *sql.Tx
	columns map[string]map[string]bool // table → its columns
	check   map[string]bool            // tables whose references may be broken
	renamed []renamed
	changed [][2]string // rows the changes changed: table, key
}

func (a *applier) apply(c Change) error {
	if _, ok := syncTables[c.Table]; !ok {
		return errors.New("not a synced table")
	}
	var key string
	var n int
	if err := a.tx.QueryRowContext(a.ctx, `SELECT json(?1), json_array_length(?1)`, string(c.Key)).Scan(&key, &n); err != nil {
		return fmt.Errorf("key: %w", err)
	}
	if n != len(syncTables[c.Table]) {
		return fmt.Errorf("key has %d values, want %d", n, len(syncTables[c.Table]))
	}
	var ms, node int64
	var deleted, known bool
	err := a.tx.QueryRowContext(a.ctx, `SELECT changed_at_ms, node_id, deleted FROM sync_rows WHERE tbl = ? AND pk_json = ?`,
		c.Table, key).Scan(&ms, &node, &deleted)
	switch {
	case err == nil:
		known = true
	case !errors.Is(err, sql.ErrNoRows):
		return err
	}
	newer := !known || later(c.ChangedAtMs, c.NodeID, ms, node)
	final := idTables[c.Table] // an id is never reused: its deletion is final
	switch {
	case known && deleted && final:
		return nil
	case c.Deleted && final:
	case !newer:
		return nil
	}
	if newer {
		ms, node = c.ChangedAtMs, c.NodeID
	}
	if c.Deleted {
		if _, err := a.tx.ExecContext(a.ctx, `DELETE FROM "`+c.Table+`" WHERE `+keyWhere(c.Table), key); err != nil {
			return err
		}
		// Rows referring to it without ON DELETE action are left broken.
		if err := a.checkReferrers(c.Table); err != nil {
			return err
		}
	} else if err := a.upsert(c.Table, key, c.Row); err != nil {
		return err
	}
	_, err = a.tx.ExecContext(a.ctx, `INSERT INTO sync_rows (tbl, pk_json, changed_at_ms, node_id, deleted) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (tbl, pk_json) DO UPDATE SET changed_at_ms = excluded.changed_at_ms, node_id = excluded.node_id, deleted = excluded.deleted`,
		c.Table, key, ms, node, c.Deleted)
	if err == nil {
		a.changed = append(a.changed, [2]string{c.Table, key})
	}
	return err
}

// upsert writes row (JSON) as tbl's row with key: its columns this table
// has, the others left as they are or at their defaults.
func (a *applier) upsert(tbl, key string, row json.RawMessage) error {
	have, ok := a.columns[tbl]
	if !ok {
		cols, err := tableColumns(a.ctx, a.tx, tbl)
		if err != nil {
			return err
		}
		have = map[string]bool{}
		for _, c := range cols {
			have[c] = true
		}
		a.columns[tbl] = have
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(row, &fields); err != nil {
		return fmt.Errorf("row: %w", err)
	}
	keyCols := syncTables[tbl]
	var cols, vals, sets []string
	for col := range fields {
		if have[col] {
			cols = append(cols, col)
		}
	}
	sort.Strings(cols)
	for _, col := range cols {
		vals = append(vals, fmt.Sprintf(`json_extract(?1, '$."%s"')`, col))
		if !slices.Contains(keyCols, col) {
			sets = append(sets, fmt.Sprintf(`"%[1]s" = excluded."%[1]s"`, col))
		}
	}
	for _, k := range keyCols {
		if !slices.Contains(cols, k) {
			return fmt.Errorf("row has no %s", k)
		}
	}
	update := "NOTHING"
	if len(sets) > 0 {
		update = "UPDATE SET " + strings.Join(sets, ", ")
	}
	q := fmt.Sprintf(`INSERT INTO "%s" ("%s") SELECT %s WHERE true ON CONFLICT ("%s") DO %s`,
		tbl, strings.Join(cols, `", "`), strings.Join(vals, ", "), strings.Join(keyCols, `", "`), update)
	r := string(row)
	for try := 2; ; try++ {
		_, err := a.tx.ExecContext(a.ctx, q, r)
		if err == nil {
			break
		}
		// Another row has this one's unique name: the one with the higher
		// id, the same on every node, gets a suffix.
		col, ok := strings.CutPrefix(uniqueFailure(err), tbl+".")
		if !ok || !have[col] || slices.Contains(keyCols, col) || try > 100 {
			return err
		}
		if idTables[tbl] {
			var other, id int64
			err := a.tx.QueryRowContext(a.ctx, `SELECT (SELECT id FROM "`+tbl+`" WHERE "`+col+`" = json_extract(?1, '$."`+col+`"')),
				json_extract(?2, '$[0]')`, r, key).Scan(&other, &id)
			if err != nil {
				return err
			}
			if other > id {
				if err := a.rename(tbl, col, other); err != nil {
					return err
				}
				continue
			}
		}
		if err := a.tx.QueryRowContext(a.ctx, `SELECT json_set(?1, '$."`+col+`"', json_extract(?1, '$."`+col+`"') || ?2)`,
			string(row), fmt.Sprintf(" (%d)", try)).Scan(&r); err != nil {
			return err
		}
		a.renamed = append(a.renamed, renamed{tbl, col, key})
	}
	a.check[tbl] = true
	return nil
}

// rename gives tbl's row id a suffix to col that makes it unique.
func (a *applier) rename(tbl, col string, id int64) error {
	for try := 2; ; try++ {
		_, err := a.tx.ExecContext(a.ctx, `UPDATE "`+tbl+`" SET "`+col+`" = "`+col+`" || ? WHERE id = ?`, fmt.Sprintf(" (%d)", try), id)
		if err == nil {
			a.renamed = append(a.renamed, renamed{tbl, col, fmt.Sprintf("[%d]", id)})
			return nil
		}
		if uniqueFailure(err) == "" || try > 100 {
			return err
		}
	}
}

// uniqueFailure is "table.column" when err is a unique constraint failing
// on one column.
func uniqueFailure(err error) string {
	_, rest, ok := strings.Cut(err.Error(), "UNIQUE constraint failed: ")
	if !ok {
		return ""
	}
	name, _, _ := strings.Cut(rest, " ")
	if strings.Contains(name, ",") {
		return ""
	}
	return name
}

// checkReferrers marks the tables referring to tbl for fixReferences.
func (a *applier) checkReferrers(tbl string) error {
	rows, err := a.tx.QueryContext(a.ctx, `SELECT m.name FROM sqlite_master m, pragma_foreign_key_list(m.name) f
		WHERE m.type = 'table' AND f."table" = ?`, tbl)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return err
		}
		a.check[name] = true
	}
	return rows.Err()
}

// brokenRef is a row referring to a row that is not there.
type brokenRef struct {
	tbl      string
	rowid    int64
	col      string // the referring column
	parent   string
	onDelete string
}

// fixReferences puts right the rows referring to missing rows, as recorded
// changes, until there are none.
func (a *applier) fixReferences() error {
	for round := 0; len(a.check) > 0; round++ {
		if round > 50 {
			return errors.New("references keep breaking")
		}
		tables := make([]string, 0, len(a.check))
		for tbl := range a.check {
			tables = append(tables, tbl)
		}
		sort.Strings(tables)
		a.check = map[string]bool{}
		var broken []brokenRef
		for _, tbl := range tables {
			rows, err := a.tx.QueryContext(a.ctx, `SELECT c.rowid, f."from", f."table", f.on_delete
				FROM pragma_foreign_key_check(?) c JOIN pragma_foreign_key_list(?) f ON f.id = c.fkid`, tbl, tbl)
			if err != nil {
				return err
			}
			for rows.Next() {
				b := brokenRef{tbl: tbl}
				if err := rows.Scan(&b.rowid, &b.col, &b.parent, &b.onDelete); err != nil {
					rows.Close()
					return err
				}
				broken = append(broken, b)
			}
			rows.Close()
			if err := rows.Err(); err != nil {
				return err
			}
		}
		for _, b := range broken {
			var err error
			switch b.onDelete {
			case "CASCADE":
				_, err = a.tx.ExecContext(a.ctx, `DELETE FROM "`+b.tbl+`" WHERE rowid = ?`, b.rowid)
				if err == nil {
					err = a.checkReferrers(b.tbl)
				}
			case "SET NULL":
				_, err = a.tx.ExecContext(a.ctx, `UPDATE "`+b.tbl+`" SET "`+b.col+`" = NULL WHERE rowid = ?`, b.rowid)
			default:
				// A required reference (a task's lane or project): the
				// first one left, which is also where DeleteLane moves
				// archived tasks.
				var res sql.Result
				res, err = a.tx.ExecContext(a.ctx, `UPDATE "`+b.tbl+`" SET "`+b.col+`" = (SELECT id FROM "`+b.parent+
					`" ORDER BY position, id LIMIT 1) WHERE rowid = ? AND EXISTS (SELECT 1 FROM "`+b.parent+`")`, b.rowid)
				if err == nil {
					if n, _ := res.RowsAffected(); n == 0 {
						err = fmt.Errorf("%s %d refers to a missing %s, and none is left", b.tbl, b.rowid, b.parent)
					}
				}
			}
			if err != nil {
				return err
			}
		}
	}
	return nil
}
