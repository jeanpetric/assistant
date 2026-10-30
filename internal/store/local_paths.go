package store

import (
	"context"
	"database/sql"
	"errors"
)

// A workdir is a path on this computer. Projects, tasks and rules keep the
// one they were created with in their synced workdir column, the default for
// every computer; local_paths holds this computer's own, which wins. The
// Workdir they are read with is the one in effect here, WorkdirDefault the
// synced one.

// localWorkdir is the SQL for the workdir in effect of a row of kind's table
// selected unaliased or as alias, e.g. localWorkdir("task", "tasks").
func localWorkdir(kind, alias string) string {
	return `COALESCE((SELECT lp.workdir FROM local_paths lp WHERE lp.kind = '` + kind + `' AND lp.ref_id = ` + alias + `.id), ` + alias + `.workdir), ` + alias + `.workdir`
}

// setLocalWorkdir makes workdir the one in effect here for the row id of
// kind's table, leaving the synced default alone: empty drops this
// computer's own, so the default applies again. Setting the workdir already in
// effect changes nothing, so saving a row as read doesn't pin the default here.
func setLocalWorkdir(ctx context.Context, tx *sql.Tx, kind, table string, id int64, workdir string) error {
	var cur string
	err := tx.QueryRowContext(ctx, `SELECT `+localWorkdir(kind, table)+` FROM `+table+` WHERE id = ?`, id).Scan(&cur, new(string))
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil || cur == workdir {
		return err
	}
	return putLocalWorkdir(ctx, tx, kind, id, workdir)
}

// putLocalWorkdir sets this computer's own workdir for the row id of kind
// (empty: none).
func putLocalWorkdir(ctx context.Context, tx *sql.Tx, kind string, id int64, workdir string) error {
	var err error
	if workdir == "" {
		_, err = tx.ExecContext(ctx, `DELETE FROM local_paths WHERE kind = ? AND ref_id = ?`, kind, id)
	} else {
		_, err = tx.ExecContext(ctx, `INSERT INTO local_paths (kind, ref_id, workdir) VALUES (?, ?, ?)
			ON CONFLICT (kind, ref_id) DO UPDATE SET workdir = excluded.workdir`, kind, id, workdir)
	}
	return err
}
