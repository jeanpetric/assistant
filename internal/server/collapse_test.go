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

// A turn's prompt and its reply each fold to one line of their own, the reply
// keeps what arrives while folded, and both stay folded across reloads.
func TestTurnCollapse(t *testing.T) {
	js, err := webFS.ReadFile("web/app.js")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"if (m.role === 'user' && m.kind === 'text') { body.append(turnEl(m)); return; }",
		"const into = turn ? turn.querySelector('.turn-body') : body;",
		"turn.append(foldEl('prompt', m.id, messageEl(m)), foldEl('reply', m.id, h('div', { class: 'turn-body' })));",
		"fold.classList.toggle('collapsed', foldCollapsed(part, id));",
		"setFoldCollapsed(fold.dataset.part, fold.closest('.turn').dataset.id, on);",
		"onclick: () => flipFold(fold, false),",
		"onclick: () => flipFold(fold, true) }, h('span', { class: 'turn-caret' })),",
		"fold.querySelector(on ? '.turn-line' : '.turn-toggle').focus({ preventScroll: true });",
		"if (m.kind === 'result') turn.dataset.end = fmtTime(m.created_at);",
		"turn.querySelectorAll(':scope > .fold').forEach(f => setFold(f, on));",
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
		".fold.collapsed > :not(.turn-line) { display: none; }",
		".fold.collapsed > .turn-line { display: block; }",
		".fold.reply:has(> .turn-body:empty) { display: none; }",
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
		t.Fatal("app.js lacks foldCollapsed")
	}
	src := filepath.Join(t.TempDir(), "turns.js")
	if err := os.WriteFile(src, js[start:end], 0o600); err != nil {
		t.Fatal(err)
	}
	script := `
const m = new Map();
globalThis.localStorage = { getItem: k => m.get(k) ?? null, setItem: (k, v) => m.set(k, String(v)), removeItem: k => m.delete(k) };
const [is, set, isSession, line] = eval(require('fs').readFileSync(process.argv.at(-1), 'utf8') + '; [foldCollapsed, setFoldCollapsed, sessionCollapsed, replyLine]');
const check = (got, want, what) => { if (got !== want) { console.log(what, JSON.stringify(got), 'want', JSON.stringify(want)); process.exitCode = 1; } };
check(is('prompt', 40), false, 'new');
set('prompt', 40, true);
check(is('prompt', 40), true, 'prompt folded');
check(is('reply', 40), false, 'reply stays open');
check(is('prompt', 41), false, 'other turn');
check(isSession(40), false, 'session with the same id');
set('reply', 40, true);
check(is('reply', 40), true, 'reply folded');
set('prompt', 40, false);
check(is('prompt', 40), false, 'prompt expanded');
check(is('reply', 40), true, 'reply stays folded');
set('reply', 40, false);
check(m.size, 0, 'keys left');
check(line(0, ''), 'Reply', 'running, no tools');
check(line(1, ''), 'Reply · 1 tool call', 'one tool');
check(line(3, '14:02'), 'Reply · 3 tool calls · ✓ 14:02', 'finished');
`
	out, err := exec.Command(node, "-e", script, "--", src).CombinedOutput()
	if err != nil || len(out) > 0 {
		t.Fatalf("foldCollapsed: %v\n%s", err, strings.TrimSpace(string(out)))
	}
}
