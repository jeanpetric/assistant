package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// TrelloBoard is a Trello board to sync and where its cards go.
type TrelloBoard struct {
	ID         int64  `json:"id"`
	Board      string `json:"board"`        // id, short link or URL
	ProjectID  *int64 `json:"project_id"`   // nil: the first project
	LaneID     *int64 `json:"lane_id"`      // imported cards land here; nil: the first lane
	DoneLaneID *int64 `json:"done_lane_id"` // tasks here get their card archived; nil: never
}

const trelloBoardCols = `id, board, project_id, lane_id, done_lane_id`

func scanTrelloBoard(sc interface{ Scan(...any) error }) (TrelloBoard, error) {
	var b TrelloBoard
	var project, lane, done sql.NullInt64
	err := sc.Scan(&b.ID, &b.Board, &project, &lane, &done)
	b.ProjectID, b.LaneID, b.DoneLaneID = fromNullID(project), fromNullID(lane), fromNullID(done)
	return b, err
}

// TrelloBoards returns the synced boards in the order they were added.
func (s *Store) TrelloBoards(ctx context.Context) ([]TrelloBoard, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+trelloBoardCols+` FROM trello_boards ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []TrelloBoard{}
	for rows.Next() {
		b, err := scanTrelloBoard(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

func (s *Store) TrelloBoard(ctx context.Context, id int64) (TrelloBoard, error) {
	b, err := scanTrelloBoard(s.db.QueryRowContext(ctx, `SELECT `+trelloBoardCols+` FROM trello_boards WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrNotFound
	}
	return b, err
}

func (s *Store) AddTrelloBoard(ctx context.Context, b TrelloBoard) (TrelloBoard, error) {
	res, err := s.db.ExecContext(ctx, `INSERT INTO trello_boards (board, project_id, lane_id, done_lane_id) VALUES (?, ?, ?, ?)`,
		b.Board, b.ProjectID, b.LaneID, b.DoneLaneID)
	if err != nil {
		return b, err
	}
	b.ID, err = res.LastInsertId()
	return b, err
}

func (s *Store) UpdateTrelloBoard(ctx context.Context, b TrelloBoard) error {
	return affected(s.db.ExecContext(ctx, `UPDATE trello_boards SET board = ?, project_id = ?, lane_id = ?, done_lane_id = ? WHERE id = ?`,
		b.Board, b.ProjectID, b.LaneID, b.DoneLaneID, b.ID))
}

// DeleteTrelloBoard stops syncing the board. Its cards stay recorded, without
// a board, so adding the board again doesn't import them twice.
func (s *Store) DeleteTrelloBoard(ctx context.Context, id int64) error {
	return affected(s.db.ExecContext(ctx, `DELETE FROM trello_boards WHERE id = ?`, id))
}

// TrelloCard is a Trello card imported as a task.
type TrelloCard struct {
	CardID  string
	BoardID *int64     // the board it was last seen on; nil: that board was removed
	TaskID  *int64     // nil once the task is deleted; the card is not imported again
	LaneID  int64      // the task's lane
	DueAt   *time.Time // the card's due date when last synced
	Closed  bool       // archived on Trello by the sync
}

// TrelloCards returns every imported card, of all boards, by card id (card
// ids are unique across Trello).
func (s *Store) TrelloCards(ctx context.Context) (map[string]TrelloCard, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT c.card_id, c.board_id, c.task_id, COALESCE(t.lane_id, 0), c.due_at, c.closed
		FROM trello_cards c LEFT JOIN tasks t ON t.id = c.task_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]TrelloCard{}
	for rows.Next() {
		var c TrelloCard
		var board, task, due sql.NullInt64
		if err := rows.Scan(&c.CardID, &board, &task, &c.LaneID, &due, &c.Closed); err != nil {
			return nil, err
		}
		c.BoardID, c.TaskID, c.DueAt = fromNullID(board), fromNullID(task), fromNullMillis(due)
		out[c.CardID] = c
	}
	return out, rows.Err()
}

// AddTrelloCard records that the board's card was imported as the task.
func (s *Store) AddTrelloCard(ctx context.Context, boardID int64, cardID string, taskID int64, due *time.Time) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO trello_cards (card_id, board_id, task_id, due_at) VALUES (?, ?, ?, ?)`,
		cardID, boardID, taskID, toNullMillis(due))
	return err
}

// SetTrelloCardBoard moves an imported card to the board it is now seen on.
func (s *Store) SetTrelloCardBoard(ctx context.Context, cardID string, boardID int64) error {
	return affected(s.db.ExecContext(ctx, `UPDATE trello_cards SET board_id = ? WHERE card_id = ?`, boardID, cardID))
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
