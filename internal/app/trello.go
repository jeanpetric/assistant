package app

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/jeanpetric/assistant/internal/store"
	"github.com/jeanpetric/assistant/internal/trello"
)

// TrelloClient is the part of the Trello API the sync uses.
type TrelloClient interface {
	OpenCards(ctx context.Context, board string) ([]trello.Card, error)
	SetClosed(ctx context.Context, cardID string, closed bool) error
}

// TrelloStatus is how the last sync of a board went; in memory only.
type TrelloStatus struct {
	LastSyncAt *time.Time `json:"last_sync_at"`
	Error      string     `json:"error"`
}

// TrelloStatus returns the status of the board's last sync.
func (a *App) TrelloStatus(boardID int64) TrelloStatus {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.trelloStatus[boardID]
}

// KickTrello asks the sync loop for a sync now instead of at its next tick.
func (a *App) KickTrello() {
	select {
	case a.trelloKick <- struct{}{}:
	default:
	}
}

// SyncTrello syncs every board: it archives the cards of tasks that reached
// the board's done lane (and brings back those that left it), imports the
// board's new open cards as tasks and takes changed due dates over to their
// tasks. It does nothing without a client. A board's failure doesn't stop
// the others; only the first failure in a row of a board notifies.
func (a *App) SyncTrello(ctx context.Context) error {
	if a.Trello == nil {
		return nil
	}
	a.trelloMu.Lock()
	defer a.trelloMu.Unlock()
	boards, err := a.Store.TrelloBoards(ctx)
	if err != nil {
		return err
	}
	var errs []error
	status := map[int64]TrelloStatus{}
	for _, b := range boards {
		err := a.syncTrello(ctx, b)
		now := a.Now()
		status[b.ID] = TrelloStatus{LastSyncAt: &now}
		if err == nil {
			continue
		}
		errs = append(errs, fmt.Errorf("board %s: %w", b.Board, err))
		status[b.ID] = TrelloStatus{LastSyncAt: &now, Error: err.Error()}
		if a.TrelloStatus(b.ID).Error == "" && ctx.Err() == nil {
			a.Store.Notify(ctx, nil, fmt.Sprintf("Trello sync of %s failed: %v", b.Board, err))
		}
	}
	a.mu.Lock()
	a.trelloStatus = status // removed boards drop out
	a.mu.Unlock()
	return errors.Join(errs...)
}

func (a *App) syncTrello(ctx context.Context, b store.TrelloBoard) error {
	known, err := a.Store.TrelloCards(ctx)
	if err != nil {
		return err
	}
	var errs []error
	if b.DoneLaneID != nil {
		for _, c := range known {
			if c.TaskID == nil || c.BoardID == nil || *c.BoardID != b.ID {
				continue
			}
			done := c.LaneID == *b.DoneLaneID
			if done == c.Closed {
				continue
			}
			// A card deleted on Trello has nothing left to archive.
			if err := a.Trello.SetClosed(ctx, c.CardID, done); err != nil && !errors.Is(err, trello.ErrNotFound) {
				errs = append(errs, err)
				continue
			}
			if err := a.Store.SetTrelloClosed(ctx, c.CardID, done); err != nil {
				return err
			}
		}
	}
	cards, err := a.Trello.OpenCards(ctx, b.Board)
	if err != nil {
		return errors.Join(append(errs, err)...)
	}
	for _, card := range cards {
		c, ok := known[card.ID]
		if !ok {
			if err := a.importCard(ctx, b, card); err != nil {
				return err
			}
			continue
		}
		// A card moved here from another board, or of a board removed and
		// added again, is this board's now; it is not imported twice.
		if c.BoardID == nil || *c.BoardID != b.ID {
			if err := a.Store.SetTrelloCardBoard(ctx, card.ID, b.ID); err != nil {
				return err
			}
		}
		if c.TaskID != nil && !sameTime(c.DueAt, card.Due) {
			if err := a.Store.SetTrelloDue(ctx, card.ID, card.Due); err != nil {
				return err
			}
		}
	}
	return errors.Join(errs...)
}

func (a *App) importCard(ctx context.Context, b store.TrelloBoard, card trello.Card) error {
	t := store.Task{Title: strings.TrimSpace(card.Name), Description: card.Desc, DueAt: card.Due,
		Fields: map[string]string{"trello": card.ShortURL}}
	if t.Title == "" {
		t.Title = "(untitled Trello card)"
	}
	t.Fields = a.WithLinkFields(ctx, t.Description, t.Fields)
	if b.ProjectID != nil {
		t.ProjectID = *b.ProjectID
	}
	if b.LaneID != nil {
		t.LaneID = *b.LaneID
	}
	task, err := a.Store.CreateTask(ctx, t)
	if err != nil {
		return fmt.Errorf("import %q: %w", card.Name, err)
	}
	return a.Store.AddTrelloCard(ctx, b.ID, card.ID, task.ID, card.Due)
}

func sameTime(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Equal(*b)
}

// RunTrelloSync syncs every interval, and when kicked, until ctx is done.
func (a *App) RunTrelloSync(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		if err := a.SyncTrello(ctx); err != nil && ctx.Err() == nil {
			log.Printf("trello: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-a.trelloKick:
		}
	}
}
