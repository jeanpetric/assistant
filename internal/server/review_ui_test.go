package server

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The PR review page and the task's review buttons, under node with a fake DOM: files in the order the
// API gives, ranked ones open; a chip per extension with its count hides its files; fold per file and
// for all visible ones; the ? at a line or file header opens a box whose question goes to the review's
// session, with the short answer and, a click away, the full one; the buttons only with a PR field.
func TestReviewPageUI(t *testing.T) {
	node, src := reviewJS(t)
	script := reviewDOM + `
const location = { hash: '' };
let confirmed = true;
const confirms = [];
const confirm = m => { confirms.push(m); return confirmed; };
const posts = [];
let nextQ = 100;
let taskReviews = [];
const api = async (method, path, body) => {
  posts.push([method, path, body]);
  if (method === 'GET' && path.endsWith('/reviews')) return taskReviews;
  if (path.endsWith('/submit') && body.body === 'gh fails') throw new Error('Can not approve your own pull request');
  if (path.endsWith('/questions')) return Object.assign({ id: nextQ++, status: 'pending', short_answer: '', full_answer: '', error: '' }, body);
  return { review_id: 42, session_id: 9 };
};
const file = (path, group, rank, hunks, annotations) => ({ path, old_path: path, status: 'modified', binary: false, adds: 1, dels: 1, group, rank, why: group + ' why',
  hunks: hunks || [], annotations: annotations || [] });
const payHunks = [{ header: '@@ -1,2 +1,2 @@', lines: [{ t: ' ', old: 1, new: 1, text: 'class Pay' }, { t: '-', old: 2, new: 0, text: 'def a' }, { t: '+', old: 0, new: 2, text: 'def b' }] }];
const data = {
  review: { id: 7, task_id: 5, status: 'ready', title: 'o/r#1: Pay', pr_url: 'https://github.com/o/r/pull/1', head_sha: 'abcdef1234567', created_at: 1 },
  summary: 'Sažetak', session: { id: 9 },
  files: [file('app/pay.rb', 'ranked', 1, payHunks, [{ line: 2, side: 'new', kind: 'method', text: 'Računa.' }]), file('lib/b.rb', 'ranked', 2),
    file('spec/pay_spec.rb', 'unimportant'), file('config/x.yml', 'hidden'), file('Makefile', 'other'), file('.env', 'other')],
  questions: [{ id: 1, path: 'app/pay.rb', side: 'new', line: 2, question: 'Zašto b?', status: 'answered', short_answer: 'Jer je kraće.', full_answer: 'Dugo **objašnjenje**.' }],
};
eval(require('fs').readFileSync(process.argv.at(-1), 'utf8'));
(async () => {
  if (fileExt('a/user_spec.rb') !== 'rb' || fileExt('Makefile') !== '' || fileExt('.env') !== 'env' || fileExt('X.YML') !== 'yml') fail('fileExt');

  const page = reviewPage(data, [{ id: 7, created_at: 1, status: 'ready' }, { id: 3, created_at: 0, status: 'failed' }]);
  const fileEls = page.files.map(x => x.el);
  const paths = page.files.map(x => x.f.path).join(' ');
  if (paths !== 'app/pay.rb lib/b.rb spec/pay_spec.rb config/x.yml Makefile .env') fail('order ' + paths);
  if (!find(page.el, e => e.tag === 'a' && e.attrs.href === '#/review/3')) fail('no link to the older review');
  if (!find(page.el, e => e.tag === 'a' && e.attrs.href === '#/task/5/s/9')) fail('no session link');
  const opened = () => page.files.map(x => x.isOpen() ? 1 : 0).join('');
  if (opened() !== '110000') fail('default fold ' + opened());
  if (!find(fileEls[0], e => e.classes.has('why') && e.text === 'ranked why')) fail('no why');
  // A note is hidden at first; its line's note button (the kind's letter) shows and hides it.
  const ann = find(fileEls[0], e => e.classes.has('ann') && e.text.includes('Računa.'));
  if (!ann || !hidden(ann)) fail('annotation not hidden at first');
  const notedLine = find(fileEls[0], e => e.classes.has('dl') && e.classes.has('noted'));
  const noteBtn = find(notedLine, e => e.classes.has('note-btn'));
  if (!noteBtn || noteBtn.text !== 'M') fail('note button ' + (noteBtn && noteBtn.text));
  if (findAll(fileEls[0], e => e.classes.has('note-btn')).length !== 1) fail('note button on a line without notes');
  noteBtn.fire('click');
  if (hidden(ann) || !noteBtn.classes.has('on')) fail('note not shown');
  noteBtn.fire('click');
  if (!hidden(ann)) fail('note not hidden again');
  noteBtn.fire('click');
  if (find(page.el, e => e.classes.has('notes-btn'))) fail('a show-all notes button');
  // Notes without a diff line are behind their own button.
  const orphanPage = reviewPage(Object.assign({}, data, { files: [file('x.rb', 'ranked', 1, payHunks, [{ line: 99, side: 'new', text: 'Daleko.' }])] }), []);
  const orphans = find(orphanPage.el, e => e.classes.has('review-orphans'));
  const orphanBtn = find(orphanPage.el, e => e.classes.has('note-btn'));
  if (!orphans || !hidden(orphans) || orphanBtn.text !== 'Bilješke bez linije u diffu (1)') fail('orphans not hidden');
  orphanBtn.fire('click');
  if (hidden(orphans)) fail('orphans not shown');

  // Chips: the extensions with their counts, all on; one off hides its files, on shows them again.
  const chips = page.chips.map(c => c.text).join(' ');
  if (chips !== 'rb (3) env (1) yml (1) bez ekstenzije (1)') fail('chips ' + chips);
  const rb = page.chips[0];
  rb.fire('click');
  if (!hidden(fileEls[0]) || !hidden(fileEls[1]) || !hidden(fileEls[2]) || hidden(fileEls[3]) || rb.classes.has('on')) fail('rb off');
  // Otvori sve / Zatvori sve act on visible files only.
  const btn = label => find(page.el, e => e.tag === 'button' && e.text === label);
  btn('Otvori sve').fire('click');
  if (opened() !== '110111') fail('open all ' + opened());
  btn('Zatvori sve').fire('click');
  if (opened() !== '110000') fail('close all ' + opened());
  rb.fire('click');
  if (hidden(fileEls[0]) || !rb.classes.has('on')) fail('rb back on');
  // One file folds by its header.
  const head = find(fileEls[0], e => e.classes.has('review-file-head'));
  head.fire('click');
  if (page.files[0].isOpen()) fail('file did not fold');
  head.fire('click');
  if (!page.files[0].isOpen()) fail('file did not unfold');

  // The line with a question is marked; its ? opens the box with the short answer, the full one a click away.
  const lineAsk = find(fileEls[0], e => e.classes.has('dl') && e.text.includes('+def b'));
  const ask = find(lineAsk, e => e.classes.has('ask-btn'));
  if (!ask.classes.has('has-q') || !ask.text.includes('1')) fail('line not marked');
  if (find(fileEls[0], e => e.classes.has('q-box'))) fail('box open before click');
  ask.fire('click');
  let box = find(fileEls[0], e => e.classes.has('q-box'));
  if (!box || !find(box, e => e.classes.has('q-short') && e.text === 'Jer je kraće.')) fail('no short answer');
  if (find(box, e => e.classes.has('q-full'))) fail('full answer shown at once');
  find(box, e => e.classes.has('q-full-btn')).fire('click');
  const full = find(box, e => e.classes.has('q-full'));
  if (!full || !full.text.includes('Dugo **objašnjenje**.') || !find(full, e => e.tag === 'a' && e.attrs.href === '#/task/5/s/9')) fail('full answer');

  // Esc in the input closes the box; the ? opens it again.
  const escInput = find(box, e => e.classes.has('q-input'));
  if (!escInput.focused) fail('input not focused on open');
  escInput.fire('keydown', { key: 'Escape' });
  if (find(fileEls[0], e => e.classes.has('q-box'))) fail('Escape did not close the box');
  ask.fire('click');
  box = find(fileEls[0], e => e.classes.has('q-box'));
  if (!box) fail('box did not reopen after Escape');

  // Asking: the question goes to the review with its place, shows as pending, and its answer arrives.
  const input = find(box, e => e.classes.has('q-input'));
  input.value = 'A testovi?';
  await input.fire('keydown', { key: 'Enter' });
  const p = posts.at(-1);
  if (p[1] !== '/api/reviews/7/questions' || p[2].path !== 'app/pay.rb' || p[2].side !== 'new' || p[2].line !== 2 || p[2].question !== 'A testovi?')
    fail('posted ' + JSON.stringify(p));
  if (!find(box, e => e.classes.has('q-pending')) || input.value !== '' || !page.polls()) fail('not pending');
  page.update({ questions: [data.questions[0], { id: 100, path: 'app/pay.rb', side: 'new', line: 2, question: 'A testovi?', status: 'answered', short_answer: 'Ima ih.', full_answer: 'x' }] });
  if (find(box, e => e.classes.has('q-pending')) || !find(box, e => e.classes.has('q-short') && e.text === 'Ima ih.') || page.polls()) fail('answer did not arrive');
  if (!ask.text.includes('2')) fail('marker count');
  // A removed line asks about the old side; the header about the whole file; a failed one can be sent again.
  const oldLine = find(fileEls[0], e => e.classes.has('dl-wrap') && e.text.includes('-def a'));
  find(oldLine, e => e.classes.has('ask-btn')).fire('click');
  const oldInput = find(oldLine, e => e.classes.has('q-input'));
  oldInput.value = 'Zašto maknuto?';
  await oldInput.fire('keydown', { key: 'Enter' });
  if (posts.at(-1)[2].side !== 'old' || posts.at(-1)[2].line !== 2) fail('old side ' + JSON.stringify(posts.at(-1)));
  find(head, e => e.classes.has('ask-btn')).fire('click');
  const fileBox = find(fileEls[0], e => e.classes.has('q-slot') && e.children.length);
  const fileInput = find(fileBox, e => e.classes.has('q-input'));
  fileInput.value = 'Cijeli?';
  await find(fileBox, e => e.tag === 'button' && e.text === 'Pitaj').fire('click');
  if (posts.at(-1)[2].side !== '' || posts.at(-1)[2].line !== 0 || posts.at(-1)[2].path !== 'app/pay.rb') fail('file question ' + JSON.stringify(posts.at(-1)));
  page.update({ questions: [{ id: 101, path: 'app/pay.rb', side: '', line: 0, question: 'Cijeli?', status: 'failed', error: 'boom' }] });
  if (!find(fileBox, e => e.classes.has('q-error') && e.text === 'boom')) fail('no error');
  await find(fileBox, e => e.tag === 'button' && e.text === 'Pošalji ponovo').fire('click');
  if (posts.at(-1)[2].question !== 'Cijeli?') fail('resend');

  btn('Otvori sve').fire('click');
  if (find(page.el, e => e.children.some(c => c === 'null' || c === 'undefined' || c === 'false'))) fail('null rendered as text');

  // Posting the review to GitHub: Comment and Request changes need a message; each asks first; what was
  // posted shows under the box; gh's error shows there too.
  const sub = find(page.el, e => e.classes.has('review-submit'));
  const subBody = find(sub, e => e.classes.has('review-submit-body'));
  const verdict = label => find(sub, e => e.tag === 'button' && e.text === label);
  const nPosts = posts.length;
  await verdict('Request changes').fire('click');
  if (posts.length !== nPosts || hidden(find(sub, e => e.classes.has('q-error')))) fail('request changes without a message');
  subBody.value = 'Fali test.';
  confirmed = false;
  await verdict('Request changes').fire('click');
  if (posts.length !== nPosts || !confirms.at(-1).includes('https://github.com/o/r/pull/1')) fail('posted without confirm');
  confirmed = true;
  await verdict('Request changes').fire('click');
  if (JSON.stringify(posts.at(-1)) !== JSON.stringify(['POST', '/api/reviews/7/submit', { event: 'request_changes', body: 'Fali test.' }])) fail('submit ' + JSON.stringify(posts.at(-1)));
  if (subBody.value !== '' || !find(sub, e => e.classes.has('review-submitted') && e.text.includes('Zatražene izmjene: Fali test.')) || toasts.at(-1) !== 'Zatražene izmjene na GitHubu') fail('after submit');
  if (!hidden(find(sub, e => e.classes.has('q-error')))) fail('error left after submit');
  await verdict('Approve').fire('click');
  if (JSON.stringify(posts.at(-1)[2]) !== JSON.stringify({ event: 'approve', body: '' })) fail('approve ' + JSON.stringify(posts.at(-1)));
  subBody.value = 'gh fails';
  await verdict('Comment').fire('click');
  if (!find(sub, e => e.classes.has('q-error') && !hidden(e) && e.text.includes('own pull request')) || subBody.value !== 'gh fails' || verdict('Comment').disabled) fail('gh error');

  // Not ready: the state with the session, no files.
  const gen = reviewPage(Object.assign({}, data, { review: Object.assign({}, data.review, { status: 'generating' }) }), []);
  if (!find(gen.el, e => e.classes.has('review-state') && e.text.includes('Generiram…')) || find(gen.el, e => e.classes.has('ask-btn')) || !gen.polls()) fail('generating');
  const failed = reviewPage(Object.assign({}, data, { review: Object.assign({}, data.review, { status: 'failed' }) }), []);
  if (!find(failed.el, e => e.classes.has('review-state') && e.text.includes('Generiranje nije uspjelo')) || failed.polls()) fail('failed');

  // The task's buttons: none without a PR field; Novi review alone before the first one.
  if (reviewButtons({ id: 5, fields: {} }, [])) fail('buttons without PR');
  const ticks = [];
  const every = (ms, fn) => ticks.push(fn);
  const first = reviewButtons({ id: 5, fields: { pull_request: 'https://github.com/o/r/pull/1' } }, [], every);
  if (find(first, e => e.text === 'Otvori review')) fail('Otvori review without a review');
  // Novi review stays in the task: Generiram… with the session, Novi review waits, no Otvori review yet.
  taskReviews = [{ id: 42, session_id: 9, status: 'generating' }];
  await find(first, e => e.tag === 'button' && e.text === 'Novi review').fire('click');
  if (!posts.some(p => p[0] === 'POST' && p[1] === '/api/tasks/5/reviews') || location.hash !== '') fail('Novi review ' + JSON.stringify(posts.at(-1)) + location.hash);
  const genState = find(first, e => e.classes.has('review-gen-state'));
  if (!genState || !genState.text.includes('Generiram…') || !find(genState, e => e.tag === 'a' && e.attrs.href === '#/task/5/s/9')) fail('generating state');
  if (!find(first, e => e.tag === 'button' && e.text === 'Novi review').attrs.disabled) fail('Novi review not disabled while generating');
  if (find(first, e => e.text === 'Otvori review')) fail('Otvori review while generating');
  // Once it is ready, a poll brings Otvori review to it.
  taskReviews = [{ id: 42, session_id: 9, status: 'ready' }];
  for (const f of ticks) await f();
  if (!find(first, e => e.tag === 'a' && e.attrs.href === '#/review/42' && e.text === 'Otvori review') || find(first, e => e.classes.has('review-gen-state'))) fail('ready after poll');
  if (find(first, e => e.tag === 'button' && e.text === 'Novi review').attrs.disabled || toasts.at(-1) !== 'Review je gotov') fail('ready state');
  // A new one generating next to an older ready one: Otvori review opens the ready one.
  const more = reviewButtons({ id: 5, fields: { pull_request: 'x' } }, [{ id: 8, session_id: 11, status: 'generating' }, { id: 3, status: 'ready' }]);
  if (!find(more, e => e.tag === 'a' && e.attrs.href === '#/review/3' && e.text === 'Otvori review') || !find(more, e => e.text.includes('Generiram…'))) fail('Otvori review');
  const failedBtns = reviewButtons({ id: 5, fields: { pull_request: 'x' } }, [{ id: 8, session_id: 11, status: 'failed' }]);
  if (find(failedBtns, e => e.text === 'Otvori review') || !find(failedBtns, e => e.text.includes('Generiranje nije uspjelo')) || find(failedBtns, e => e.text === 'Novi review').attrs.disabled) fail('failed buttons');
})();
`
	out, err := exec.Command(node, "-e", script, "--", src).CombinedOutput()
	if err != nil || len(out) > 0 {
		t.Fatalf("review page: %v\n%s", err, strings.TrimSpace(string(out)))
	}
}

