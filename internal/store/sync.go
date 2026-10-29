package store

import (
	"context"
	"database/sql"
	"fmt"
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
