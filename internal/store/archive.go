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
	// Working directories agents can be started in, offered as a dropdown:
	// this computer's (local_settings, not synced).
	Workdirs []string `json:"workdirs"`
	// SessionPrompt is a template appended to every agent turn's system
	// prompt (see app.SessionPromptData); empty sends nothing.
	SessionPrompt string `json:"session_prompt"`
	// SessionLaneID is the lane a task made for a new agent session starts
	// in; nil means the first lane.
	SessionLaneID *int64 `json:"session_lane_id"`
}

// DefaultSessionPrompt tells an agent it runs inside the app, so "add a task"
// means a task on this board even when the working directory is another repo.
const DefaultSessionPrompt = `You are running as a session of the "assistant" app: a task board the user drives you from, which you can reach through its MCP tools (create_task, get_task, search_tasks, start_session, ...). ` +
	`This session belongs to the app's project "{{.Project.Name}}" (project_id {{.Project.ID}}){{if .Task.ID}}, on task #{{.Task.ID}} "{{.Task.Title}}"{{end}}; it is session #{{.Session.ID}}. ` +
	`Your working directory ({{.Session.Workdir}}) is only where you run, not where tasks live. ` +
	`When the user asks to add, open, find, move or start a task, they mean a task in the assistant app: use its MCP tools, and for new tasks use project_id {{.Project.ID}} unless they name another project.`

func (s *Store) Settings(ctx context.Context) (Settings, error) {
	var x Settings
	var lane, sessionLane sql.NullInt64
	var dirs string
	err := s.db.QueryRowContext(ctx, `SELECT archive_lane_id, archive_after_minutes, (SELECT workdirs FROM local_settings WHERE id = 1), session_prompt, session_lane_id FROM settings WHERE id = 1`).
		Scan(&lane, &x.ArchiveAfterMinutes, &dirs, &x.SessionPrompt, &sessionLane)
	if err != nil {
		return x, err
	}
	x.ArchiveLaneID = fromNullID(lane)
	x.SessionLaneID = fromNullID(sessionLane)
	x.Workdirs = []string{}
	return x, json.Unmarshal([]byte(dirs), &x.Workdirs)
}

func (s *Store) SaveSettings(ctx context.Context, x Settings) error {
	if x.Workdirs == nil {
		x.Workdirs = []string{}
	}
	dirs, _ := json.Marshal(x.Workdirs)
	return s.tx(ctx, func(tx *sql.Tx) error {
		if err := affected(tx.ExecContext(ctx, `UPDATE local_settings SET workdirs = ? WHERE id = 1`, string(dirs))); err != nil {
			return err
		}
		return affected(tx.ExecContext(ctx, `UPDATE settings SET archive_lane_id = ?, archive_after_minutes = ?, session_prompt = ?, session_lane_id = ? WHERE id = 1`,
			x.ArchiveLaneID, x.ArchiveAfterMinutes, x.SessionPrompt, x.SessionLaneID))
	})
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
