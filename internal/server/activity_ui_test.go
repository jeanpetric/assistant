package server

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The Activity tab lists the thread with each agent's session as a link, an
// open question stands out with Answer, and the message box answers the
// latest open question unless another is picked. Runs under node with a fake DOM.
func TestActivityPanel(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	js, err := webFS.ReadFile("web/app.js")
	if err != nil {
		t.Fatal(err)
	}
	start := bytes.Index(js, []byte("function activityPanel("))
	end := bytes.Index(js, []byte("// The tasks this one builds on, editable"))
	if start < 0 || end < start {
		t.Fatal("app.js lacks activityPanel")
	}
	for _, want := range []string{
		"const activity = activityPanel(id, data.activity, { openSession: sid => focus(sid), onChange: () => drawTabs() });",
		"activity.reload();",
		"if (n.task_id && n.message.startsWith('Question from ')) openTaskDialog(n.task_id, 'activity');",
	} {
		if !bytes.Contains(js, []byte(want)) {
			t.Fatalf("app.js lacks %q", want)
		}
	}
	src := filepath.Join(t.TempDir(), "activity.js")
	if err := os.WriteFile(src, js[start:end], 0o600); err != nil {
		t.Fatal(err)
	}
	script := `
const fail = msg => { console.log(msg); process.exitCode = 1; };
function h(tag, attrs, ...children) {
  attrs = attrs || {};
  const cls = new Set(String(attrs.class || '').split(' ').filter(Boolean));
  const on = {};
  for (const [k, v] of Object.entries(attrs)) if (k.startsWith('on')) on[k.slice(2)] = v;
  const el = {
    tag, attrs, value: '', focused: false, disabled: false, children: children.flat(Infinity).filter(c => c != null && c !== false),
    classList: { contains: c => cls.has(c), toggle: (c, f) => f ? cls.add(c) : cls.delete(c) },
    replaceChildren(...k) { this.children = k.flat(Infinity).filter(c => c != null && c !== false); },
    addEventListener(ev, fn) { on[ev] = fn; },
    focus() { this.focused = true; },
    scrollIntoView() {},
    fire(ev) { return on[ev]({ preventDefault() {} }); },
    get text() { return this.children.map(c => typeof c === 'object' ? c.text : String(c)).join(''); },
  };
  return el;
}
const find = (el, pred) => {
  if (typeof el !== 'object') return null;
  if (pred(el)) return el;
  for (const c of el.children) { const f = find(c, pred); if (f) return f; }
  return null;
};
const all = (el, pred, out = []) => {
  if (typeof el !== 'object') return out;
  if (pred(el)) out.push(el);
  el.children.forEach(c => all(c, pred, out));
  return out;
};
const hint = () => null, ago = () => '1m ago', fmtTime = () => 'time', trunc = s => s, toast = () => {};
const markdown = text => ['md:' + text];
const submitOnCmdEnter = () => {};
let thread = [
  { id: 1, session_id: 41, session_title: 'Guard', body: 'Spec rejected.', question: false, answered_at: null },
  { id: 2, session_id: 42, session_title: 'Spec refine', body: 'Show it?', question: true, answered_at: null },
  { id: 3, session_id: 42, session_title: 'Spec refine', body: 'Which name?', question: true, answered_at: null },
  { id: 4, session_id: null, session_title: '', body: 'fyi', question: false, answered_at: null },
];
const posts = [];
const api = async (method, path, body) => {
  if (method === 'POST') { posts.push([path, body]); return { answered_id: body.answer_to || 3, queued: false }; }
  return thread;
};
const opened = [];
let changes = 0;
const panel = eval(require('fs').readFileSync(process.argv.at(-1), 'utf8') + '; activityPanel')(7, thread,
  { openSession: id => opened.push(id), onChange: () => changes++ });
(async () => {
  if (panel.count() !== 4 || panel.openCount() !== 2) fail('counts ' + panel.count() + ' ' + panel.openCount());
  const msgs = all(panel.el, e => e.attrs.class && e.attrs.class.startsWith('activity-msg'));
  if (msgs.length !== 4) fail('messages ' + msgs.length);
  if (msgs[0].attrs.class !== 'activity-msg' || msgs[1].attrs.class !== 'activity-msg open' || msgs[3].attrs.class !== 'activity-msg mine') fail('classes ' + msgs.map(m => m.attrs.class));
  const link = find(msgs[0], e => e.tag === 'a');
  if (link.text !== 'Guard · #41') fail('author ' + link.text);
  link.fire('click');
  if (opened.join() !== '41') fail('opened ' + opened);
  if (find(msgs[3], e => e.tag === 'strong').text !== 'You') fail('the user is not You');
  if (find(msgs[0], e => e.tag === 'button')) fail('a plain message has Answer');
  const target = find(panel.el, e => e.attrs.class === 'muted small activity-target');
  if (!target.text.includes('Which name?')) fail('target ' + target.text);

  const input = find(panel.el, e => e.tag === 'textarea');
  const send = find(panel.el, e => e.tag === 'button' && e.text === 'Send');
  find(msgs[1], e => e.tag === 'button' && e.text === 'Answer').fire('click');
  if (!target.text.includes('Show it?') || !input.focused) fail('Answer should pick its question: ' + target.text);
  input.value = ' show it ';
  thread = thread.map(m => m.id === 2 ? { ...m, answered_at: 'now' } : m).concat([{ id: 5, session_id: null, body: 'show it', question: false }]);
  await send.fire('click');
  if (JSON.stringify(posts[0]) !== JSON.stringify(['/api/tasks/7/messages', { body: 'show it', answer_to: 2 }])) fail('posted ' + JSON.stringify(posts));
  if (input.value !== '' || panel.count() !== 5 || panel.openCount() !== 1 || changes !== 1) fail('after send ' + panel.count() + ' ' + panel.openCount() + ' ' + changes);
  if (!target.text.includes('Which name?')) fail('target after send ' + target.text);

  input.value = 'next';
  await send.fire('click');
  if (JSON.stringify(posts[1][1]) !== JSON.stringify({ body: 'next' })) fail('the latest question goes without answer_to ' + JSON.stringify(posts[1]));
  thread = thread.map(m => ({ ...m, answered_at: m.question ? 'now' : null }));
  await panel.reload();
  if (!target.text.includes('No open question')) fail('target with none open ' + target.text);
})();
`
	out, err := exec.Command(node, "-e", script, "--", src).CombinedOutput()
	if err != nil || len(out) > 0 {
		t.Fatalf("activityPanel: %v\n%s", err, strings.TrimSpace(string(out)))
	}
}
