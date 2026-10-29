package server

import (
	"context"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/jeanpetric/assistant/internal/store"
	"github.com/jeanpetric/assistant/internal/syncauth"
)

// SyncHub serves /sync/* to the nodes syncing with this one, on a listener
// of its own: every request must be signed by a known key (syncauth). The
// UI, API and MCP never go through it.
type SyncHub struct {
	st      *store.Store
	Poll    time.Duration // how often a waiting pull looks for the hub's own changes
	MaxWait time.Duration // longest a pull waits for changes
	Limit   int           // log entries per pull

	mu   sync.Mutex
	wake chan struct{} // closed on a push
}

func NewSyncHub(st *store.Store) *SyncHub {
	return &SyncHub{st: st, Poll: time.Second, MaxWait: 30 * time.Second, Limit: 500, wake: make(chan struct{})}
}

// Handler serves the sync endpoints behind v; anything else is 404 to a
// signed request and 401 to the rest.
func (h *SyncHub) Handler(v *syncauth.Verifier) http.Handler {
	m := http.NewServeMux()
	m.HandleFunc("POST /sync/push", h.push)
	m.HandleFunc("GET /sync/pull", h.pull)
	m.HandleFunc("POST /sync/lease", h.lease)
	m.HandleFunc("GET /sync/snapshot", h.snapshot)
	return v.Middleware(m)
}

// woken is closed on the next push.
func (h *SyncHub) woken() <-chan struct{} {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.wake
}

func (h *SyncHub) pushed() {
	h.mu.Lock()
	close(h.wake)
	h.wake = make(chan struct{})
	h.mu.Unlock()
}

func sender(r *http.Request) int64 {
	node, _ := syncauth.Node(r.Context())
	return node
}

// push applies a node's changes, later change winning, and logs those that
// changed a row. acked is the node's last outbox entry they cover.
func (h *SyncHub) push(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Changes []store.Change `json:"changes"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, err)
		return
	}
	var acked int64
	for _, c := range in.Changes {
		acked = max(acked, c.Seq)
	}
	if err := h.st.ApplyPushed(r.Context(), sender(r), in.Changes); err != nil {
		log.Printf("sync: push from node %d: %v", sender(r), err)
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": err.Error()})
		return
	}
	h.pushed()
	writeJSON(w, http.StatusOK, map[string]int64{"acked": acked})
}

// pull gives the changes after since that came from other nodes, waiting up
// to wait seconds (at most MaxWait) for some when there are none.
func (h *SyncHub) pull(w http.ResponseWriter, r *http.Request) {
	since, err := queryID(r, "since")
	if err != nil || since < 0 {
		writeErr(w, badRequest{"invalid since"})
		return
	}
	wait := time.Duration(0)
	if v := r.URL.Query().Get("wait"); v != "" {
		secs, err := strconv.Atoi(v)
		if err != nil || secs < 0 {
			writeErr(w, badRequest{"invalid wait"})
			return
		}
		wait = min(time.Duration(secs)*time.Second, h.MaxWait)
	}
	ctx := r.Context()
	deadline := time.NewTimer(wait)
	defer deadline.Stop()
	tick := time.NewTicker(h.Poll)
	defer tick.Stop()
	for {
		// Taken before reading, so a push in between still wakes us.
		woken := h.woken()
		p, err := h.st.PullChanges(ctx, since, h.Limit, sender(r))
		if err != nil {
			writeErr(w, err)
			return
		}
		if len(p.Changes) > 0 || p.More || wait == 0 {
			writeJSON(w, http.StatusOK, p)
			return
		}
		// Only the node's own changes so far: skip them and wait on.
		since = p.NextSeq
		select {
		case <-woken:
		case <-tick.C:
		case <-deadline.C:
			writeJSON(w, http.StatusOK, p)
			return
		case <-ctx.Done():
			return
		}
	}
}

// lease gives the node a new block of ids for a table.
func (h *SyncHub) lease(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Table string `json:"tbl"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, err)
		return
	}
	from, to, err := h.st.LeaseBlock(r.Context(), in.Table, sender(r))
	if err != nil {
		writeErr(w, badRequest{err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"tbl": in.Table, "from": from, "to": to})
}

// snapshot sends a consistent copy of the database for a node to join from;
// X-Sync-Seq is the since of its first pull.
func (h *SyncHub) snapshot(w http.ResponseWriter, r *http.Request) {
	dir, err := os.MkdirTemp("", "assistant-snapshot-")
	if err != nil {
		writeErr(w, err)
		return
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "assistant.db")
	seq, err := h.st.Snapshot(r.Context(), path)
	if err != nil {
		writeErr(w, err)
		return
	}
	f, err := os.Open(path)
	if err != nil {
		writeErr(w, err)
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		writeErr(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/vnd.sqlite3")
	w.Header().Set("Content-Length", strconv.FormatInt(info.Size(), 10))
	w.Header().Set("X-Sync-Seq", strconv.FormatInt(seq, 10))
	io.Copy(w, f)
}

// ServeSync serves h on ln until ctx is done; pulls waiting then end.
func ServeSync(ctx context.Context, ln net.Listener, h http.Handler) error {
	srv := &http.Server{Handler: h, ReadHeaderTimeout: 10 * time.Second,
		BaseContext: func(net.Listener) context.Context { return ctx }}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(shutdownCtx)
	}()
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
