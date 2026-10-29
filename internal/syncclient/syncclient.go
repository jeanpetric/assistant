// Package syncclient keeps a node in sync with its hub: it pushes the node's
// outbox a moment after local changes, pulls the hub's log by long-poll and
// leases id blocks before the node runs out. Offline it retries with
// exponential backoff while the app goes on as usual and the outbox waits.
package syncclient

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/jeanpetric/assistant/internal/store"
	"github.com/jeanpetric/assistant/internal/syncauth"
)

// Client syncs a store with the hub at a URL.
type Client struct {
	st     *store.Store
	server string
	signer *syncauth.Signer
	HTTP   *http.Client

	Wait       time.Duration // how long a pull waits at the hub for changes
	Debounce   time.Duration // quiet time after a local change before pushing it
	MaxDelay   time.Duration // longest a change waits while changes go on
	Check      time.Duration // how often the outbox is looked at
	MinBackoff time.Duration // first retry after a failure, doubling up to MaxBackoff
	MaxBackoff time.Duration
	Limit      int // outbox entries per push
	Now        func() time.Time

	mu     sync.Mutex
	online bool
	err    string
}

// New makes a client for the hub at server (scheme and host, no path: the
// signature covers the path the hub sees) signing with key.
func New(st *store.Store, server string, key ed25519.PrivateKey) (*Client, error) {
	u, err := url.Parse(server)
	if err != nil {
		return nil, fmt.Errorf("sync_server: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" || u.Host == "" || strings.Trim(u.Path, "/") != "" || u.RawQuery != "" {
		return nil, fmt.Errorf("sync_server %q: want http(s)://host[:port], no path", server)
	}
	return &Client{
		st: st, server: u.Scheme + "://" + u.Host, signer: syncauth.NewSigner(key), HTTP: &http.Client{},
		Wait: 30 * time.Second, Debounce: 2 * time.Second, MaxDelay: 10 * time.Second, Check: 250 * time.Millisecond,
		MinBackoff: time.Second, MaxBackoff: time.Minute, Limit: 500, Now: time.Now,
	}, nil
}

// Status is how syncing with the hub goes.
type Status struct {
	Server   string     `json:"server"`
	Online   bool       `json:"online"`              // the hub answered the latest request
	Pending  int        `json:"pending"`             // rows with changes not pushed yet
	SyncedAt *time.Time `json:"synced_at,omitempty"` // last exchange with the hub; nil: never
	Error    string     `json:"error,omitempty"`     // why the latest request failed
}

func (c *Client) Status(ctx context.Context) (Status, error) {
	c.mu.Lock()
	s := Status{Server: c.server, Online: c.online, Error: c.err}
	c.mu.Unlock()
	var err error
	if s.Pending, err = c.st.PendingChanges(ctx); err != nil {
		return s, err
	}
	at, err := c.st.SyncedAt(ctx)
	if !at.IsZero() {
		s.SyncedAt = &at
	}
	return s, err
}

// Run syncs until ctx is done, then returns once requests in flight are
// dropped; what they carried is pushed or pulled again next time.
func (c *Client) Run(ctx context.Context) {
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); c.pushLoop(ctx) }()
	go func() { defer wg.Done(); c.pullLoop(ctx) }()
	wg.Wait()
}

