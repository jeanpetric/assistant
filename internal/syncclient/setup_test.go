package syncclient

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"net/http"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jeanpetric/assistant/internal/store"
	"github.com/jeanpetric/assistant/internal/syncauth"
)

// authorized makes a client for st with a key the hub authorized as name,
// as assistant sync init and authorize do.
func (h *hub) authorized(t *testing.T, st *store.Store, name string) *Client {
	t.Helper()
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.st.AuthorizeSyncKey(context.Background(), syncauth.PublicKey(key), name); err != nil {
		t.Fatal(err)
	}
	c, err := New(st, h.url, key)
	if err != nil {
		t.Fatal(err)
	}
	c.Wait = time.Second
	c.Debounce = 30 * time.Millisecond
	c.MaxDelay = 150 * time.Millisecond
	c.Check = 5 * time.Millisecond
	c.MinBackoff = 10 * time.Millisecond
	c.MaxBackoff = 50 * time.Millisecond
	return c
}

// running starts c syncing, as the app does once restarted.
func running(t *testing.T, st *store.Store, c *Client) *node {
	t.Helper()
	st.SetLeasedIDs(true)
	n := &node{st: st, c: c}
	n.start(t)
	return n
}

func syncDump(t *testing.T, st *store.Store) string {
	t.Helper()
	d, err := st.SyncDump(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// converged waits until every store holds the same synced data.
func converged(t *testing.T, stores ...*store.Store) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		want, diff := syncDump(t, stores[0]), -1
		for i, st := range stores[1:] {
			if syncDump(t, st) != want {
				diff = i + 1
				break
			}
		}
		if diff < 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("store %d differs from store 0:\n%s\nstore 0:\n%s", diff, syncDump(t, stores[diff]), want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// withData is a database used on its own for a while.
func withData(t *testing.T) *store.Store {
	t.Helper()
	ctx := context.Background()
	st := open(t, filepath.Join(t.TempDir(), "laptop.db"))
	if err := st.DeleteLane(ctx, 1); err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateLane(ctx, 3, "Doing", ""); err != nil {
		t.Fatal(err)
	}
	p, err := st.CreateProject(ctx, store.Project{Name: "Work"})
	if err != nil {
		t.Fatal(err)
	}
	for _, title := range []string{"plan", "build", "ship"} {
		task, err := st.CreateTask(ctx, store.Task{Title: title, LaneID: 2, ProjectID: p.ID})
		if err != nil {
			t.Fatal(err)
		}
		sess, err := st.CreateSession(ctx, store.Session{TaskID: task.ID, Title: title})
		if err != nil {
			t.Fatal(err)
		}
		for range 3 {
			if _, err := st.AddMessage(ctx, store.Message{SessionID: sess.ID, Role: "user", Kind: "text", Content: title}); err != nil {
				t.Fatal(err)
			}
		}
	}
	return st
}

func TestSeedJoinAndConverge(t *testing.T) {
	ctx := context.Background()
	h := newHub(t)
	laptop := withData(t)
	lc := h.authorized(t, laptop, "laptop")
	lc.Limit = 7 // several pages
	backups := 0
	n, err := lc.Seed(ctx, func() error { backups++; return nil })
	if err != nil {
		t.Fatal(err)
	}
	if backups != 1 || n == 0 {
		t.Fatalf("%d backups, %d rows", backups, n)
	}
	converged(t, h.st, laptop)
	if node, _ := laptop.NodeID(ctx); node != 2 {
		t.Fatalf("laptop is node %d", node)
	}
	if low, err := laptop.LowOnIDs(ctx); err != nil || len(low) != 0 {
		t.Fatalf("laptop low on %v, %v", low, err)
	}
	if hello, err := lc.Hello(ctx); err != nil || hello.Blank || hello.NodeID != 2 {
		t.Fatalf("hello %+v, %v", hello, err)
	}

	desktop := open(t, filepath.Join(t.TempDir(), "desktop.db"))
	dc := h.authorized(t, desktop, "desktop")
	if err := dc.Join(ctx); err != nil {
		t.Fatal(err)
	}
	converged(t, h.st, laptop, desktop)
	if node, _ := desktop.NodeID(ctx); node != 3 {
		t.Fatalf("desktop is node %d", node)
	}
	if err := dc.Join(ctx); err == nil {
		t.Fatal("joined twice")
	}

	// Restarted, both sync; changes on each reach all.
	running(t, laptop, lc)
	running(t, desktop, dc)
	fromLaptop := create(t, laptop, "from the laptop")
	fromDesktop := create(t, desktop, "from the desktop")
	fromHub := create(t, h.st, "from the hub")
	rename(t, laptop, 1, "plan, edited on the laptop")
	rename(t, desktop, 2, "build, edited on the desktop")
	all := []*store.Store{h.st, laptop, desktop}
	has(t, fromLaptop, "from the laptop", all...)
	has(t, fromDesktop, "from the desktop", all...)
	has(t, fromHub, "from the hub", all...)
	has(t, 1, "plan, edited on the laptop", all...)
	has(t, 2, "build, edited on the desktop", all...)
	converged(t, all...)
	if fromLaptop == fromDesktop || fromLaptop < 1000 || fromDesktop < 1000 {
		t.Fatalf("new ids %d and %d", fromLaptop, fromDesktop)
	}
	if got := title(desktop, 3); got != "ship" {
		t.Fatalf("seeded task on the desktop: %q", got)
	}
}

func TestSeedOnlyABlankHub(t *testing.T) {
	ctx := context.Background()
	h := newHub(t)
	create(t, h.st, "the hub's own")
	laptop := withData(t)
	before := syncDump(t, laptop)
	c := h.authorized(t, laptop, "laptop")
	backups := 0
	if _, err := c.Seed(ctx, func() error { backups++; return nil }); err == nil || !strings.Contains(err.Error(), "data already") {
		t.Fatalf("seed: %v", err)
	}
	if backups != 0 || syncDump(t, laptop) != before {
		t.Fatalf("refused seed changed things: %d backups", backups)
	}
	if node, _ := laptop.NodeID(ctx); node != 1 {
		t.Fatalf("node %d", node)
	}
}

func TestSeedStopsIfTheBackupFails(t *testing.T) {
	ctx := context.Background()
	h := newHub(t)
	laptop := withData(t)
	c := h.authorized(t, laptop, "laptop")
	if _, err := c.Seed(ctx, func() error { return errors.New("disk full") }); err == nil || !strings.Contains(err.Error(), "disk full") {
		t.Fatalf("seed: %v", err)
	}
	if node, _ := laptop.NodeID(ctx); node != 1 {
		t.Fatalf("node %d", node)
	}
	if blank, err := h.st.Blank(ctx); err != nil || !blank {
		t.Fatalf("hub blank %v, %v", blank, err)
	}
}

// failing fails pushes once the first went through, while fail is set.
type failing struct {
	fail   atomic.Bool
	pushes atomic.Int32
}

func (f *failing) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Path == "/sync/push" && f.pushes.Add(1) > 1 && f.fail.Load() {
		return nil, errors.New("line dropped")
	}
	return http.DefaultTransport.RoundTrip(r)
}

func TestSeedGoesOnWhereItStopped(t *testing.T) {
	ctx := context.Background()
	h := newHub(t)
	laptop := withData(t)
	c := h.authorized(t, laptop, "laptop")
	c.Limit = 5
	f := &failing{}
	f.fail.Store(true)
	c.HTTP = &http.Client{Transport: f}
	backups := 0
	backup := func() error { backups++; return nil }
	if _, err := c.Seed(ctx, backup); err == nil {
		t.Fatal("seed went through a dropped line")
	}
	if n, _ := laptop.PendingChanges(ctx); n == 0 {
		t.Fatal("nothing left to push")
	}
	f.fail.Store(false)
	if _, err := c.Seed(ctx, backup); err != nil {
		t.Fatal(err)
	}
	if backups != 1 {
		t.Fatalf("%d backups", backups)
	}
	converged(t, h.st, laptop)
	if n, _ := laptop.PendingChanges(ctx); n != 0 {
		t.Fatalf("%d pending", n)
	}
}

func TestJoinOnlyAnEmptyDatabase(t *testing.T) {
	ctx := context.Background()
	h := newHub(t)
	create(t, h.st, "the hub's own")
	desktop := withData(t)
	before := syncDump(t, desktop)
	c := h.authorized(t, desktop, "desktop")
	if err := c.Join(ctx); !errors.Is(err, store.ErrNotBlank) {
		t.Fatalf("join: %v", err)
	}
	if syncDump(t, desktop) != before {
		t.Fatal("refused join changed the database")
	}
}

func TestSetupWantsAnAuthorizedKey(t *testing.T) {
	ctx := context.Background()
	h := newHub(t)
	_, key, _ := ed25519.GenerateKey(rand.Reader)
	st := open(t, filepath.Join(t.TempDir(), "node.db"))
	c, err := New(st, h.url, key)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Join(ctx); err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("join: %v", err)
	}
	if _, err := c.Seed(ctx, func() error { return nil }); err == nil {
		t.Fatal("seeded with an unknown key")
	}
}
