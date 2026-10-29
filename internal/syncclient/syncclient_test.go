package syncclient

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jeanpetric/assistant/internal/server"
	"github.com/jeanpetric/assistant/internal/store"
	"github.com/jeanpetric/assistant/internal/syncauth"
)

const (
	up       = iota
	down     // connections are dropped, as with no network
	badGate  // 502, as nginx answers with the hub stopped
	lostAcks // pushes are applied but their answers lost
)

// flaky is the hub behind a line that can fail.
type flaky struct {
	h      http.Handler
	mode   atomic.Int32
	pushes atomic.Int32 // push requests that reached the hub
	calls  atomic.Int32 // requests of any kind, failed ones included
}

func (f *flaky) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.calls.Add(1)
	switch f.mode.Load() {
	case down:
		drop(w)
		return
	case badGate:
		http.Error(w, "bad gateway", http.StatusBadGateway)
		return
	case lostAcks:
		if r.URL.Path == "/sync/push" {
			f.pushes.Add(1)
			f.h.ServeHTTP(httptest.NewRecorder(), r)
			drop(w)
			return
		}
	}
	if r.URL.Path == "/sync/push" {
		f.pushes.Add(1)
	}
	f.h.ServeHTTP(w, r)
}

func drop(w http.ResponseWriter) {
	conn, _, err := w.(http.Hijacker).Hijack()
	if err == nil {
		conn.Close()
	}
}

type hub struct {
	st  *store.Store
	f   *flaky
	url string
}

func newHub(t *testing.T) *hub {
	t.Helper()
	st := open(t, filepath.Join(t.TempDir(), "hub.db"))
	if err := st.SetSyncEnabled(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	st.SetHub(true)
	h := server.NewSyncHub(st)
	h.Poll = 10 * time.Millisecond
	f := &flaky{h: h.Handler(syncauth.NewVerifier(st.SyncKeyNode))}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return &hub{st: st, f: f, url: srv.URL}
}

func open(t *testing.T, path string) *store.Store {
	t.Helper()
	st, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

type node struct {
	st   *store.Store
	c    *Client
	stop func() // stops the client and waits for it
}

// join makes node id from the hub's snapshot, with a key the hub accepts,
// and starts its client once set up.
func (h *hub) join(t *testing.T, id int64, setup ...func(*Client)) *node {
	t.Helper()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), fmt.Sprintf("node%d.db", id))
	if _, err := h.st.Snapshot(ctx, path); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`UPDATE sync_node SET node_id = ?`, id)
	db.Close()
	if err != nil {
		t.Fatal(err)
	}
	st := open(t, path)
	if err := st.SetSyncEnabled(ctx, true); err != nil {
		t.Fatal(err)
	}
	st.SetLeasedIDs(true)
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.st.AddSyncKey(ctx, syncauth.PublicKey(key), fmt.Sprint("node ", id), id); err != nil {
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
	for _, f := range setup {
		f(c)
	}
	n := &node{st: st, c: c}
	n.start(t)
	return n
}

func (n *node) start(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		n.c.Run(ctx)
	}()
	stopped := false
	n.stop = func() {
		if stopped {
			return
		}
		stopped = true
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("client did not stop")
		}
	}
	t.Cleanup(n.stop)
}

