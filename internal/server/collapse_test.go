package server

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// A session collapses to its head from the head's button or title, stays
// collapsed across reloads, and opens again when a link focuses it.
func TestSessionCollapse(t *testing.T) {
	js, err := webFS.ReadFile("web/app.js")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"el.classList.toggle('collapsed', sessionCollapsed(session.id));",
		"onclick: () => c.setCollapsed(!el.classList.contains('collapsed'))",
		"onclick: () => c.setCollapsed(false) }, '▸')",
		"onclick: () => c.setCollapsed(true) }, '▾')",
		"if (c.el.classList.contains('collapsed')) c.setCollapsed(false);",
	} {
		if !bytes.Contains(js, []byte(want)) {
			t.Fatalf("app.js lacks %q", want)
		}
	}
	css, err := webFS.ReadFile("web/style.css")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(css, []byte(".session.collapsed > :not(.session-head) { display: none; }")) {
		t.Fatal("style.css does not hide a collapsed session's parts")
	}

	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	start := bytes.Index(js, []byte("function remember("))
	end := bytes.Index(js, []byte("function trunc("))
	if start < 0 || end < start {
		t.Fatal("app.js lacks sessionCollapsed")
	}
	src := filepath.Join(t.TempDir(), "collapse.js")
	if err := os.WriteFile(src, js[start:end], 0o600); err != nil {
		t.Fatal(err)
	}
	script := `
const m = new Map();
globalThis.localStorage = { getItem: k => m.get(k) ?? null, setItem: (k, v) => m.set(k, String(v)), removeItem: k => m.delete(k) };
const [is, set] = eval(require('fs').readFileSync(process.argv.at(-1), 'utf8') + '; [sessionCollapsed, setSessionCollapsed]');
const check = (got, want, what) => { if (got !== want) { console.log(what, got, 'want', want); process.exitCode = 1; } };
check(is(7), false, 'new');
set(7, true);
check(is(7), true, 'collapsed');
check(is(8), false, 'other');
set(7, false);
check(is(7), false, 'expanded');
check(m.size, 0, 'keys left');
`
	out, err := exec.Command(node, "-e", script, "--", src).CombinedOutput()
	if err != nil || len(out) > 0 {
		t.Fatalf("sessionCollapsed: %v\n%s", err, strings.TrimSpace(string(out)))
	}
}

// A turn — a prompt and what follows it — folds to one line of its own, keeps
// what arrives while folded, and stays folded across reloads.
func TestTurnCollapse(t *testing.T) {
	js, err := webFS.ReadFile("web/app.js")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"if (m.role === 'user' && m.kind === 'text') { body.append(turnEl(m)); return; }",
		"const into = turn ? turn.lastElementChild : body;",
		"turn.classList.toggle('collapsed', turnCollapsed(m.id));",
		"onclick: () => setTurn(turn, false)",
		"onclick: () => setTurn(turn, true) }, '▾')",
		"if (m.kind === 'result') turn.dataset.end = fmtTime(m.created_at);",
		"[...body.querySelectorAll(':scope > .turn')].slice(0, -1).forEach(t => setTurn(t, true))",
	} {
		if !bytes.Contains(js, []byte(want)) {
			t.Fatalf("app.js lacks %q", want)
		}
	}
	css, err := webFS.ReadFile("web/style.css")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		".turn.collapsed > :not(.turn-line) { display: none; }",
		".turn.collapsed > .turn-line { display: block; }",
	} {
		if !bytes.Contains(css, []byte(want)) {
			t.Fatalf("style.css lacks %q", want)
		}
	}

	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	start := bytes.Index(js, []byte("function remember("))
	end := bytes.Index(js, []byte("// ---------- state & routing"))
	if start < 0 || end < start {
		t.Fatal("app.js lacks turnCollapsed")
	}
	src := filepath.Join(t.TempDir(), "turns.js")
	if err := os.WriteFile(src, js[start:end], 0o600); err != nil {
		t.Fatal(err)
	}
	script := `
const m = new Map();
globalThis.localStorage = { getItem: k => m.get(k) ?? null, setItem: (k, v) => m.set(k, String(v)), removeItem: k => m.delete(k) };
const [is, set, isSession, line] = eval(require('fs').readFileSync(process.argv.at(-1), 'utf8') + '; [turnCollapsed, setTurnCollapsed, sessionCollapsed, turnLine]');
const check = (got, want, what) => { if (got !== want) { console.log(what, JSON.stringify(got), 'want', JSON.stringify(want)); process.exitCode = 1; } };
check(is(40), false, 'new');
set(40, true);
check(is(40), true, 'collapsed');
check(is(41), false, 'other turn');
check(isSession(40), false, 'session with the same id');
set(40, false);
check(is(40), false, 'expanded');
check(m.size, 0, 'keys left');
check(line('Fix  the\nbug', 0, ''), 'Fix the bug', 'running, no tools');
check(line('Fix', 1, ''), 'Fix · 1 tool call', 'one tool');
check(line('Fix', 3, '14:02'), 'Fix · 3 tool calls · ✓ 14:02', 'finished');
check(line('x'.repeat(200), 0, '').length, 121, 'long prompt');
`
	out, err := exec.Command(node, "-e", script, "--", src).CombinedOutput()
	if err != nil || len(out) > 0 {
		t.Fatalf("turnCollapsed: %v\n%s", err, strings.TrimSpace(string(out)))
	}
}
