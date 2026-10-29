package main

import (
	"bytes"
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/jeanpetric/assistant/internal/config"
	"github.com/jeanpetric/assistant/internal/server"
	"github.com/jeanpetric/assistant/internal/store"
	"github.com/jeanpetric/assistant/internal/syncauth"
)

// machine is a computer's ~/.assistant.
type machine struct {
	t    *testing.T
	path string // config.json
}

func newMachine(t *testing.T, settings map[string]string) *machine {
	t.Helper()
	dir := t.TempDir()
	m := &machine{t: t, path: filepath.Join(dir, "config.json")}
	if err := config.Set(m.path, "db", filepath.Join(dir, "assistant.db")); err != nil {
		t.Fatal(err)
	}
	if err := config.Set(m.path, "sync_key", filepath.Join(dir, "sync_ed25519")); err != nil {
		t.Fatal(err)
	}
	for k, v := range settings {
		if err := config.Set(m.path, k, v); err != nil {
			t.Fatal(err)
		}
	}
	return m
}

func (m *machine) cfg() config.Config {
	m.t.Helper()
	c, err := config.Load(m.path, m.t.TempDir())
	if err != nil {
		m.t.Fatal(err)
	}
	return c
}

// run runs assistant sync args and returns what it printed and its error.
func (m *machine) run(args ...string) (string, error) {
	var out bytes.Buffer
	err := runSync(context.Background(), m.cfg(), m.path, args, &out)
	return out.String(), err
}

func (m *machine) must(args ...string) string {
	m.t.Helper()
	out, err := m.run(args...)
	if err != nil {
		m.t.Fatalf("sync %v: %v\n%s", args, err, out)
	}
	return out
}

func (m *machine) store() *store.Store {
	m.t.Helper()
	st, err := store.Open(m.cfg().DB)
	if err != nil {
		m.t.Fatal(err)
	}
	m.t.Cleanup(func() { st.Close() })
	return st
}

var pubkeyLine = regexp.MustCompile(`(?m)^  (\S+)$`)

func TestSyncSetUp(t *testing.T) {
	ctx := context.Background()
	hub := newMachine(t, map[string]string{"sync_listen": "127.0.0.1:0"})
	hubStore := hub.store()
	if err := hubStore.SetSyncEnabled(ctx, true); err != nil {
		t.Fatal(err)
	}
	hubStore.SetHub(true)
	srv := httptest.NewServer(server.NewSyncHub(hubStore).Handler(syncauth.NewVerifier(hubStore.SyncKeyNode)))
	t.Cleanup(srv.Close)

	laptop := newMachine(t, nil)
	if _, err := laptop.store().CreateTask(ctx, store.Task{Title: "my data"}); err != nil {
		t.Fatal(err)
	}
	if _, err := laptop.run("seed"); err == nil || !strings.Contains(err.Error(), "sync init") {
		t.Fatalf("seed before init: %v", err)
	}
	if _, err := laptop.run("init"); err == nil {
		t.Fatal("init without --server")
	}
	out := laptop.must("init", "--server", srv.URL+"/")
	if got := laptop.cfg().SyncServer; got != srv.URL {
		t.Fatalf("sync_server %q", got)
	}
	m := pubkeyLine.FindStringSubmatch(out)
	if m == nil || syncauth.CheckPublicKey(m[1]) != nil {
		t.Fatalf("no public key in\n%s", out)
	}
	pub := m[1]
	// Again: the same key.
	if again := laptop.must("init", "--server", srv.URL); !strings.Contains(again, pub) {
		t.Fatalf("init made a new key:\n%s", again)
	}
	if _, err := hub.run("init", "--server", srv.URL); err == nil {
		t.Fatal("init on the hub")
	}

	if _, err := laptop.run("seed"); err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("seed before authorize: %v", err)
	}
	if _, err := laptop.run("authorize", pub, "--name", "laptop"); err == nil {
		t.Fatal("authorized off the hub")
	}
	if _, err := hub.run("authorize", "not a key", "--name", "x"); err == nil {
		t.Fatal("authorized a bad key")
	}
	if out := hub.must("authorize", pub, "--name", "laptop"); !strings.Contains(out, "node 2") {
		t.Fatalf("authorize: %s", out)
	}

	out = laptop.must("seed")
	backups, _ := filepath.Glob(filepath.Join(filepath.Dir(laptop.cfg().DB), "backups", "assistant-*.db"))
	if len(backups) != 1 || !strings.Contains(out, backups[0]) || !strings.Contains(out, "node 2") {
		t.Fatalf("seed printed\n%s\nbackups %v", out, backups)
	}
	if all, _ := hubStore.SearchTasks(ctx, "my data", 10); len(all) != 1 {
		t.Fatalf("hub has %d seeded tasks", len(all))
	}

	desktop := newMachine(t, nil)
	out = desktop.must("init", "--server", srv.URL)
	m = pubkeyLine.FindStringSubmatch(out)
	flagFirst := hub.must("authorize", "--name", "desktop", m[1])
	if !strings.Contains(flagFirst, "node 3") {
		t.Fatalf("authorize: %s", flagFirst)
	}
	if out := desktop.must("join"); !strings.Contains(out, "node 3") {
		t.Fatalf("join: %s", out)
	}
	if all, _ := desktop.store().SearchTasks(ctx, "my data", 10); len(all) != 1 {
		t.Fatalf("desktop has %d tasks", len(all))
	}

	if out := hub.must("keys"); !strings.Contains(out, "laptop") || !strings.Contains(out, "desktop") {
		t.Fatalf("keys:\n%s", out)
	}
	if out := hub.must("revoke", "desktop"); !strings.Contains(out, "node 3") {
		t.Fatalf("revoke: %s", out)
	}
	if _, err := hub.run("revoke", "desktop"); err == nil {
		t.Fatal("revoked twice")
	}
	if out := hub.must("keys"); !strings.Contains(out, "revoked") {
		t.Fatalf("keys:\n%s", out)
	}
	if _, err := desktop.run("join"); err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("join with a revoked key: %v", err)
	}
	if _, err := hub.run("bogus"); err == nil {
		t.Fatal("unknown command ran")
	}
	if _, err := os.Stat(laptop.cfg().SyncKey); err != nil {
		t.Fatal(err)
	}
}
