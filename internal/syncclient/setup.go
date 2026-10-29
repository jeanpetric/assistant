package syncclient

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/jeanpetric/assistant/internal/store"
)

// Hello is what the hub tells a node setting up.
type Hello struct {
	NodeID int64 `json:"node_id"` // the node the hub knows this key as
	Blank  bool  `json:"blank"`   // the hub holds no data yet: it may be seeded
}

// Hello asks the hub which node this is; a key it does not accept is an
// error.
func (c *Client) Hello(ctx context.Context) (Hello, error) {
	var h Hello
	err := c.call(ctx, http.MethodGet, "/sync/hello", nil, &h)
	if err == nil && h.NodeID < 2 {
		err = fmt.Errorf("hub gave node %d", h.NodeID)
	}
	return h, err
}

// set is the node this database is in sync as, or 0 if it never was: the
// hub is node 1, and so is a database never set up.
func (c *Client) set(ctx context.Context, h Hello) (int64, error) {
	node, err := c.st.NodeID(ctx)
	if err != nil || node == 1 {
		return 0, err
	}
	if node != h.NodeID {
		return 0, fmt.Errorf("this database is in sync as node %d, but the hub knows this key as node %d", node, h.NodeID)
	}
	return node, nil
}

// Seed sends all of this database to a blank hub, after backup, and leases
// its id blocks: it is the first node, whose data every other one joins.
// A seed cut short (offline, say) goes on where it stopped when run again.
// It returns how many rows it pushed.
func (c *Client) Seed(ctx context.Context, backup func() error) (int, error) {
	h, err := c.Hello(ctx)
	if err != nil {
		return 0, err
	}
	node, err := c.set(ctx, h)
	if err != nil {
		return 0, err
	}
	if node == 0 {
		if !h.Blank {
			return 0, errors.New("the hub has data already: only a blank hub is seeded; to use its data here, join from an empty database")
		}
		if err := backup(); err != nil {
			return 0, fmt.Errorf("backup: %w", err)
		}
		if _, err := c.st.StartSeed(ctx, h.NodeID); err != nil {
			return 0, err
		}
	}
	n, err := c.st.PendingChanges(ctx)
	if err != nil {
		return 0, err
	}
	// The hub has every id in use before it leases above them.
	if err := c.pushOutbox(ctx); err != nil {
		return 0, err
	}
	return n, c.lease(ctx)
}

// Join fills this database, which must be blank, from the hub's snapshot,
// leases its id blocks and pulls what changed since the snapshot.
func (c *Client) Join(ctx context.Context) error {
	h, err := c.Hello(ctx)
	if err != nil {
		return err
	}
	node, err := c.set(ctx, h)
	if err != nil {
		return err
	}
	if node != 0 {
		return fmt.Errorf("this database is in sync already, as node %d", node)
	}
	if blank, err := c.st.Blank(ctx); err != nil {
		return err
	} else if !blank {
		return fmt.Errorf("%w: only an empty database joins; to send this one's data to a blank hub, seed it", store.ErrNotBlank)
	}
	dir, err := os.MkdirTemp("", "assistant-join-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "snapshot.db")
	seq, err := c.snapshot(ctx, path)
	if err != nil {
		return err
	}
	if err := c.st.Join(ctx, path, h.NodeID, seq); err != nil {
		return err
	}
	if err := c.lease(ctx); err != nil {
		return err
	}
	for {
		more, err := c.pull(ctx, 0)
		if err != nil || !more {
			return err
		}
	}
}

// snapshot downloads the hub's snapshot to path and returns its seq.
func (c *Client) snapshot(ctx context.Context, path string) (int64, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	resp, err := c.do(ctx, http.MethodGet, "/sync/snapshot", nil)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	seq, err := strconv.ParseInt(resp.Header.Get("X-Sync-Seq"), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("snapshot: bad X-Sync-Seq %q", resp.Header.Get("X-Sync-Seq"))
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return 0, err
	}
	n, err := io.Copy(f, resp.Body)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil && resp.ContentLength >= 0 && n != resp.ContentLength {
		err = fmt.Errorf("snapshot cut short: %d of %d bytes", n, resp.ContentLength)
	}
	if err != nil {
		return 0, c.fail(fmt.Errorf("snapshot: %w", err))
	}
	return seq, nil
}
