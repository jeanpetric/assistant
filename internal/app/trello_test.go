package app

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jeanpetric/assistant/internal/store"
	"github.com/jeanpetric/assistant/internal/trello"
)

type fakeTrello struct {
	mu      sync.Mutex
	cards   []trello.Card
	closed  map[string]bool // archived by SetClosed
	err     error           // OpenCards fails with it
	missing map[string]bool // SetClosed: card gone
}

func (f *fakeTrello) OpenCards(ctx context.Context, board string) ([]trello.Card, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.cards, f.err
}

func (f *fakeTrello) SetClosed(ctx context.Context, id string, closed bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.missing[id] {
		return fmt.Errorf("card %s: %w", id, trello.ErrNotFound)
	}
	f.closed[id] = closed
	return nil
}

func newTrelloApp(t *testing.T) (*App, *fakeTrello, map[string]int64) {
	t.Helper()
	a, _ := newApp(t)
	f := &fakeTrello{closed: map[string]bool{}, missing: map[string]bool{}}
	a.Trello = f
	lanes := lanesByName(t, a)
	done := lanes["Done"]
	todo := lanes["Todo"]
	if err := a.Store.SaveTrelloSettings(context.Background(), store.TrelloSettings{Board: "b", LaneID: &todo, DoneLaneID: &done}); err != nil {
		t.Fatal(err)
	}
	return a, f, lanes
}

func taskByTitle(t *testing.T, a *App, title string) store.Task {
	t.Helper()
	tasks, _ := a.Store.Tasks(context.Background(), 0)
	var found []store.Task
	for _, x := range tasks {
		if x.Title == title {
			found = append(found, x)
		}
	}
	if len(found) != 1 {
		t.Fatalf("tasks titled %q: %d", title, len(found))
	}
	x, _ := a.Store.Task(context.Background(), found[0].ID)
	return x
}

func TestSyncTrelloImportsCardsOnce(t *testing.T) {
	a, f, lanes := newTrelloApp(t)
	ctx := context.Background()
	due := time.Date(2026, 10, 25, 10, 0, 0, 0, time.UTC)
	f.cards = []trello.Card{
		{ID: "c1", Name: "Buy milk", Desc: "2 litres", ShortURL: "https://trello.com/c/1", Due: &due},
		{ID: "c2", Name: "  ", ShortURL: "https://trello.com/c/2"},
	}
	for range 2 {
		if err := a.SyncTrello(ctx); err != nil {
			t.Fatal(err)
		}
	}
	milk := taskByTitle(t, a, "Buy milk")
	if milk.Description != "2 litres" || !milk.DueAt.Equal(due) || milk.LaneID != lanes["Todo"] || milk.Fields["trello"] != "https://trello.com/c/1" {
		t.Fatalf("task = %+v", milk)
	}
	taskByTitle(t, a, "(untitled Trello card)")
	if st := a.TrelloStatus(); st.LastSyncAt == nil || st.Error != "" {
		t.Fatalf("status = %+v", st)
	}

	// A task deleted here stays deleted.
	a.Store.DeleteTask(ctx, milk.ID)
	a.SyncTrello(ctx)
	if tasks, _ := a.Store.Tasks(ctx, 0); len(tasks) != 1 {
		t.Fatalf("tasks = %+v", tasks)
	}
}

func TestSyncTrelloTakesNewDueDates(t *testing.T) {
	a, f, _ := newTrelloApp(t)
	ctx := context.Background()
	due := time.Date(2026, 10, 25, 10, 0, 0, 0, time.UTC)
	f.cards = []trello.Card{{ID: "c1", Name: "Report", Due: &due}}
	a.SyncTrello(ctx)
	task := taskByTitle(t, a, "Report")

	// A due date changed here survives syncs while the card's stays the same.
	mine := due.Add(48 * time.Hour)
	task.DueAt = &mine
	a.Store.UpdateTask(ctx, task)
	a.SyncTrello(ctx)
	if got := taskByTitle(t, a, "Report"); !got.DueAt.Equal(mine) {
		t.Fatalf("due = %v, want local %v", got.DueAt, mine)
	}

	// A new one on Trello wins, and a removed one clears it.
	moved := due.Add(time.Hour)
	f.cards[0].Due = &moved
	a.SyncTrello(ctx)
	if got := taskByTitle(t, a, "Report"); !got.DueAt.Equal(moved) {
		t.Fatalf("due = %v, want %v", got.DueAt, moved)
	}
	f.cards[0].Due = nil
	a.SyncTrello(ctx)
	if got := taskByTitle(t, a, "Report"); got.DueAt != nil {
		t.Fatalf("due = %v, want none", got.DueAt)
	}
}

func TestSyncTrelloArchivesDoneCards(t *testing.T) {
	a, f, lanes := newTrelloApp(t)
	ctx := context.Background()
	f.cards = []trello.Card{{ID: "c1", Name: "Ship it"}, {ID: "c2", Name: "Gone"}}
	a.SyncTrello(ctx)
	task := taskByTitle(t, a, "Ship it")

	if _, err := a.MoveTask(ctx, task.ID, lanes["Done"], -1); err != nil {
		t.Fatal(err)
	}
	select { // moving a task asks for a sync
	case <-a.trelloKick:
	default:
		t.Fatal("no kick")
	}
	a.SyncTrello(ctx)
	if !f.closed["c1"] {
		t.Fatal("card not archived")
	}
	// Archiving the task here keeps it done: no further calls.
	delete(f.closed, "c1")
	a.Store.ArchiveTask(ctx, task.ID)
	a.SyncTrello(ctx)
	if _, called := f.closed["c1"]; called {
		t.Fatal("archived again")
	}
	// Out of the done lane, the card comes back.
	a.MoveTask(ctx, task.ID, lanes["Todo"], -1)
	a.SyncTrello(ctx)
	if closed, called := f.closed["c1"]; !called || closed {
		t.Fatalf("card closed = %v, called = %v", closed, called)
	}

	// A card deleted on Trello counts as archived.
	gone := taskByTitle(t, a, "Gone")
	f.missing["c2"] = true
	a.MoveTask(ctx, gone.ID, lanes["Done"], -1)
	if err := a.SyncTrello(ctx); err != nil {
		t.Fatal(err)
	}
	if cards, _ := a.Store.TrelloCards(ctx); !cards["c2"].Closed {
		t.Fatalf("card = %+v", cards["c2"])
	}
}

func TestSyncTrelloFailureNotifiesOnce(t *testing.T) {
	a, f, _ := newTrelloApp(t)
	ctx := context.Background()
	f.err = errors.New("401 invalid token")
	for range 3 {
		if err := a.SyncTrello(ctx); err == nil {
			t.Fatal("no error")
		}
	}
	if st := a.TrelloStatus(); !strings.Contains(st.Error, "invalid token") {
		t.Fatalf("status = %+v", st)
	}
	notes, _ := a.Store.Notifications(ctx, false)
	if len(notes) != 1 || !strings.Contains(notes[0].Message, "Trello sync failed") {
		t.Fatalf("notifications = %+v", notes)
	}
	f.err = nil
	a.SyncTrello(ctx)
	if st := a.TrelloStatus(); st.Error != "" {
		t.Fatalf("status = %+v", st)
	}
}

func TestSyncTrelloOff(t *testing.T) {
	a, _ := newApp(t)
	if err := a.SyncTrello(context.Background()); err != nil { // no client
		t.Fatal(err)
	}
	f := &fakeTrello{err: errors.New("must not be called")}
	a.Trello = f
	if err := a.SyncTrello(context.Background()); err != nil { // no board
		t.Fatal(err)
	}
}
