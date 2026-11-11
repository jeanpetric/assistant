package server

import (
	"os/exec"
	"strings"
	"testing"
)

// A .plain file on the review page reads as a spec: the head content with the PR's change in place,
// the frontmatter, sections and items of PlainLang; what changed is open and the rest folded (an
// unchanged section to its header, a run of unchanged items to one row, an unchanged item to its first
// line); Samo promjene and Razmotaj sve; :Concepts: explain themselves on hover with their definition
// (from the PR or an imported module) and the review's Croatian note, and a click opens the definition;
// §refs open their spec; every line keeps its ?, also those outside the diff; Sirovi diff shows the diff.
func TestReviewSpecUI(t *testing.T) {
	node, src := reviewJS(t)
	script := reviewDOM + `
const location = { hash: '' };
const confirm = () => true;
const posts = [];
const api = async (method, path, body) => { posts.push([method, path, body]); return Object.assign({ id: 1, status: 'pending' }, body); };

const head = [
  '---',
  'description: Whole cents for :Period: amounts.',
  'import:',
  '  - quotes/quote',
  '---',
  '',
  '***definitions***',
  '',
  '- :Base: is the amount percentages apply to.',
  '  - On a :Quote: its total.',
  '> code: Forms::Base#base',
  '',
  '- :WholeCents: of an amount is it rounded half-up.',
  '',
  '***functional specs***',
  '',
  '> spec: §1.1',
  '- A :Period: stores :WholeCents: of :Base:.',
  '',
  '> spec: §1.2',
  '- The remainder goes to the last :Period:, see §1.1.',
  '  - A :Period: at 0 % never takes it.',
  '',
  '  ***acceptance tests***',
  '  - 100001 at 50 / 50: 50001 / 50000.',
  '',
  '> spec: §1.3',
  '- A locked :Period: is never written.',
  '',
].join('\n') + '\n';
// §1.2's sub-bullet is new, §1.1's line changed (old: "stores cents"); line 21 onwards shifts by one.
const specHunks = [{ header: '@@ -17,6 +17,7 @@', lines: [
  { t: ' ', old: 17, new: 17, text: '> spec: §1.1' },
  { t: '-', old: 18, new: 0, text: '- A :Period: stores cents.' },
  { t: '+', old: 0, new: 18, text: '- A :Period: stores :WholeCents: of :Base:.' },
  { t: ' ', old: 19, new: 19, text: '' },
  { t: ' ', old: 20, new: 20, text: '> spec: §1.2' },
  { t: ' ', old: 21, new: 21, text: '- The remainder goes to the last :Period:, see §1.1.' },
  { t: '+', old: 0, new: 22, text: '  - A :Period: at 0 % never takes it.' },
  { t: ' ', old: 22, new: 23, text: '' },
] }];
const specFile = { path: 'plain/internal/rd/amounts.plain', old_path: 'plain/internal/rd/amounts.plain', status: 'modified', binary: false, adds: 2, dels: 1,
  group: 'hidden', hidden_by: '*.plain', hunks: specHunks, annotations: [{ line: 18, side: 'new', kind: 'line', text: 'Zaokružuje.' }] };
const data = {
  review: { id: 7, task_id: 5, status: 'ready', title: 't', pr_url: '', created_at: 1 },
  summary: '', session: { id: 9 }, questions: [],
  files: [specFile],
  specs: {
    'plain/internal/rd/amounts.plain': head,
    'plain/internal/quotes/quote.plain': '---\ndescription: q\n---\n\n***definitions***\n\n- :Quote: is a deal.\n\n- :Period: is a slice of a :Quote:.\n  - Has start_on.\n> code: RevenueDistribution\n',
  },
  concepts: { Period: 'Razdoblje u kojem se priznaje prihod.' },
};
eval(require('fs').readFileSync(process.argv.at(-1), 'utf8'));
(async () => {
  // The merge: the head content with the hunk's lines in place, removed ones too.
  const lines = specLines(specFile, head);
  const txt = lines.map(l => l.t + (l.old || '') + '/' + (l.new || '')).join(' ');
  if (lines.length !== 30 || !txt.startsWith(' 1/1 ') || !txt.includes(' -18/ +/18 ') || !txt.includes(' +/22  22/23 ') || !txt.endsWith(' 28/29')) fail('specLines ' + txt);
  // Without the head content: the hunks with the gaps between them.
  const bare = specLines(specFile, undefined);
  if (bare[0].t !== 'gap' || bare[0].n !== 16 || bare.length !== 9) fail('bare ' + JSON.stringify(bare[0]) + ' ' + bare.length);

  const spec = parsePlain(lines);
  const names = spec.sections.map(s => s.name + ':' + s.items.length + (s.changed ? '*' : '')).join(' ');
  if (names !== 'definitions:2 functional specs:3*' || spec.front.length !== 5 || spec.frontChanged) fail('sections ' + names);
  const fs = spec.sections[1].items;
  if (fs.map(it => it.ids.join() + ':' + it.state).join(' ') !== '§1.1:changed §1.2:changed §1.3:') fail('items ' + fs.map(it => it.ids + it.state));
  if (fs[0].first.text !== '- A :Period: stores :WholeCents: of :Base:.' || spec.sections[0].items[0].defines !== 'Base') fail('first / defines');
  // Items without blank lines between them still split.
  const tight = parsePlain(['***definitions***', '- :A: is a.', '  - sub', '- :B: is b.', '> code: B', '> spec: §2', '- x'].map((text, i) => ({ t: ' ', old: i + 1, new: i + 1, text })));
  if (tight.sections[0].items.map(it => it.lines.length).join() !== '2,2,2' || tight.sections[0].items[2].ids[0] !== '§2') fail('tight ' + tight.sections[0].items.map(it => it.lines.length));

  // The index: local definitions, an imported module's, its sub-bullets and code.
  const index = conceptIndex(data.files, data.specs);
  const per = index.get('Period');
  if (!per || per.path !== 'plain/internal/quotes/quote.plain' || per.inPR || per.subs.join() !== 'Has start_on.' || per.code.join() !== 'RevenueDistribution') fail('Period ' + JSON.stringify(per && { ...per, item: 0 }));
  if (!index.get('Base').inPR || index.get('Base').code.join() !== 'Forms::Base#base') fail('Base');

  const page = reviewPage(data, []);
  const x = page.files[0];
  if (x.isOpen()) fail('a hidden spec open at first');
  x.setOpen(true);
  const view = find(x.el, e => e.classes.has('spec-view'));
  if (!view) fail('no spec view');
  const sec = name => findAll(view, e => e.classes.has('spec-sec')).find(s => find(s, e => e.classes.has('spec-sec-name') && e.text === name));
  const body = s => find(s, e => e.classes.has('spec-sec-body'));
  // Unchanged definitions folded, changed functional specs open; the unchanged §1.3 in a folded run.
  if (!hidden(body(sec('Definicije'))) || hidden(body(sec('Funkcionalne specifikacije')))) fail('section folds');
  if (!find(sec('Definicije'), e => e.text === ' · bez promjena') || !find(sec('Funkcionalne specifikacije'), e => e.classes.has('spec-changed') && e.text === '2 izmj.')) fail('section counts');
  const run = find(sec('Funkcionalne specifikacije'), e => e.classes.has('spec-run'));
  if (!run || run.text.trim() !== '▸ 1 nepromijenjena stavka') fail('run ' + (run && run.text));
  const items = findAll(sec('Funkcionalne specifikacije'), e => e.classes.has('spec-item'));
  const second = items[1];
  if (!find(second, e => e.classes.has('spec-id') && e.text === '§1.2') || !find(second, e => e.classes.has('spec-changed') && e.text === 'izmijenjeno')) fail('item badges');
  if (hidden(find(second, e => e.classes.has('spec-item-body')))) fail('changed item folded');
  // An unchanged spec-id line is not shown as a row (it is the badge); the removed line is, struck.
  if (find(view, e => e.classes.has('sl') && e.text.includes('spec: §1.2'))) fail('spec comment shown');
  const del = find(view, e => e.classes.has('sl') && e.classes.has('del'));
  if (!del || !del.text.includes('stores cents.')) fail('removed line');
  // The frontmatter folds to its description; the module's description has its concepts.
  const front = find(view, e => e.classes.has('spec-front'));
  if (!hidden(find(front, e => e.classes.has('spec-front-body'))) || !find(front, e => e.classes.has('concept') && e.text === 'Period')) fail('front');

  // Samo promjene / Razmotaj sve.
  const btn = (root, label) => find(root, e => e.tag === 'button' && e.text === label);
  btn(x.el, 'Razmotaj sve').fire('click');
  if (hidden(body(sec('Definicije'))) || run.text.trim() !== '▾ 1 nepromijenjena stavka') fail('open all ' + run.text);
  const baseItem = find(sec('Definicije'), e => e.classes.has('spec-item') && e.text.includes('is the amount'));
  if (hidden(find(baseItem, e => e.classes.has('spec-item-body')))) fail('unchanged item not opened by Razmotaj sve');
  btn(x.el, 'Samo promjene').fire('click');
  if (!hidden(body(sec('Definicije'))) || hidden(body(sec('Funkcionalne specifikacije'))) || !hidden(find(baseItem, e => e.classes.has('spec-item-body')))) fail('only changes');
  // An item folds and unfolds by its first line.
  const secondHead = find(second, e => e.classes.has('spec-head'));
  secondHead.fire('click');
  if (!hidden(find(second, e => e.classes.has('spec-item-body'))) || !second.classes.has('folded')) fail('item did not fold');
  secondHead.fire('click');

  // Hover: the card with the Croatian note, definition, sub-bullets, code and module.
  const conceptEl = find(find(view, e => e.classes.has('sl') && e.classes.has('add')), e => e.classes.has('concept') && e.text === 'Period');
  conceptEl.fire('mouseenter');
  const tipEl = find(page.el, e => e.classes.has('concept-tip'));
  if (hidden(tipEl) || !tipEl.text.includes('Razdoblje u kojem se priznaje prihod.') || !tipEl.text.includes('is a slice of a Quote.') ||
    !tipEl.text.includes('Has start_on.') || !tipEl.text.includes('kod: RevenueDistribution') || !tipEl.text.includes('iz importa: plain/internal/quotes/quote.plain')) fail('tip ' + tipEl.text);
  conceptEl.fire('mouseleave');
  if (!hidden(tipEl)) fail('tip stays');
  const tipFor = conceptTip(index, {});
  tipFor.show('Nope', h('span'));
  if (!tipFor.el.text.includes('Nije definiran')) fail('unknown tip');
  // A click opens the definition in this file: the Definitions section and its item.
  const baseEl = find(view, e => e.classes.has('concept') && e.text === 'Base' && !e.classes.has('unknown'));
  baseEl.fire('click');
  if (hidden(body(sec('Definicije'))) || hidden(find(baseItem, e => e.classes.has('spec-item-body'))) || !baseItem.classes.has('flash')) fail('reveal definition');
  // A §ref opens its spec.
  btn(x.el, 'Samo promjene').fire('click');
  const ref = find(view, e => e.classes.has('spec-ref') && e.text === '§1.1');
  ref.fire('click');
  if (!items[0].classes.has('flash')) fail('§ ref');

  // Every line keeps its ? and note: a changed line asks on its side, an unchanged one outside the diff too.
  const addLine = find(view, e => e.classes.has('sl') && e.classes.has('add') && e.text.includes('stores'));
  if (!addLine.classes.has('noted') || !find(addLine, e => e.classes.has('note-btn'))) fail('note on spec line');
  btn(x.el, 'Razmotaj sve').fire('click');
  const outside = find(view, e => e.classes.has('sl-wrap') && e.text.includes('is never written'));
  find(outside, e => e.classes.has('ask-btn')).fire('click');
  const input = find(outside, e => e.classes.has('q-input'));
  input.value = 'Što je locked?';
  await input.fire('keydown', { key: 'Enter' });
  const p = posts.at(-1);
  if (p[1] !== '/api/reviews/7/questions' || p[2].side !== 'new' || p[2].line !== 28 || p[2].path !== specFile.path) fail('question ' + JSON.stringify(p));
  const delWrap = find(view, e => e.classes.has('sl-wrap') && e.text.includes('stores cents.'));
  find(delWrap, e => e.classes.has('ask-btn')).fire('click');
  const delInput = find(delWrap, e => e.classes.has('q-input'));
  delInput.value = 'Zašto?';
  await delInput.fire('keydown', { key: 'Enter' });
  if (posts.at(-1)[2].side !== 'old' || posts.at(-1)[2].line !== 18) fail('old question ' + JSON.stringify(posts.at(-1)));

  // Sirovi diff and back.
  btn(x.el, 'Sirovi diff').fire('click');
  if (find(x.el, e => e.classes.has('spec-view')) || !find(x.el, e => e.classes.has('dl') && e.text.includes('+- A :Period: stores :WholeCents: of :Base:.'))) fail('raw diff');
  btn(x.el, 'Prikaži kao specifikaciju').fire('click');
  if (!find(x.el, e => e.classes.has('spec-view'))) fail('back to spec');

  // Without the head content (gh could not give it): the hunks with a gap row, still a spec.
  const bareData = Object.assign({}, data, { specs: {} });
  const barePage = reviewPage(bareData, []);
  barePage.files[0].setOpen(true);
  if (!find(barePage.el, e => e.classes.has('spec-gap') && e.text.includes('16 linija')) || !find(barePage.el, e => e.classes.has('concept') && e.text === 'Period' && e.classes.has('unknown'))) fail('bare spec view');
  // A new file: everything open, no "novo" badges, no spec-id rows, nothing rendered as null.
  const added = head.split('\n').slice(0, -1).map((text, i) => ({ t: '+', old: 0, new: i + 1, text }));
  const newPage = reviewPage(Object.assign({}, data, { specs: {}, files: [Object.assign({}, specFile, { status: 'added', hunks: [{ header: '@@ -0,0 +1,29 @@', lines: added }] })] }), []);
  newPage.files[0].setOpen(true);
  const newView = find(newPage.el, e => e.classes.has('spec-view'));
  if (!newView.classes.has('all-new') || find(newView, e => e.classes.has('spec-added')) || find(newView, e => e.classes.has('sl') && e.text.includes('spec: §')) ||
    findAll(newView, e => e.classes.has('spec-sec-body')).some(hidden) || find(newView, e => e.children.some(c => c === 'null'))) fail('new file');
  // A non-spec file keeps the diff.
  const rb = reviewPage(Object.assign({}, data, { files: [Object.assign({}, specFile, { path: 'app/x.rb', group: 'ranked', rank: 1 })] }), []);
  if (find(rb.el, e => e.classes.has('spec-view')) || !find(rb.el, e => e.classes.has('dl'))) fail('rb as spec');
  if (find(page.el, e => e.children.some(c => c === 'null' || c === 'undefined' || c === 'false'))) fail('null rendered as text');
})();
`
	out, err := exec.Command(node, "-e", script, "--", src).CombinedOutput()
	if err != nil || len(out) > 0 {
		t.Fatalf("spec view: %v\n%s", err, strings.TrimSpace(string(out)))
	}
}
