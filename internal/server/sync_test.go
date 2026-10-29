package server

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/jeanpetric/assistant/internal/store"
	"github.com/jeanpetric/assistant/internal/syncauth"
)

// syncHub serves a hub's store over HTTP, as the sync listener does.
type syncHub struct {
	st  *store.Store
	h   *SyncHub
	url string
}

func newSyncHub(t *testing.T) *syncHub {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	ctx := context.Background()
	if err := st.SetSyncEnabled(ctx, true); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateTask(ctx, store.Task{Title: "before sync"}); err != nil {
		t.Fatal(err)
	}
	st.SetHub(true)
	h := NewSyncHub(st)
	h.Poll = 20 * time.Millisecond
	srv := httptest.NewServer(h.Handler(syncauth.NewVerifier(st.SyncKeyNode)))
	t.Cleanup(srv.Close)
	return &syncHub{st: st, h: h, url: srv.URL}
}

// syncNode is a node syncing with the hub by hand, as the client worker will.
type syncNode struct {
	t      *testing.T
	hub    *syncHub
	key    ed25519.PrivateKey
	signer *syncauth.Signer
	st     *store.Store
	since  int64
}

func newKey(t *testing.T) ed25519.PrivateKey {
	t.Helper()
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

// join authorizes a key for node on the hub and makes the node's database
// from the hub's snapshot.
func (hub *syncHub) join(t *testing.T, node int64) *syncNode {
	t.Helper()
	key := newKey(t)
	if _, err := hub.st.AddSyncKey(context.Background(), syncauth.PublicKey(key), fmt.Sprint("node ", node), node); err != nil {
		t.Fatal(err)
	}
	n := &syncNode{t: t, hub: hub, key: key, signer: syncauth.NewSigner(key)}
	status, body, header := n.do("GET", "/sync/snapshot", nil)
	if status != http.StatusOK {
		t.Fatalf("snapshot: %d %s", status, body)
	}
	path := filepath.Join(t.TempDir(), fmt.Sprintf("node%d.db", node))
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	if n.since, _ = strconv.ParseInt(header.Get("X-Sync-Seq"), 10, 64); n.since == 0 {
		t.Fatalf("snapshot seq %q", header.Get("X-Sync-Seq"))
	}
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`UPDATE sync_node SET node_id = ?`, node)
	db.Close()
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	n.st = st
	if err := st.SetSyncEnabled(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	st.SetLeasedIDs(true)
	return n
}

// do sends a signed request to the hub.
func (n *syncNode) do(method, path string, in any) (int, []byte, http.Header) {
	n.t.Helper()
	var body []byte
	if in != nil {
		body, _ = json.Marshal(in)
	}
	req, err := http.NewRequest(method, n.hub.url+path, bytes.NewReader(body))
	if err != nil {
		n.t.Fatal(err)
	}
	n.signer.Sign(req, body)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		n.t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, out, resp.Header
}

// lease gets the node a block of ids for tbl.
func (n *syncNode) lease(tbl string) (from, to int64) {
	n.t.Helper()
	status, body, _ := n.do("POST", "/sync/lease", map[string]string{"tbl": tbl})
	var out struct{ From, To int64 }
	if status != http.StatusOK || json.Unmarshal(body, &out) != nil {
		n.t.Fatalf("lease %s: %d %s", tbl, status, body)
	}
	if err := n.st.AddBlock(context.Background(), tbl, out.From, out.To); err != nil {
		n.t.Fatal(err)
	}
	return out.From, out.To
}

func (n *syncNode) push() {
	n.t.Helper()
	changes, err := n.st.ExportChanges(context.Background(), 0, 0)
	if err != nil {
		n.t.Fatal(err)
	}
	status, body, _ := n.do("POST", "/sync/push", map[string]any{"changes": changes})
	var out struct{ Acked int64 }
	if status != http.StatusOK || json.Unmarshal(body, &out) != nil {
		n.t.Fatalf("push: %d %s", status, body)
	}
	if len(changes) > 0 && out.Acked != changes[len(changes)-1].Seq {
		n.t.Fatalf("acked %d of %+v", out.Acked, changes)
	}
	if err := n.st.AckOutbox(context.Background(), out.Acked); err != nil {
		n.t.Fatal(err)
	}
}

// pull applies one page of the hub's log and returns it.
func (n *syncNode) pull(wait int) store.Pull {
	n.t.Helper()
	status, body, _ := n.do("GET", fmt.Sprintf("/sync/pull?since=%d&wait=%d", n.since, wait), nil)
	var p store.Pull
	if status != http.StatusOK || json.Unmarshal(body, &p) != nil {
		n.t.Fatalf("pull: %d %s", status, body)
	}
	if err := n.st.ApplyChanges(context.Background(), p.Changes); err != nil {
		n.t.Fatal(err)
	}
	n.since = p.NextSeq
	return p
}

func (n *syncNode) title(id int64) string {
	n.t.Helper()
	task, err := n.st.Task(context.Background(), id)
	if err != nil {
		return err.Error()
	}
	return task.Title
}

func TestSyncPushAndPull(t *testing.T) {
	hub := newSyncHub(t)
	a, b := hub.join(t, 2), hub.join(t, 3)
	ctx := context.Background()
	if got := a.title(1); got != "before sync" {
		t.Fatalf("joined node has %q", got)
	}
	a.lease("tasks")
	task, err := a.st.CreateTask(ctx, store.Task{Title: "from a"})
	if err != nil {
		t.Fatal(err)
	}
	a.push()
	if p := a.pull(0); len(p.Changes) != 0 {
		t.Fatalf("a got its own changes back: %+v", p.Changes)
	}
	b.pull(0)
	if got := b.title(task.ID); got != "from a" {
		t.Fatalf("b has %q", got)
	}

	// The hub's own changes reach both.
	mine, err := hub.st.CreateTask(ctx, store.Task{Title: "from the hub"})
	if err != nil {
		t.Fatal(err)
	}
	a.pull(0)
	b.pull(0)
	if a.title(mine.ID) != "from the hub" || b.title(mine.ID) != "from the hub" {
		t.Fatalf("a %q, b %q", a.title(mine.ID), b.title(mine.ID))
	}

	// Both edit the task offline; the later edit wins everywhere.
	title := func(n *syncNode, s string) {
		t.Helper()
		cur, err := n.st.Task(ctx, task.ID)
		if err != nil {
			t.Fatal(err)
		}
		cur.Title = s
		if err := n.st.UpdateTask(ctx, cur); err != nil {
			t.Fatal(err)
		}
	}
	title(a, "a's edit")
	time.Sleep(5 * time.Millisecond)
	title(b, "b's later edit")
	b.push()
	a.push()
	a.pull(0)
	b.pull(0)
	for name, got := range map[string]string{"hub": titleOn(t, hub.st, task.ID), "a": a.title(task.ID), "b": b.title(task.ID)} {
		if got != "b's later edit" {
			t.Errorf("%s has %q", name, got)
		}
	}
}

func titleOn(t *testing.T, st *store.Store, id int64) string {
	t.Helper()
	task, err := st.Task(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return task.Title
}

func TestSyncPullPages(t *testing.T) {
	hub := newSyncHub(t)
	hub.h.Limit = 2
	a := hub.join(t, 2)
	for i := range 3 {
		if _, err := hub.st.CreateTask(context.Background(), store.Task{Title: fmt.Sprint("t", i)}); err != nil {
			t.Fatal(err)
		}
	}
	pages := 0
	for p := a.pull(0); ; p = a.pull(0) {
		pages++
		if !p.More {
			break
		}
	}
	if pages < 2 {
		t.Fatalf("%d pages", pages)
	}
	if latest, _ := hub.st.LatestSeq(context.Background()); a.since != latest {
		t.Fatalf("since %d, log ends at %d", a.since, latest)
	}
}

func TestSyncLongPollWakes(t *testing.T) {
	hub := newSyncHub(t)
	a, b := hub.join(t, 2), hub.join(t, 3)
	a.lease("tasks")
	ctx := context.Background()

	waitFor := func(change func()) (store.Pull, time.Duration) {
		t.Helper()
		got := make(chan store.Pull, 1)
		start := time.Now()
		go func() { got <- b.pull(10) }()
		time.Sleep(100 * time.Millisecond)
		change()
		select {
		case p := <-got:
			return p, time.Since(start)
		case <-time.After(5 * time.Second):
			t.Fatal("pull still waiting")
			return store.Pull{}, 0
		}
	}
	// A push wakes the pull.
	p, took := waitFor(func() {
		if _, err := a.st.CreateTask(ctx, store.Task{Title: "pushed"}); err != nil {
			t.Error(err)
		}
		a.push()
	})
	if len(p.Changes) == 0 || took < 100*time.Millisecond {
		t.Fatalf("pull returned after %v with %+v", took, p)
	}
	// So does a change on the hub itself.
	p, _ = waitFor(func() {
		if _, err := hub.st.CreateTask(ctx, store.Task{Title: "on the hub"}); err != nil {
			t.Error(err)
		}
	})
	if len(p.Changes) == 0 {
		t.Fatalf("pull returned %+v", p)
	}
	// Its own push does not wake a node: the wait runs out, empty, past it.
	hub.h.MaxWait = 300 * time.Millisecond
	b.lease("tasks")
	if _, err := b.st.CreateTask(ctx, store.Task{Title: "b's own"}); err != nil {
		t.Fatal(err)
	}
	b.push()
	start := time.Now()
	p = b.pull(10)
	if latest, _ := hub.st.LatestSeq(ctx); len(p.Changes) != 0 || p.NextSeq != latest || time.Since(start) < 300*time.Millisecond {
		t.Fatalf("after %v: %+v (log ends at %d)", time.Since(start), p, latest)
	}
}

func TestSyncLeasesDoNotOverlap(t *testing.T) {
	hub := newSyncHub(t)
	a, b := hub.join(t, 2), hub.join(t, 3)
	type block struct{ from, to int64 }
	var blocks []block
	for _, n := range []*syncNode{a, b, a, b} {
		from, to := n.lease("tasks")
		blocks = append(blocks, block{from, to})
	}
	// The hub's own block, leased for its next task.
	mine, err := hub.st.CreateTask(context.Background(), store.Task{Title: "hub"})
	if err != nil {
		t.Fatal(err)
	}
	for i, x := range blocks {
		if x.from <= 1 || x.to < x.from {
			t.Fatalf("block %v", x)
		}
		if mine.ID >= x.from && mine.ID <= x.to {
			t.Fatalf("hub's task %d in block %v", mine.ID, x)
		}
		for _, y := range blocks[i+1:] {
			if x.from <= y.to && y.from <= x.to {
				t.Fatalf("blocks %v and %v overlap", x, y)
			}
		}
	}
	status, body, _ := a.do("POST", "/sync/lease", map[string]string{"tbl": "sync_keys"})
	if status != http.StatusBadRequest {
		t.Fatalf("lease of a local table: %d %s", status, body)
	}
}

func TestSyncRejectsUnauthorized(t *testing.T) {
	hub := newSyncHub(t)
	a := hub.join(t, 2)
	stranger := &syncNode{t: t, hub: hub, signer: syncauth.NewSigner(newKey(t))}
	for _, path := range []string{"/sync/pull?since=0", "/sync/snapshot"} {
		if status, _, _ := stranger.do("GET", path, nil); status != http.StatusUnauthorized {
			t.Errorf("unknown key, %s: %d", path, status)
		}
		resp, err := http.Get(hub.url + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("unsigned, %s: %d", path, resp.StatusCode)
		}
	}
	if status, _, _ := stranger.do("POST", "/sync/push", map[string]any{"changes": []any{}}); status != http.StatusUnauthorized {
		t.Errorf("unknown key push: %d", status)
	}
	// Only /sync/* is there, even signed.
	if status, _, _ := a.do("GET", "/api/board", nil); status != http.StatusNotFound {
		t.Errorf("api on the sync listener: %d", status)
	}
	if err := hub.st.RevokeSyncKey(context.Background(), syncauth.PublicKey(a.key)); err != nil {
		t.Fatal(err)
	}
	if status, _, _ := a.do("GET", "/sync/pull?since=0", nil); status != http.StatusUnauthorized {
		t.Errorf("revoked key: %d", status)
	}
}
