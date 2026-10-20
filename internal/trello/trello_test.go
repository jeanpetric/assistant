package trello

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestBoardID(t *testing.T) {
	for in, want := range map[string]string{
		"https://trello.com/b/AbC123/my-board": "AbC123",
		"trello.com/b/AbC123":                  "AbC123",
		" AbC123 ":                             "AbC123",
		"5f0c1e2d3a4b5c6d7e8f9012":             "5f0c1e2d3a4b5c6d7e8f9012",
	} {
		if got := BoardID(in); got != want {
			t.Errorf("BoardID(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestOpenCardsAndSetClosed(t *testing.T) {
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if a := r.Header.Get("Authorization"); a != `OAuth oauth_consumer_key="k", oauth_token="tok"` {
			t.Errorf("Authorization = %q", a)
		}
		got = append(got, r.Method+" "+r.URL.Path+"?"+r.URL.RawQuery)
		if r.Method == http.MethodGet {
			w.Write([]byte(`[{"id":"c1","name":"Buy milk","desc":"2l","shortUrl":"https://trello.com/c/x","due":"2026-10-25T10:00:00.000Z"},
				{"id":"c2","name":"No due","desc":"","shortUrl":"https://trello.com/c/y","due":null}]`))
		}
	}))
	defer srv.Close()
	c := &Client{Key: "k", Token: "tok", BaseURL: srv.URL}
	ctx := context.Background()

	cards, err := c.OpenCards(ctx, "https://trello.com/b/AbC/board")
	if err != nil {
		t.Fatal(err)
	}
	want := time.Date(2026, 10, 25, 10, 0, 0, 0, time.UTC)
	if len(cards) != 2 || cards[0].Name != "Buy milk" || cards[0].Desc != "2l" || !cards[0].Due.Equal(want) || cards[1].Due != nil {
		t.Fatalf("cards = %+v", cards)
	}
	if err := c.SetClosed(ctx, "c1", true); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != "GET /1/boards/AbC/cards/open?fields=name%2Cdesc%2Cdue%2CshortUrl" || got[1] != "PUT /1/cards/c1?closed=true" {
		t.Fatalf("requests = %q", got)
	}
}

func TestErrorKeepsTokenOut(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "invalid token", http.StatusUnauthorized)
	}))
	defer srv.Close()
	c := &Client{Key: "k", Token: "secret", BaseURL: srv.URL}
	_, err := c.OpenCards(context.Background(), "b")
	if err == nil || !strings.Contains(err.Error(), "401") || !strings.Contains(err.Error(), "invalid token") || strings.Contains(err.Error(), "secret") {
		t.Fatalf("err = %v", err)
	}
}

func TestNotFound(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()
	c := &Client{BaseURL: srv.URL}
	if err := c.SetClosed(context.Background(), "gone", true); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v", err)
	}
}
