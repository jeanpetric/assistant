package server

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// Settings' Task kinds panel lists the kinds with their badges, adds one and
// deletes one through the API; task and reminder have no Delete. A kind
// without a colour of its own gets one from the palette, the same each time.
// Runs under node with a fake DOM.
func TestKindsPanel(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	js, err := webFS.ReadFile("web/app.js")
	if err != nil {
		t.Fatal(err)
	}
	cut := func(from, to string) []byte {
		start, end := bytes.Index(js, []byte(from)), bytes.Index(js, []byte(to))
		if start < 0 || end < start {
			t.Fatalf("app.js lacks %q … %q", from, to)
		}
		return js[start:end]
	}
	code := append(cut("const styledKinds", "function dueBadge("), cut("function kindsPanel(", "// ---------- skills ----------")...)
	for _, want := range []string{"fields,\n    kindsPanel(kinds, rerender)));", "state.meta.kinds = state.board.kinds;"} {
		if !bytes.Contains(js, []byte(want)) {
			t.Fatalf("app.js lacks %q", want)
		}
	}
	src := filepath.Join(t.TempDir(), "kinds.js")
	if err := os.WriteFile(src, code, 0o600); err != nil {
		t.Fatal(err)
	}
	script := `
const fail = msg => { console.log(msg); process.exitCode = 1; };
function h(tag, attrs, ...children) {
  attrs = attrs || {};
  const on = {};
  for (const [k, v] of Object.entries(attrs)) if (k.startsWith('on')) on[k.slice(2)] = v;
  return {
    tag, attrs, value: attrs.value || '', children: children.flat(Infinity).filter(c => c != null && c !== false),
    addEventListener(ev, fn) { on[ev] = fn; },
    fire(ev, e) { return on[ev]({ preventDefault() {}, stopPropagation() {}, ...e }); },
    get text() { return this.children.map(c => typeof c === 'object' ? c.text : String(c)).join(''); },
  };
}
const findAll = (el, pred, out = []) => {
  if (typeof el !== 'object') return out;
  if (pred(el)) out.push(el);
  for (const c of el.children) findAll(c, pred, out);
  return out;
};
let confirmed = [];
const confirm = msg => { confirmed.push(msg); return true; };
const state = { meta: { kinds: [] } };
const calls = [];
const list = [{ name: 'task', built_in: true, tasks: 3, rules: 0, automations: 0 }, { name: 'coding', built_in: false, tasks: 0, rules: 2, automations: 1 },
  { name: 'reminder', built_in: true, tasks: 0, rules: 0, automations: 0 }, { name: 'bug', built_in: false, tasks: 2, rules: 0, automations: 0 }];
const api = async (method, path, body) => { calls.push([method, path, body]); return list; };
const ui = eval(require('fs').readFileSync(process.argv.at(-1), 'utf8') + '; ({ panel: kindsPanel, badge: kindBadge, palette: kindPalette })');

(async () => {
  if (ui.badge('task') !== null) fail('task has a badge');
  if (ui.badge('coding').attrs.style) fail('coding lost its own colour');
  const bug = ui.badge('bug');
  if (!ui.palette.some(c => bug.attrs.style === 'color:' + c)) fail('bug colour ' + bug.attrs.style);
  if (ui.badge('bug').attrs.style !== bug.attrs.style) fail('colour changes');
  if (ui.badge('feature').attrs.style === bug.attrs.style && ui.badge('chore').attrs.style === bug.attrs.style) fail('one colour for all');

  let rendered = 0;
  const panel = ui.panel(list, () => rendered++);
  const rows = findAll(panel, e => e.tag === 'tr');
  if (rows.map(r => r.children[0].text).join() !== 'task,coding,reminder,bug') fail('rows ' + rows.map(r => r.text));
  const deletes = rows.map(r => findAll(r, e => e.tag === 'button' && e.text === 'Delete')[0]);
  if (deletes[0] || deletes[2] || !deletes[1] || !deletes[3]) fail('Delete buttons on ' + deletes.map(Boolean));
  if (!rows[1].text.includes('2 rules, 1 skill automation')) fail('uses ' + rows[1].text);

  await deletes[3].fire('click');
  if (!confirmed[0].includes('Its 2 tasks become kind task')) fail('confirm ' + confirmed[0]);
  if (JSON.stringify(calls[0]) !== JSON.stringify(['DELETE', '/api/kinds/bug', undefined])) fail('delete ' + JSON.stringify(calls[0]));
  if (state.meta.kinds.join() !== 'task,coding,reminder,bug' || rendered !== 1) fail('after delete ' + state.meta.kinds);

  const input = findAll(panel, e => e.tag === 'input')[0];
  input.value = 'feature';
  await findAll(panel, e => e.tag === 'button' && e.text === 'Add')[0].fire('click');
  if (JSON.stringify(calls[1]) !== JSON.stringify(['POST', '/api/kinds', { name: 'feature' }])) fail('add ' + JSON.stringify(calls[1]));
  input.value = 'x';
  await input.fire('keydown', { key: 'Enter' });
  if (calls.length !== 3 || rendered !== 3) fail('Enter adds ' + calls.length);
})();
`
	out, err := exec.Command(node, "-e", script, src).CombinedOutput()
	if err != nil || len(out) > 0 {
		t.Fatalf("node: %v\n%s", err, out)
	}
}
