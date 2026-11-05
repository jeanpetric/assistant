package server

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// Settings' Session prompts panel shows each prompt with its projects ticked,
// saves name, text and projects through the API, adds and deletes prompts.
// Runs under node with a fake DOM.
func TestSessionPromptsPanel(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	js, err := webFS.ReadFile("web/app.js")
	if err != nil {
		t.Fatal(err)
	}
	start, end := bytes.Index(js, []byte("function sessionPromptsPanel(")), bytes.Index(js, []byte("// The lane a task made for a new agent session"))
	if start < 0 || end < start {
		t.Fatal("app.js lacks sessionPromptsPanel")
	}
	if !bytes.Contains(js, []byte("sessionPromptsPanel(prompts, b.projects, rerender)")) || !bytes.Contains(js, []byte("api('GET', '/api/session-prompts')")) {
		t.Fatal("settings don't show the session prompts")
	}
	src := filepath.Join(t.TempDir(), "prompts.js")
	if err := os.WriteFile(src, js[start:end], 0o600); err != nil {
		t.Fatal(err)
	}
	script := `
const fail = msg => { console.log(msg); process.exitCode = 1; };
function h(tag, attrs, ...children) {
  attrs = attrs || {};
  const on = {};
  for (const [k, v] of Object.entries(attrs)) if (k.startsWith('on')) on[k.slice(2)] = v;
  return {
    tag, attrs, value: attrs.value || '', checked: !!attrs.checked, disabled: !!attrs.disabled,
    children: children.flat(Infinity).filter(c => c != null && c !== false),
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
const confirm = () => true;
const toast = () => {};
const calls = [];
const api = async (method, path, body) => { calls.push([method, path, body]); return {}; };
const panel = eval(require('fs').readFileSync(process.argv.at(-1), 'utf8') + '; sessionPromptsPanel');
const projects = [{ id: 1, name: 'Assistant' }, { id: 7, name: 'Web' }];
const prompts = [{ id: 3, name: 'Default', prompt: 'All {{.Project.Name}}', all_projects: true, project_ids: [] },
  { id: 5, name: 'Web', prompt: 'web', all_projects: false, project_ids: [7] }];

(async () => {
  let rendered = 0;
  const p = panel(prompts, projects, () => rendered++);
  const items = findAll(p, e => e.attrs.class === 'session-prompt');
  if (items.length !== 2) fail('items ' + items.length);
  const boxes = items.map(it => findAll(it, e => e.tag === 'input' && e.attrs.type === 'checkbox'));
  if (boxes[0].map(b => b.checked).join() !== 'true,false,false' || !boxes[0][1].disabled) fail('default boxes');
  if (boxes[1].map(b => b.checked).join() !== 'false,false,true' || boxes[1][1].disabled) fail('web boxes');
  if (findAll(items[1], e => e.tag === 'textarea')[0].value !== 'web') fail('text');
  if (!items[1].text.includes('All projects') || !items[1].text.includes('Assistant')) fail('labels ' + items[1].text);

  // Ticking All projects disables the project boxes; unticking enables them.
  boxes[0][0].checked = false;
  boxes[0][0].fire('change');
  if (boxes[0][1].disabled) fail('still disabled');

  boxes[1][1].checked = true;
  findAll(items[1], e => e.tag === 'input' && !e.attrs.type)[0].value = 'Web+';
  await findAll(items[1], e => e.tag === 'button' && e.text === 'Save')[0].fire('click');
  const want = ['PATCH', '/api/session-prompts/5', { name: 'Web+', prompt: 'web', all_projects: false, project_ids: [1, 7] }];
  if (JSON.stringify(calls[0]) !== JSON.stringify(want)) fail('save ' + JSON.stringify(calls[0]));

  await findAll(items[0], e => e.tag === 'button' && e.text === 'Delete')[0].fire('click');
  if (JSON.stringify(calls[1]) !== JSON.stringify(['DELETE', '/api/session-prompts/3', undefined]) || rendered !== 1) fail('delete ' + JSON.stringify(calls[1]));

  await findAll(p, e => e.tag === 'button' && e.text === '+ Add prompt')[0].fire('click');
  if (calls[2][0] !== 'POST' || calls[2][1] !== '/api/session-prompts' || calls[2][2].all_projects !== false || rendered !== 2) fail('add ' + JSON.stringify(calls[2]));

  if (!panel([], projects, () => {}).text.includes('No session prompts')) fail('empty');
})();
`
	out, err := exec.Command(node, "-e", script, src).CombinedOutput()
	if err != nil || len(out) > 0 {
		t.Fatalf("node: %v\n%s", err, out)
	}
}
