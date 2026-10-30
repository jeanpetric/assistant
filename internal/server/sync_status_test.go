package server

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jeanpetric/assistant/internal/app"
)

// GET /api/sync/status and sync_status give the node's role, and on a
// client how its sync goes.
func TestSyncStatusEndpoint(t *testing.T) {
	c := newClient(t)
	get := func() map[string]any {
		t.Helper()
		var out map[string]any
		c.do("GET", "/api/sync/status", nil, 200, &out)
		return out
	}
	if got := get(); len(got) != 1 || got["role"] != "off" {
		t.Fatalf("off: %v", got)
	}
	if text, isErr := c.tool("sync_status", map[string]any{}); isErr || !strings.Contains(text, `"role":"off"`) {
		t.Fatalf("tool off: %s", text)
	}
	c.app.SyncRole = app.RoleHub
	if got := get(); len(got) != 1 || got["role"] != "hub" {
		t.Fatalf("hub: %v", got)
	}
	c.app.SyncRole, c.app.SyncServer = app.RoleClient, "https://hub.example"
	if got := get(); got["role"] != "client" || got["server"] != "https://hub.example" || got["online"] != false ||
		!strings.Contains(got["error"].(string), "not in sync yet") {
		t.Fatalf("client not in sync: %v", got)
	}
	at := time.Now().Add(-time.Minute)
	c.app.SyncClient = func(context.Context) (app.ClientStatus, error) {
		return app.ClientStatus{Server: "https://hub.example", Online: true, Pending: 2, SyncedAt: &at}, nil
	}
	if got := get(); got["role"] != "client" || got["online"] != true || got["pending"] != float64(2) || got["synced_at"] == nil || got["error"] != nil {
		t.Fatalf("client: %v", got)
	}
	if text, isErr := c.tool("sync_status", map[string]any{}); isErr || !strings.Contains(text, `"pending":2`) {
		t.Fatalf("tool client: %s", text)
	}
}

// The header's sync indicator shows only on a client: online with the last
// sync, or offline, and the changes waiting. Runs under node with a fake element.
func TestSyncIndicator(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	js, err := webFS.ReadFile("web/app.js")
	if err != nil {
		t.Fatal(err)
	}
	html, _ := webFS.ReadFile("web/index.html")
	if !bytes.Contains(html, []byte(`<span id="sync-status" class="sync-status hidden"></span>`)) {
		t.Fatal("index.html lacks the sync indicator")
	}
	if !bytes.Contains(js, []byte("api('GET', '/api/sync/status').then(st => drawSyncStatus(document.getElementById('sync-status'), st))")) {
		t.Fatal("refreshGlobal does not draw the sync status")
	}
	start := bytes.Index(js, []byte("function drawSyncStatus("))
	end := bytes.Index(js[start:], []byte("\n}\n"))
	if start < 0 || end < 0 {
		t.Fatal("app.js lacks drawSyncStatus")
	}
	src := filepath.Join(t.TempDir(), "sync.js")
	if err := os.WriteFile(src, js[start:start+end+3], 0o600); err != nil {
		t.Fatal(err)
	}
	script := `
const fail = msg => { console.log(msg); process.exitCode = 1; };
const ago = ts => 'AGO';
const fmtTime = ts => 'TIME';
const el = () => { const cls = new Set(['sync-status', 'hidden']); return { textContent: '', title: '',
  classList: { toggle: (c, on) => on ? cls.add(c) : cls.delete(c), has: c => cls.has(c) } }; };
const draw = eval(require('fs').readFileSync(process.argv.at(-1), 'utf8') + '; drawSyncStatus');

for (const role of ['off', 'hub']) {
  const e = el(); draw(e, { role });
  if (!e.classList.has('hidden') || e.textContent) fail(role + ' shown: ' + e.textContent);
}
let e = el();
draw(e, { role: 'client', server: 'https://hub', online: true, pending: 0, synced_at: '2026-10-30T08:00:00Z' });
if (e.classList.has('hidden') || e.classList.has('offline') || e.textContent !== '● Synced AGO') fail('online: ' + e.textContent);
if (!e.title.includes('https://hub') || !e.title.includes('last sync TIME')) fail('title: ' + e.title);
draw(e, { role: 'client', server: 'https://hub', online: false, pending: 3, error: 'dial tcp: refused' });
if (!e.classList.has('offline') || e.textContent !== '○ Offline · 3 pending') fail('offline: ' + e.textContent);
if (!e.title.includes('never synced') || !e.title.includes('3 changes waiting') || !e.title.includes('dial tcp: refused')) fail('offline title: ' + e.title);
`
	out, err := exec.Command(node, "-e", script, "--", src).CombinedOutput()
	if err != nil || len(out) > 0 {
		t.Fatalf("%s %v", out, err)
	}
}