// pushLoop pushes the outbox Debounce after the last local change, at most
// MaxDelay after the first one not pushed, and on start what waited.
func (c *Client) pushLoop(ctx context.Context) {
	tick := time.NewTicker(c.Check)
	defer tick.Stop()
	var (
		seen    int64     // the outbox's latest seq as last looked at
		first   time.Time // the first change not pushed was seen; zero: none
		due     = c.Now() // when to push next; zero: nothing to
		backoff time.Duration
	)
	for {
		if !due.IsZero() && !c.Now().Before(due) {
			if err := c.pushAll(ctx); err != nil {
				if ctx.Err() != nil {
					return
				}
				backoff = c.nextBackoff(backoff)
				due = c.Now().Add(backoff)
			} else {
				backoff, first, due = 0, time.Time{}, time.Time{}
			}
			// Acking moved the latest seq; only changes after count.
			if seq, err := c.st.LatestSeq(ctx); err == nil {
				seen = seq
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		seq, err := c.st.LatestSeq(ctx)
		if err != nil || seq == seen {
			continue
		}
		seen = seq
		if seq == 0 || backoff > 0 {
			continue // nothing to push, or a retry is set already
		}
		now := c.Now()
		if first.IsZero() {
			first = now
		}
		due = now.Add(c.Debounce)
		if limit := first.Add(c.MaxDelay); due.After(limit) {
			due = limit
		}
	}
}

// pushAll leases the id blocks running low, then pushes the outbox.
func (c *Client) pushAll(ctx context.Context) error {
	if err := c.lease(ctx); err != nil {
		return err
	}
	return c.pushOutbox(ctx)
}

// lease leases a block for each table with less than a full one in hand.
func (c *Client) lease(ctx context.Context) error {
	low, err := c.st.LowOnIDs(ctx)
	if err != nil {
		return err
	}
	for _, tbl := range low {
		var out struct {
			From int64 `json:"from"`
			To   int64 `json:"to"`
		}
		if err := c.call(ctx, http.MethodPost, "/sync/lease", map[string]string{"tbl": tbl}, &out); err != nil {
			return err
		}
		if err := c.st.AddBlock(ctx, tbl, out.From, out.To); err != nil {
			return err
		}
	}
	return nil
}

// pushOutbox pushes the outbox page by page, dropping each page the hub
// acked.
func (c *Client) pushOutbox(ctx context.Context) error {
	for {
		changes, err := c.st.ExportChanges(ctx, 0, c.Limit)
		if err != nil || len(changes) == 0 {
			return err
		}
		var out struct {
			Acked int64 `json:"acked"`
		}
		if err := c.call(ctx, http.MethodPost, "/sync/push", map[string]any{"changes": changes}, &out); err != nil {
			return err
		}
		if out.Acked < changes[len(changes)-1].Seq {
			return c.fail(fmt.Errorf("push: hub acked %d of %d", out.Acked, changes[len(changes)-1].Seq))
		}
		if err := c.st.AckOutbox(ctx, out.Acked); err != nil {
			return err
		}
		c.synced(ctx)
	}
}

// pullLoop applies the hub's log as it grows: a pull waits at the hub until
// there are changes, and the next follows at once.
func (c *Client) pullLoop(ctx context.Context) {
	var backoff time.Duration
	for {
		_, err := c.pull(ctx, c.Wait)
		if ctx.Err() != nil {
			return
		}
		if err == nil {
			backoff = 0
			continue
		}
		backoff = c.nextBackoff(backoff)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
	}
}

// pull applies the next page of the hub's log, waiting up to wait for one,
// and reports whether the log goes on after it.
func (c *Client) pull(ctx context.Context, wait time.Duration) (bool, error) {
	since, err := c.st.PulledSeq(ctx)
	if err != nil {
		return false, err
	}
	var p store.Pull
	path := fmt.Sprintf("/sync/pull?since=%d&wait=%d", since, int(wait/time.Second))
	if err := c.call(ctx, http.MethodGet, path, nil, &p); err != nil {
		return false, err
	}
	if err := c.st.ApplyPulled(ctx, p.Changes, p.NextSeq); err != nil {
		return false, c.fail(fmt.Errorf("pull: %w", err))
	}
	c.synced(ctx)
	return p.More, nil
}

func (c *Client) nextBackoff(d time.Duration) time.Duration {
	return min(max(2*d, c.MinBackoff), c.MaxBackoff)
}

// call sends a signed request with in as its JSON body (nil: none) and
// decodes the answer into out. The hub's answer, or its lack, sets Online.
func (c *Client) call(ctx context.Context, method, path string, in, out any) error {
	// A pull waits at the hub; anything longer than that is a dead line.
	ctx, cancel := context.WithTimeout(ctx, c.Wait+30*time.Second)
	defer cancel()
	resp, err := c.do(ctx, method, path, in)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return c.fail(fmt.Errorf("%s %s: %w", method, path, err))
	}
	return nil
}

// do sends a signed request with in as its JSON body (nil: none) and
// returns the hub's answer if it is 200 OK.
func (c *Client) do(ctx context.Context, method, path string, in any) (*http.Response, error) {
	var body []byte
	if in != nil {
		var err error
		if body, err = json.Marshal(in); err != nil {
			return nil, err
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, c.server+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	c.signer.Sign(req, body)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, c.fail(err)
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, c.fail(fmt.Errorf("%s %s: %s %s", method, strings.SplitN(path, "?", 2)[0], resp.Status, strings.TrimSpace(string(msg))))
	}
	return resp, nil
}

// fail records err as why syncing stopped, unless it is the client being
// stopped, and returns it.
func (c *Client) fail(err error) error {
	if errors.Is(err, context.Canceled) {
		return err
	}
	c.mu.Lock()
	c.online, c.err = false, err.Error()
	c.mu.Unlock()
	return err
}

func (c *Client) synced(ctx context.Context) {
	c.mu.Lock()
	c.online, c.err = true, ""
	c.mu.Unlock()
	c.st.SetSyncedAt(ctx, c.Now())
}
