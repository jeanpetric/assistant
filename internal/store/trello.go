package store

import (
	"context"
	"database/sql"
	"time"
)

// TrelloSettings say which Trello board to sync and where its cards go.
type TrelloSettings struct {
	Board      string `json:"board"`        // id, short link or URL; empty: sync off
	ProjectID  *int64 `json:"project_id"`   // nil: the first project
	LaneID     *int64 `json:"lane_id"`      // imported cards land here; nil: the first lane
	DoneLaneID *int64 `json:"done_lane_id"` // tasks here get their card archived; nil: never
}

func (s *Store) TrelloSettings(ctx context.Context) (TrelloSettings, error) {
	var x TrelloSettings
	var project, lane, done sql.NullInt64
	err := s.db.QueryRowContext(ctx, `SELECT trello_board, trello_project_id, trello_lane_id, trello_done_lane_id FROM settings WHERE id = 1`).
		Scan(&x.Board, &project, &lane, &done)
	x.ProjectID, x.LaneID, x.DoneLaneID = fromNullID(project), fromNullID(lane), fromNullID(done)
	return x, err
}

func (s *Store) SaveTrelloSettings(ctx context.Context, x TrelloSettings) error {
	return affected(s.db.ExecContext(ctx, `UPDATE settings SET trello_board = ?, trello_project_id = ?, trello_lane_id = ?, trello_done_lane_id = ? WHERE id = 1`,
		x.Board, x.ProjectID, x.LaneID, x.DoneLaneID))
}

// TrelloCard is a Trello card imported as a task.
type TrelloCard struct {
	CardID string
	TaskID *int64     // nil once the task is deleted; the card is not imported again
	LaneID int64      // the task's lane
	DueAt  *time.Time // the card's due date when last synced
	Closed bool       // archived on Trello by the sync
}

// TrelloCards returns every imported card by card id.
func (s *Store) TrelloCards(ctx context.Context) (map[string]TrelloCard, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT c.card_id, c.task_id, COALESCE(t.lane_id, 0), c.due_at, c.closed
		FROM trello_cards c LEFT JOIN tasks t ON t.id = c.task_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]TrelloCard{}
	for rows.Next() {
		var c TrelloCard
		var task, due sql.NullInt64
		if err := rows.Scan(&c.CardID, &task, &c.LaneID, &due, &c.Closed); err != nil {
			return nil, err
		}
		c.TaskID, c.DueAt = fromNullID(task), fromNullMillis(due)
		out[c.CardID] = c
	}
	return out, rows.Err()
}

// AddTrelloCard records that the card was imported as the task.
func (s *Store) AddTrelloCard(ctx context.Context, cardID string, taskID int64, due *time.Time) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO trello_cards (card_id, task_id, due_at) VALUES (?, ?, ?)`, cardID, taskID, toNullMillis(due))
	return err
}

// SetTrelloDue takes a new due date from the card to its task, re-arming the
// task's reminder.
func (s *Store) SetTrelloDue(ctx context.Context, cardID string, due *time.Time) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		d := toNullMillis(due)
		if _, err := tx.ExecContext(ctx, `UPDATE tasks SET due_at = ?, reminded_at = NULL, updated_at = ?
			WHERE id = (SELECT task_id FROM trello_cards WHERE card_id = ?)`, d, s.ts(), cardID); err != nil {
			return err
		}
		return affected(tx.ExecContext(ctx, `UPDATE trello_cards SET due_at = ? WHERE card_id = ?`, d, cardID))
	})
}

func (s *Store) SetTrelloClosed(ctx context.Context, cardID string, closed bool) error {
	return affected(s.db.ExecContext(ctx, `UPDATE trello_cards SET closed = ? WHERE card_id = ?`, closed, cardID))
}
