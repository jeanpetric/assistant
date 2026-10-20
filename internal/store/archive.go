package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

// Settings are the app settings edited in the UI.
type Settings struct {
	// Tasks that stay in this lane for ArchiveAfterMinutes get archived.
	// Nil (or a non-positive duration) turns auto-archive off.
	ArchiveLaneID       *int64 `json:"archive_lane_id"`
	ArchiveAfterMinutes int    `json:"archive_after_minutes"`
	// Working directories agents can be started in, offered as a dropdown.
	Workdirs []string `json:"workdirs"`
}

func (s *Store) Settings(ctx context.Context) (Settings, error) {
	var x Settings
	var lane sql.NullInt64
	var dirs string
	err := s.db.QueryRowContext(ctx, `SELECT archive_lane_id, archive_after_minutes, workdirs FROM settings WHERE id = 1`).
		Scan(&lane, &x.ArchiveAfterMinutes, &dirs)
	if err != nil {
		return x, err
	}
	x.ArchiveLaneID = fromNullID(lane)
	x.Workdirs = []string{}
	return x, json.Unmarshal([]byte(dirs), &x.Workdirs)
}

func (s *Store) SaveSettings(ctx context.Context, x Settings) error {
	if x.Workdirs == nil {
		x.Workdirs = []string{}
	}
	dirs, _ := json.Marshal(x.Workdirs)
	return affected(s.db.ExecContext(ctx, `UPDATE settings SET archive_lane_id = ?, archive_after_minutes = ?, workdirs = ? WHERE id = 1`,
		x.ArchiveLaneID, x.ArchiveAfterMinutes, string(dirs)))
}

// ArchiveTask takes the task off the board; it keeps its lane. Archiving an
// archived task is a no-op.
func (s *Store) ArchiveTask(ctx context.Context, id int64) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		return archive(ctx, tx, id, s.ts())
	})
}

func archive(ctx context.Context, tx *sql.Tx, id, now int64) error {
	var project, lane int64
	var archived bool
	err := tx.QueryRowContext(ctx, `SELECT project_id, lane_id, archived_at IS NOT NULL FROM tasks WHERE id = ?`, id).
		Scan(&project, &lane, &archived)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil || archived {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE tasks SET archived_at = ? WHERE id = ?`, now, id); err != nil {
		return err
	}
	ids, err := laneTaskIDs(ctx, tx, project, lane, id)
	if err != nil {
		return err
	}
	return renumber(ctx, tx, ids)
}

// UnarchiveTask puts the task back at the end of its lane. Its time in the
// lane starts over, so auto-archive doesn't take it straight back.
func (s *Store) UnarchiveTask(ctx context.Context, id int64) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		var project, lane int64
		var archived bool
		err := tx.QueryRowContext(ctx, `SELECT project_id, lane_id, archived_at IS NOT NULL FROM tasks WHERE id = ?`, id).
			Scan(&project, &lane, &archived)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil || !archived {
			return err
		}
		now := s.ts()
		_, err = tx.ExecContext(ctx, `UPDATE tasks SET archived_at = NULL, position = `+nextPosition+`,
			lane_changed_at = ?, updated_at = ? WHERE id = ?`, project, lane, now, now, id)
		return err
	})
}

// ArchiveStale archives the tasks that entered laneID at or before cutoff.
// It returns their ids.
func (s *Store) ArchiveStale(ctx context.Context, laneID int64, cutoff time.Time) ([]int64, error) {
	var ids []int64
	err := s.tx(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT id FROM tasks WHERE lane_id = ? AND archived_at IS NULL AND lane_changed_at <= ?
			ORDER BY position, id`, laneID, cutoff.UnixMilli())
		if err != nil {
			return err
		}
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			ids = append(ids, id)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		now := s.ts()
		for _, id := range ids {
			if err := archive(ctx, tx, id, now); err != nil {
				return err
			}
		}
		return nil
	})
	return ids, err
}
