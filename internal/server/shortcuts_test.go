package server

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The shortcut table and shortcutFor run under node: ⌥+key is matched by the physical key even when
// ⌥ turns the character into "∫" or the like, works in text fields, and plain keys never fire there.
func TestShortcutKeys(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	js, err := webFS.ReadFile("web/app.js")
	if err != nil {
		t.Fatal(err)
	}
	start := bytes.Index(js, []byte("const shortcuts = ["))
	end := bytes.Index(js, []byte("function onShortcut("))
	if start < 0 || end < start {
		t.Fatal("app.js lacks the shortcut table or shortcutFor")
	}
	// The table names functions defined elsewhere in app.js; a proxy stands in for them.
	script := `
const src = require('fs').readFileSync(process.argv.at(-1), 'utf8');
const own = new Set(['src', 'shortcuts', 'shortcutFor', 'taskHash', 'taskLink', 'pickedProjectHash', 'submitOnCmdEnter']);
const stubs = new Proxy({}, { has: (_, k) => !own.has(k) && !(k in globalThis), get: () => () => {} });
const api = (function () { with (stubs) return eval(src + '; ({ shortcutFor, taskHash, taskLink, pickedProjectHash, submitOnCmdEnter })'); })();
const key = (code, key, mods = {}) => ({ code, key, altKey: false, shiftKey: false, metaKey: false, ctrlKey: false, repeat: false, ...mods });
const cases = [
  [key('KeyB', '∫', { altKey: true }), true, '⌥B'],
  [key('KeyB', '∫', { altKey: true }), false, '⌥B'],
  [key('KeyL', '¬', { altKey: true }), true, '⌥L'],
  [key('KeyS', 'ß', { altKey: true }), true, '⌥S'],
  [key('KeyA', 'å', { altKey: true }), true, '⌥A'],
  [key('KeyO', 'ø', { altKey: true }), true, '⌥O'],
  [key('Comma', '≤', { altKey: true }), true, '⌥,'],
  [key('KeyT', '†', { altKey: true }), true, '⌥T'],
  [key('KeyK', '˚', { altKey: true }), false, '⌥K'],
  [key('KeyC', 'ç', { altKey: true }), true, '⌥C'],
  [key('KeyC', 'ç', { altKey: true }), false, '⌥C'],
  [key('KeyW', '∑', { altKey: true }), true, '⌥W'],
  [key('KeyJ', '∆', { altKey: true }), true, '⌥J'],
  [key('BracketRight', '‘', { altKey: true }), true, '⌥]'],
  [key('KeyH', '˙', { altKey: true }), true, '⌥H'],
  [key('KeyH', '˙', { altKey: true }), false, '⌥H'],
  [key('Slash', '/'), false, '/'],
  [key('Slash', '?', { shiftKey: true }), false, '?'],
  [key('Slash', '/'), true, null],
  [key('KeyB', 'b'), false, null],
  [key('KeyB', 'b'), true, null],
  [key('KeyG', 'g'), false, null],
  [key('KeyE', '´', { altKey: true }), true, null],
  [key('KeyB', 'ı', { altKey: true, shiftKey: true }), true, null],
  [key('KeyB', 'b', { altKey: true, metaKey: true }), true, null],
  [key('KeyB', 'b', { ctrlKey: true }), true, null],
  [key('KeyA', 'a', { ctrlKey: true }), true, null],
  [key('KeyB', '∫', { altKey: true, repeat: true }), true, null],
  [key('Escape', 'Escape'), true, null],
];
for (const [hash, want] of [['#/list', '#/list'], ['#/board', '#/board'], ['', '#/board'],
    ['#/archive', '#/board'], ['#/task/3', '#/board'], ['#/listing', '#/board']]) {
  const got = api.pickedProjectHash(hash);
  if (got !== want) {
    console.log(JSON.stringify({ hash, want, got }));
    process.exitCode = 1;
  }
}
// taskHash: a task link, with or without a session, names what the task dialog opens; other links name nothing.
for (const [hash, want] of [['#/task/7', { id: 7, session: 0 }], ['#/task/7/s/3', { id: 7, session: 3 }], ['#/board', null],
    ['', null], [undefined, null], ['#/session/5', null], ['#/tasks/7', null], ['#/task/7x', null], ['#/list', null]]) {
  const got = api.taskHash(hash);
  if (JSON.stringify(got) !== JSON.stringify(want)) {
    console.log(JSON.stringify({ hash, want, got }));
    process.exitCode = 1;
  }
}
if (api.taskLink('http://localhost:6767', 7) !== 'http://localhost:6767/#/task/7') {
  console.log('taskLink: ' + api.taskLink('http://localhost:6767', 7));
  process.exitCode = 1;
}
// submitOnCmdEnter: only ⌘↵ / Ctrl↵ submit, once per press, and the newline is not typed.
let listener, submits = 0;
api.submitOnCmdEnter([{ addEventListener: (type, fn) => { if (type === 'keydown') listener = fn; } }], () => submits++);
for (const [e, want] of [[key('Enter', 'Enter', { metaKey: true }), 1], [key('Enter', 'Enter', { ctrlKey: true }), 1],
    [key('Enter', 'Enter'), 0], [key('Enter', 'Enter', { shiftKey: true }), 0], [key('KeyA', 'a', { metaKey: true }), 0],
    [key('Enter', 'Enter', { metaKey: true, repeat: true }), 0]]) {
  let prevented = false;
  submits = 0;
  listener({ ...e, preventDefault: () => { prevented = true; } });
  if (submits !== want || prevented !== (want === 1)) {
    console.log(JSON.stringify({ e, want, submits, prevented }));
    process.exitCode = 1;
  }
}
for (const [e, inField, want] of cases) {
  const got = api.shortcutFor(e, inField);
  if ((got ? got.keys : null) !== want) {
    console.log(JSON.stringify({ e, inField, want, got: got && got.keys }));
    process.exitCode = 1;
  }
}
`
	dir := t.TempDir()
	src := filepath.Join(dir, "shortcuts.js")
	if err := os.WriteFile(src, js[start:end], 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(node, "-e", script, "--", src).CombinedOutput()
	if err != nil {
		t.Fatalf("shortcutFor: %v\n%s", err, strings.TrimSpace(string(out)))
	}
}