func (n *node) status(t *testing.T) Status {
	t.Helper()
	s, err := n.c.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// leased waits until the node holds a full block of ids for every table.
func (n *node) leased(t *testing.T) {
	t.Helper()
	eventually(t, "leases", func() bool {
		low, err := n.st.LowOnIDs(context.Background())
		return err == nil && len(low) == 0
	})
}

func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func title(st *store.Store, id int64) string {
	task, err := st.Task(context.Background(), id)
	if err != nil {
		return err.Error()
	}
	return task.Title
}

func create(t *testing.T, st *store.Store, title string) int64 {
	t.Helper()
	task, err := st.CreateTask(context.Background(), store.Task{Title: title})
	if err != nil {
		t.Fatal(err)
	}
	return task.ID
}

func rename(t *testing.T, st *store.Store, id int64, title string) {
	t.Helper()
	task, err := st.Task(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	task.Title = title
	if err := st.UpdateTask(context.Background(), task); err != nil {
		t.Fatal(err)
	}
}

// has waits until every store has task id titled want.
func has(t *testing.T, id int64, want string, stores ...*store.Store) {
	t.Helper()
	for i, st := range stores {
		eventually(t, fmt.Sprintf("task %d %q on store %d", id, want, i), func() bool { return title(st, id) == want })
	}
}

// logged counts the hub's log entries for task id.
func (h *hub) logged(t *testing.T, id int64) int {
	t.Helper()
	p, err := h.st.PullChanges(context.Background(), 0, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, c := range p.Changes {
		if c.Table == "tasks" && string(c.Key) == fmt.Sprintf("[%d]", id) {
			n++
		}
	}
	return n
}

func TestSyncsBothWays(t *testing.T) {
	h := newHub(t)
	a, b := h.join(t, 2), h.join(t, 3)
	a.leased(t)
	b.leased(t)

	fromA := create(t, a.st, "from a")
	has(t, fromA, "from a", h.st, b.st)
	rename(t, b.st, fromA, "edited on b")
	has(t, fromA, "edited on b", h.st, a.st)
	fromHub := create(t, h.st, "from the hub")
	has(t, fromHub, "from the hub", a.st, b.st)
	if fromA == fromHub || fromA < 1000 {
		t.Fatalf("ids %d and %d", fromA, fromHub)
	}

	for _, n := range []*node{a, b} {
		eventually(t, "nothing pending", func() bool { return n.status(t).Pending == 0 })
		s := n.status(t)
		if !s.Online || s.SyncedAt == nil || s.Error != "" || s.Server != h.url {
			t.Fatalf("status %+v", s)
		}
	}
}

func TestOfflineChangesWaitAndArrive(t *testing.T) {
	for _, mode := range []int32{down, badGate} {
		t.Run(fmt.Sprint("mode ", mode), func(t *testing.T) {
			h := newHub(t)
			a, b := h.join(t, 2), h.join(t, 3)
			a.leased(t)
			b.leased(t)
			shared := create(t, a.st, "shared")
			has(t, shared, "shared", b.st)

			h.f.mode.Store(mode)
			before := time.Now()
			var offline []int64
			for i := range 5 {
				offline = append(offline, create(t, a.st, fmt.Sprint("a offline ", i)))
				offline = append(offline, create(t, b.st, fmt.Sprint("b offline ", i)))
			}
			rename(t, b.st, shared, "renamed offline")
			eventually(t, "a offline", func() bool { s := a.status(t); return !s.Online && s.Error != "" })
			if s := a.status(t); s.Pending != 5 || s.SyncedAt == nil || s.SyncedAt.After(before) {
				t.Fatalf("offline status %+v", s)
			}
			// The app goes on: more changes, ids from the blocks in hand.
			rename(t, a.st, offline[0], "a offline 0, edited")
			time.Sleep(100 * time.Millisecond)

			h.f.mode.Store(up)
			all := []*store.Store{h.st, a.st, b.st}
			has(t, shared, "renamed offline", all...)
			has(t, offline[0], "a offline 0, edited", all...)
			for i, id := range offline[1:] {
				want := fmt.Sprintf("%s offline %d", map[int]string{0: "b", 1: "a"}[i%2], (i+1)/2)
				has(t, id, want, all...)
			}
			for _, n := range []*node{a, b} {
				eventually(t, "online", func() bool { s := n.status(t); return s.Online && s.Pending == 0 && s.Error == "" })
			}
			for _, id := range offline {
				if got := h.logged(t, id); got != 1 {
					t.Fatalf("task %d in the hub's log %d times", id, got)
				}
			}
		})
	}
}

func TestLostAcksPushNothingTwice(t *testing.T) {
	h := newHub(t)
	a, b := h.join(t, 2), h.join(t, 3)
	a.leased(t)
	h.f.mode.Store(lostAcks)
	pushes := h.f.pushes.Load()
	id := create(t, a.st, "pushed, answer lost")
	// The hub has it, but a never heard and pushes it again.
	eventually(t, "a second push", func() bool { return h.f.pushes.Load() >= pushes+2 })
	if got := title(h.st, id); got != "pushed, answer lost" {
		t.Fatalf("hub has %q", got)
	}
	if s := a.status(t); s.Pending != 1 {
		t.Fatalf("status %+v", s)
	}
	h.f.mode.Store(up)
	eventually(t, "acked", func() bool { return a.status(t).Pending == 0 })
	has(t, id, "pushed, answer lost", b.st)
	if got := h.logged(t, id); got != 1 {
		t.Fatalf("task in the hub's log %d times", got)
	}
	if all, err := b.st.SearchTasks(context.Background(), "answer lost", 10); err != nil || len(all) != 1 {
		t.Fatalf("b has %d such tasks (%v)", len(all), err)
	}
}

func TestPushWaitsForQuiet(t *testing.T) {
	h := newHub(t)
	a := h.join(t, 2, func(c *Client) {
		c.Debounce = 300 * time.Millisecond
		c.MaxDelay = 800 * time.Millisecond
	})
	a.leased(t)
	pushes := h.f.pushes.Load()

	// A change every 100 ms: no quiet time, so it goes at MaxDelay.
	start := time.Now()
	id := create(t, a.st, "v0")
	for i := 1; time.Since(start) < 600*time.Millisecond; i++ {
		time.Sleep(100 * time.Millisecond)
		rename(t, a.st, id, fmt.Sprint("v", i))
	}
	if got := h.f.pushes.Load(); got != pushes {
		t.Fatalf("pushed %d times while changes went on", got-pushes)
	}
	eventually(t, "pushed", func() bool { return h.f.pushes.Load() > pushes })
	if waited := time.Since(start); waited > 2*time.Second {
		t.Fatalf("pushed after %v", waited)
	}
	eventually(t, "nothing pending", func() bool { return a.status(t).Pending == 0 })

	// One change: pushed once, Debounce after it.
	pushes = h.f.pushes.Load()
	rename(t, a.st, id, "last")
	time.Sleep(150 * time.Millisecond)
	if got := h.f.pushes.Load(); got != pushes {
		t.Fatal("pushed before the quiet time")
	}
	has(t, id, "last", h.st)
	time.Sleep(200 * time.Millisecond)
	if got := h.f.pushes.Load(); got != pushes+1 {
		t.Fatalf("%d pushes for one change", got-pushes)
	}
}

func TestBacksOffWhileOffline(t *testing.T) {
	h := newHub(t)
	a := h.join(t, 2)
	a.leased(t)
	h.f.mode.Store(down)
	create(t, a.st, "waits")
	time.Sleep(50 * time.Millisecond)
	calls := h.f.calls.Load()
	time.Sleep(500 * time.Millisecond)
	// Two loops retrying at most every 50 ms once backed off.
	if n := h.f.calls.Load() - calls; n > 25 {
		t.Fatalf("%d requests in 500 ms offline", n)
	}
	c := &Client{MinBackoff: time.Second, MaxBackoff: time.Minute}
	var d time.Duration
	var got []time.Duration
	for range 8 {
		d = c.nextBackoff(d)
		got = append(got, d)
	}
	want := []time.Duration{1, 2, 4, 8, 16, 32, 60, 60}
	for i := range want {
		if got[i] != want[i]*time.Second {
			t.Fatalf("backoff %v", got)
		}
	}
}

func TestStopsGracefully(t *testing.T) {
	h := newHub(t)
	a := h.join(t, 2, func(c *Client) { c.Wait = 30 * time.Second })
	a.leased(t)
	// A pull waiting at the hub, then stop.
	time.Sleep(1200 * time.Millisecond)
	start := time.Now()
	a.stop()
	if took := time.Since(start); took > time.Second {
		t.Fatalf("stopping took %v", took)
	}
	// Changes made while stopped are pushed on the next start.
	id := create(t, a.st, "made while stopped")
	time.Sleep(100 * time.Millisecond)
	if got := title(h.st, id); got == "made while stopped" {
		t.Fatal("pushed while stopped")
	}
	a.start(t)
	has(t, id, "made while stopped", h.st)

	// Stopping while backing off.
	h.f.mode.Store(down)
	create(t, a.st, "waits")
	eventually(t, "offline", func() bool { return !a.status(t).Online })
	start = time.Now()
	a.stop()
	if took := time.Since(start); took > time.Second {
		t.Fatalf("stopping offline took %v", took)
	}
}

func TestNewWantsAHubURL(t *testing.T) {
	key := ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize))
	for _, bad := range []string{"", "assistant.example.com", "ftp://x", "https://x/sync", "https://x?a=1"} {
		if _, err := New(nil, bad, key); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	c, err := New(nil, "https://assistant.example.com/", key)
	if err != nil || c.server != "https://assistant.example.com" {
		t.Fatalf("%+v, %v", c, err)
	}
}
