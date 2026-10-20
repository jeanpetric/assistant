package server

import (
	"context"
	"testing"

	"github.com/jeanpetric/assistant/internal/trello"
)

type fakeTrello struct{ cards []trello.Card }

func (f fakeTrello) OpenCards(context.Context, string) ([]trello.Card, error) { return f.cards, nil }
func (fakeTrello) SetClosed(context.Context, string, bool) error              { return nil }

type trelloResp struct {
	Board      string `json:"board"`
	ProjectID  *int64 `json:"project_id"`
	DoneLaneID *int64 `json:"done_lane_id"`
	Configured bool   `json:"configured"`
	LastSyncAt any    `json:"last_sync_at"`
	Error      string `json:"error"`
}

func TestTrelloSettingsAndSync(t *testing.T) {
	c := newClient(t)
	var got trelloResp
	c.do("GET", "/api/trello", nil, 200, &got)
	if got.Configured || got.Board != "" {
		t.Fatalf("initial = %+v", got)
	}
	c.do("PUT", "/api/trello", map[string]any{"board": "b", "done_lane_id": 999}, 400, nil)
	c.do("PUT", "/api/trello", map[string]any{"board": "b", "project_id": 999}, 400, nil)
	c.do("PUT", "/api/trello", map[string]any{"board": " https://trello.com/b/AbC/x ", "project_id": 1, "done_lane_id": 5}, 200, &got)
	if got.Board != "https://trello.com/b/AbC/x" || *got.ProjectID != 1 || *got.DoneLaneID != 5 {
		t.Fatalf("saved = %+v", got)
	}

	c.app.Trello = fakeTrello{cards: []trello.Card{{ID: "c1", Name: "From Trello"}}}
	c.do("POST", "/api/trello/sync", nil, 200, &got)
	if !got.Configured || got.LastSyncAt == nil || got.Error != "" {
		t.Fatalf("after sync = %+v", got)
	}
	var tasks []struct{ Title string }
	c.do("GET", "/api/tasks?q=Trello", nil, 200, &tasks)
	if len(tasks) != 1 || tasks[0].Title != "From Trello" {
		t.Fatalf("tasks = %+v", tasks)
	}
}
