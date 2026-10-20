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

// TrelloStatus is how the last Trello sync went; in memory only.
type TrelloStatus struct {
	LastSyncAt *time.Time `json:"last_sync_at"`
	Error      string     `json:"error"`
}

func (a *App) TrelloStatus() TrelloStatus {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.trelloStatus
}

// KickTrello asks the sync loop for a sync now instead of at its next tick.
func (a *App) KickTrello() {
	select {
	case a.trelloKick <- struct{}{}:
	default:
	}
}

// SyncTrello archives the cards of tasks that reached the done lane (and
// brings back those that left it), imports the board's new open cards as
// tasks and takes changed due dates over to their tasks. It does nothing
// without a client or a board. Only the first failure in a row notifies.
func (a *App) SyncTrello(ctx context.Context) error {
	if a.Trello == nil {
		return nil
	}
	a.trelloMu.Lock()
	defer a.trelloMu.Unlock()
	set, err := a.Store.TrelloSettings(ctx)
	if err != nil || set.Board == "" {
		return err
	}
	err = a.syncTrello(ctx, set)
	now := a.Now()
	a.mu.Lock()
	failing := a.trelloStatus.Error != ""
	a.trelloStatus = TrelloStatus{LastSyncAt: &now}
	if err != nil {
		a.trelloStatus.Error = err.Error()
	}
	a.mu.Unlock()
	if err != nil && !failing && ctx.Err() == nil {
		a.Store.Notify(ctx, nil, "Trello sync failed: "+err.Error())
	}
	return err
}

func (a *App) syncTrello(ctx context.Context, set store.TrelloSettings) error {
	known, err := a.Store.TrelloCards(ctx)
	if err != nil {
		return err
	}
	var errs []error
	if set.DoneLaneID != nil {
		for _, c := range known {
			if c.TaskID == nil {
				continue
			}
			done := c.LaneID == *set.DoneLaneID
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
	cards, err := a.Trello.OpenCards(ctx, set.Board)
	if err != nil {
		return errors.Join(append(errs, err)...)
	}
	for _, card := range cards {
		c, ok := known[card.ID]
		if !ok {
			if err := a.importCard(ctx, set, card); err != nil {
				return err
			}
			continue
		}
		if c.TaskID != nil && !sameTime(c.DueAt, card.Due) {
			if err := a.Store.SetTrelloDue(ctx, card.ID, card.Due); err != nil {
				return err
			}
		}
	}
	return errors.Join(errs...)
}

func (a *App) importCard(ctx context.Context, set store.TrelloSettings, card trello.Card) error {
	t := store.Task{Title: strings.TrimSpace(card.Name), Description: card.Desc, DueAt: card.Due,
		Fields: map[string]string{"trello": card.ShortURL}}
	if t.Title == "" {
		t.Title = "(untitled Trello card)"
	}
	if set.ProjectID != nil {
		t.ProjectID = *set.ProjectID
	}
	if set.LaneID != nil {
		t.LaneID = *set.LaneID
	}
	task, err := a.Store.CreateTask(ctx, t)
	if err != nil {
		return fmt.Errorf("import %q: %w", card.Name, err)
	}
	return a.Store.AddTrelloCard(ctx, card.ID, task.ID, card.Due)
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
