// Package trello is a minimal client for the Trello REST API: the open cards
// of a board, and archiving a card.
package trello

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// ErrNotFound: the board or card does not exist, or the token can't see it.
var ErrNotFound = errors.New("not found")

type Client struct {
	Key, Token string
	BaseURL    string // default https://api.trello.com
	HTTP       *http.Client
}

type Card struct {
	ID       string     `json:"id"`
	Name     string     `json:"name"`
	Desc     string     `json:"desc"`
	ShortURL string     `json:"shortUrl"`
	Due      *time.Time `json:"due"`
}

// boardURL matches https://trello.com/b/<shortLink>/<name>.
var boardURL = regexp.MustCompile(`trello\.com/b/([A-Za-z0-9]+)`)

// BoardID takes a board's id, short link or URL and returns what the API
// accepts as its id.
func BoardID(s string) string {
	s = strings.TrimSpace(s)
	if m := boardURL.FindStringSubmatch(s); m != nil {
		return m[1]
	}
	return s
}

// OpenCards returns the cards of the board that are not archived.
func (c *Client) OpenCards(ctx context.Context, board string) ([]Card, error) {
	var cards []Card
	q := url.Values{"fields": {"name,desc,due,shortUrl"}}
	err := c.do(ctx, http.MethodGet, "/1/boards/"+url.PathEscape(BoardID(board))+"/cards/open", q, &cards)
	return cards, err
}

// SetClosed archives the card, or brings it back.
func (c *Client) SetClosed(ctx context.Context, cardID string, closed bool) error {
	q := url.Values{"closed": {fmt.Sprint(closed)}}
	return c.do(ctx, http.MethodPut, "/1/cards/"+url.PathEscape(cardID), q, nil)
}

func (c *Client) do(ctx context.Context, method, path string, q url.Values, out any) error {
	base := c.BaseURL
	if base == "" {
		base = "https://api.trello.com"
	}
	req, err := http.NewRequestWithContext(ctx, method, base+path+"?"+q.Encode(), nil)
	if err != nil {
		return err
	}
	// In a header rather than the URL, so the token stays out of error messages.
	req.Header.Set("Authorization", fmt.Sprintf(`OAuth oauth_consumer_key="%s", oauth_token="%s"`, c.Key, c.Token))
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	res, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode == http.StatusNotFound {
		return fmt.Errorf("trello: %s %s: %w", method, path, ErrNotFound)
	}
	if res.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(res.Body, 300))
		return fmt.Errorf("trello: %s %s: %s %s", method, path, res.Status, strings.TrimSpace(string(body)))
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(res.Body).Decode(out)
}
