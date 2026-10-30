package server

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// The task dialog's Slack section connects a channel, with the orchestrator
// picked or the latest session, then shows it as a link with its sync status
// and Disconnect; a card shows the channel as a badge. Runs under node with a
// fake DOM.
func TestSlackEditor(t *testing.T) {
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
	code := append(cut("function slackBadge(", "function kindBadge("), cut("function slackEditor(", "// The tasks this one builds on, editable")...)
	for _, want := range []string{
		"slackBadge(t), fieldLinks(t)),\n    runningMeta(",
		"    buildsOn.el,\n    slack.el,\n",
		"every(5000, () => slack.reload());",
	} {
		if !bytes.Contains(js, []byte(want)) {
			t.Fatalf("app.js lacks %q", want)
		}
	}
	src := filepath.Join(t.TempDir(), "slack.js")
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
    // As the DOM's: nodes and strings only, no arrays or null.
    replaceChildren(...k) { if (k.some(c => c == null || Array.isArray(c))) fail('replaceChildren with ' + k); this.children = k; },
    addEventListener(ev, fn) { on[ev] = fn; },
    fire(ev) { return on[ev]({ preventDefault() {}, stopPropagation() {} }); },
    get text() { return this.children.map(c => typeof c === 'object' ? c.text : String(c)).join(''); },
  };
}
function select(options, value, attrs) {
  const el = h('select', attrs, options.map(([v, label]) => h('option', { value: v }, label)));
  el.value = String(value);
  return el;
}
const find = (el, pred) => {
  if (typeof el !== 'object') return null;
  if (pred(el)) return el;
  for (const c of el.children) { const f = find(c, pred); if (f) return f; }
  return null;
};
const ago = () => '1m ago', toast = () => {}, confirm = () => true;
global.document = { activeElement: null };
const calls = [];
const connected = { channel_id: 'C1', channel_name: 'help', session_id: 41, url: 'https://acme.slack.com/archives/C1', last_sync_at: 'now', error: '' };
let polled = connected;
const api = async (method, path, body) => {
  calls.push([method, path, body]);
  if (method === 'POST') return { id: 7, slack: { ...connected, session_id: body.session_id || 42, last_sync_at: null } };
  if (method === 'DELETE') return { id: 7, slack: null };
  return { task: { id: 7, slack: polled } };
};
const ui = eval(require('fs').readFileSync(process.argv.at(-1), 'utf8') + '; ({ badge: slackBadge, editor: slackEditor })');

(async () => {
  if (ui.badge({ slack: null }) !== null) fail('a badge without a channel');
  const badge = ui.badge({ slack: connected });
  if (badge.tag !== 'a' || badge.text !== '#help' || badge.attrs.href !== connected.url || badge.attrs.class !== 'badge slack') fail('badge ' + JSON.stringify(badge.attrs));
  if (ui.badge({ slack: { ...connected, error: 'ratelimited' } }).attrs.class !== 'badge slack failed') fail('a failing badge');

  const task = { id: 7, slack: null };
  const sessions = [{ id: 41, title: 'Older', owned: true }, { id: 43, title: 'Elsewhere', owned: false }, { id: 42, title: 'Orchestrator', owned: true }];
  const ed = ui.editor(task, sessions);
  const input = find(ed.el, e => e.tag === 'input');
  const sel = find(ed.el, e => e.tag === 'select');
  if (!input || !sel) fail('no channel field or orchestrator select');
  if (sel.children.map(o => o.text).join() !== 'Latest session,Orchestrator,Older') fail('orchestrators ' + sel.children.map(o => o.text));
  if (ed.el.text.includes('connecting starts')) fail('a new orchestrator promised with sessions');
  // A task without a session here connects too: the app starts its orchestrator.
  const bare = ui.editor({ id: 8, slack: null }, [{ id: 43, title: 'Elsewhere', owned: false }]);
  if (!bare.el.text.includes('connecting starts its orchestrator') || !find(bare.el, e => e.tag === 'button' && e.text === 'Connect')) fail('empty task ' + bare.el.text);
  await ed.reload();
  if (calls.length) fail('polled while not connected');

  input.value = 'https://acme.slack.com/archives/C1';
  await find(ed.el, e => e.tag === 'button' && e.text === 'Connect').fire('click');
  if (JSON.stringify(calls[0]) !== JSON.stringify(['POST', '/api/tasks/7/slack', { channel: 'https://acme.slack.com/archives/C1' }])) fail('connect ' + JSON.stringify(calls[0]));
  const link = find(ed.el, e => e.tag === 'a');
  if (!link || link.text !== '#help' || link.attrs.href !== connected.url) fail('channel link');
  if (!ed.el.text.includes('Not synced yet') || !ed.el.text.includes('orchestrator: Orchestrator')) fail('status ' + ed.el.text);
  if (find(ed.el, e => e.tag === 'input')) fail('channel field while connected');

  polled = { ...connected, error: 'channel_not_found' };
  await ed.reload();
  if (!ed.el.text.includes('Sync failed: channel_not_found') || !ed.el.text.includes('orchestrator: Older')) fail('after reload ' + ed.el.text);

  await find(ed.el, e => e.tag === 'button' && e.text === 'Disconnect').fire('click');
  if (JSON.stringify(calls.at(-1)) !== JSON.stringify(['DELETE', '/api/tasks/7/slack', undefined])) fail('disconnect ' + JSON.stringify(calls.at(-1)));
  if (task.slack !== null || !find(ed.el, e => e.tag === 'button' && e.text === 'Connect')) fail('not back to Connect');

  const input2 = find(ed.el, e => e.tag === 'input');
  input2.value = 'C1';
  find(ed.el, e => e.tag === 'select').value = '41';
  await find(ed.el, e => e.tag === 'button' && e.text === 'Connect').fire('click');
  if (JSON.stringify(calls.at(-1)[2]) !== JSON.stringify({ channel: 'C1', session_id: 41 })) fail('picked orchestrator ' + JSON.stringify(calls.at(-1)));
})();
`
	out, err := exec.Command(node, "-e", script, src).CombinedOutput()
	if err != nil || len(out) > 0 {
		t.Fatalf("node: %v\n%s", err, out)
	}
}
