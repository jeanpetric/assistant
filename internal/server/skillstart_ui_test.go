package server

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The task's New agent session panel has a Run skill dropdown: picking a skill
// starts a new session with it at once, the prompt (optional) and the panel's
// settings going along; Start still needs a prompt. Runs under node with a fake DOM.
func TestNewSessionPanelRunSkill(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	js, err := webFS.ReadFile("web/app.js")
	if err != nil {
		t.Fatal(err)
	}
	start := bytes.Index(js, []byte("function newSessionPanel("))
	end := bytes.Index(js, []byte("// 7s, 2m 05s, 1h 02m"))
	if start < 0 || end < start {
		t.Fatal("app.js lacks newSessionPanel")
	}
	src := filepath.Join(t.TempDir(), "panel.js")
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
    tag, attrs, value: attrs.value || '', checked: !!attrs.checked, textContent: '',
    children: children.flat(Infinity).filter(c => c != null && c !== false),
    addEventListener(ev, fn) { on[ev] = fn; },
    fire(ev, e) { return on[ev](Object.assign({ target: this, preventDefault() {} }, e)); },
  };
}
const select = (options, value, attrs) => Object.assign(h('select', attrs || {}), { value: String(value), options });
const find = (el, pred) => {
  if (!el || typeof el !== 'object') return null;
  if (pred(el)) return el;
  for (const c of el.children) { const f = find(c, pred); if (f) return f; }
  return null;
};
const hint = () => null, modeOptions = () => [], taskWorkdir = () => '~/w';
const modelInput = () => [Object.assign(h('input'), { value: 'fable' }), h('datalist')];
const agentSelect = () => Object.assign(h('select'), { value: 'claude' });
const workdirSelect = () => Object.assign(h('select'), { value: '' });
const doneLaneSelect = () => h('select'), doneLaneValue = () => 4;
const state = { meta: { default_mode: 'auto' }, board: { all_lanes: [] }, skills: [{ id: 7, name: 'Refine' }] };
const posts = [];
const api = async (method, path, body) => {
  if (method === 'POST') { posts.push([path, body]); return { id: 99 }; }
  return { session: { id: 99 }, messages: [] };
};
const started = [];
const task = { id: 5, context_from: [], memories: [] };
const panel = eval(require('fs').readFileSync(process.argv.at(-1), 'utf8') + '; newSessionPanel')(task, s => started.push(s.id));
(async () => {
  const skill = find(panel.el, e => e.tag === 'select' && e.options && e.options.length && e.options[0][1] === 'Run skill…');
  if (!skill || skill.options.length !== 2 || skill.options[1][0] !== 7) fail('no Run skill dropdown');
  const startBtn = find(panel.el, e => e.tag === 'button');
  await startBtn.fire('click');
  if (posts.length) fail('Start without a prompt posted ' + JSON.stringify(posts));

  skill.value = '7';
  await skill.fire('change');
  const b = posts[0] && posts[0][1];
  if (!b || posts[0][0] !== '/api/tasks/5/sessions' || b.skill_id !== 7 || b.prompt !== '' || b.model !== 'fable' || b.done_lane_id !== 4)
    fail('skill start posted ' + JSON.stringify(posts));
  if (skill.value !== '' || started.join() !== '99') fail('after skill start ' + skill.value + ' ' + started);

  const prompt = find(panel.el, e => e.tag === 'textarea');
  prompt.value = 'focus on tests';
  await startBtn.fire('click');
  if (posts.length !== 2 || 'skill_id' in posts[1][1] || posts[1][1].prompt !== 'focus on tests') fail('plain start posted ' + JSON.stringify(posts[1]));

  state.skills = [];
  const bare = eval(require('fs').readFileSync(process.argv.at(-1), 'utf8') + '; newSessionPanel')(task, () => {});
  if (find(bare.el, e => e.tag === 'select' && e.options && e.options.length && e.options[0][1] === 'Run skill…')) fail('dropdown without skills');
})();
`
	out, err := exec.Command(node, "-e", script, "--", src).CombinedOutput()
	if err != nil || len(out) > 0 {
		t.Fatalf("newSessionPanel: %v\n%s", err, strings.TrimSpace(string(out)))
	}
}