// reviewJS is node and a file with app.js's PR review part, for a test to eval; it skips the test
// without node.
func reviewJS(t *testing.T) (node, src string) {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	js, err := webFS.ReadFile("web/app.js")
	if err != nil {
		t.Fatal(err)
	}
	start := bytes.Index(js, []byte("// ---------- PR review ----------"))
	end := bytes.Index(js, []byte("function artifactsPanel("))
	if start < 0 || end < start {
		t.Fatal("app.js lacks the review page")
	}
	src = filepath.Join(t.TempDir(), "review.js")
	if err := os.WriteFile(src, js[start:end], 0o600); err != nil {
		t.Fatal(err)
	}
	return node, src
}

// reviewDOM is the fake DOM and helpers the review UI tests run on: h, find, findAll, hidden, fail.
const reviewDOM = `
const fail = msg => { console.log(msg); process.exitCode = 1; };
function mk(tag, attrs, children) {
  const on = {};
  const el = {
    tag, attrs, value: attrs.value || '', children: [],
    classes: new Set((attrs.class || '').split(' ').filter(Boolean)),
    addEventListener(ev, fn) { on[ev] = fn; },
    fire(ev, e) { return on[ev](Object.assign({ target: el, preventDefault() {}, stopPropagation() {} }, e)); },
    // As the DOM's: null, undefined and false become text.
    append(...cs) { el.children.push(...cs.map(c => typeof c === 'object' && c !== null ? c : String(c))); },
    replaceChildren(...cs) { el.children = []; el.append(...cs); },
    setAttribute(k, v) { el.attrs[k] = v; },
    focus() { el.focused = true; },
    get text() { return el.children.map(c => typeof c === 'object' ? c.text : String(c)).join(''); },
  };
  el.classList = {
    toggle(c, on) { if (on === undefined) on = !el.classes.has(c); if (on) el.classes.add(c); else el.classes.delete(c); },
    add(c) { el.classes.add(c); }, remove(c) { el.classes.delete(c); }, contains: c => el.classes.has(c),
  };
  for (const [k, v] of Object.entries(attrs)) if (k.startsWith('on') && typeof v === 'function') on[k.slice(2)] = v;
  el.append(...children.flat(Infinity).filter(c => c != null && c !== false));
  return el;
}
const h = (tag, attrs, ...children) => mk(tag, attrs || {}, children);
const markdown = text => h('div', { class: 'markdown' }, text);
const fmtTime = ts => 'T' + ts;
const toasts = [];
const toast = m => toasts.push(m);
const sessionHref = s => '#/task/' + s.task_id + '/s/' + s.id;
const find = (el, pred) => {
  if (!el || typeof el !== 'object') return null;
  if (pred(el)) return el;
  for (const c of el.children) { const f = find(c, pred); if (f) return f; }
  return null;
};
const findAll = (el, pred, out = []) => {
  if (!el || typeof el !== 'object') return out;
  if (pred(el)) out.push(el);
  el.children.forEach(c => findAll(c, pred, out));
  return out;
};
const hidden = el => el.classes.has('hidden');
`
