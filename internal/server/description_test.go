package server

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The task description reads as rendered markdown, Edit swaps in the textarea and saves on change,
// Done renders the new text, and an empty description starts in the textarea. Runs under node
// with a fake DOM.
func TestDescriptionField(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	js, err := webFS.ReadFile("web/app.js")
	if err != nil {
		t.Fatal(err)
	}
	start := bytes.Index(js, []byte("function descriptionField("))
	end := bytes.Index(js, []byte("// ---------- task dialog: every page opens a task here, over itself ----------"))
	if start < 0 || end < start {
		t.Fatal("app.js lacks descriptionField")
	}
	if !bytes.Contains(js, []byte("descriptionField(t.description, text => save({ description: text }))")) {
		t.Fatal("the task dialog does not use descriptionField")
	}
	src := filepath.Join(t.TempDir(), "desc.js")
	if err := os.WriteFile(src, js[start:end], 0o600); err != nil {
		t.Fatal(err)
	}
	script := `
const fail = msg => { console.log(msg); process.exitCode = 1; };
function h(tag, attrs, ...children) {
  const cls = new Set(String((attrs || {}).class || '').split(' ').filter(Boolean));
  const on = {};
  const el = {
    tag, value: '', textContent: '', focused: false, children: [],
    classList: { contains: c => cls.has(c), toggle: (c, f) => f ? cls.add(c) : cls.delete(c) },
    replaceChildren(...k) { this.children = k; },
    focus() { this.focused = true; },
    fire(ev) { on[ev](); },
  };
  for (const [k, v] of Object.entries(attrs || {})) if (k.startsWith('on')) on[k.slice(2)] = v;
  const kids = children.flat(Infinity).filter(c => c != null);
  if (tag === 'textarea') el.value = kids.join(''); else el.children = kids;
  return el;
}
const hint = () => null;
const markdown = text => text ? ['md:' + text] : [];
const field = eval(require('fs').readFileSync(process.argv.at(-1), 'utf8') + '; descriptionField');

const saved = [];
let [bar, view, area] = field('# Title', t => saved.push(t));
const [label, toggle, expand] = bar.children;
if (!area.classList.contains('hidden') || view.classList.contains('hidden')) fail('a description should start rendered');
if (view.children.join() !== 'md:# Title') fail('view shows ' + view.children.join());
if (toggle.textContent !== 'Edit' || expand.classList.contains('hidden')) fail('want Edit and Expand');
toggle.fire('click');
if (area.classList.contains('hidden') || !view.classList.contains('hidden') || !area.focused) fail('Edit should show the focused textarea');
if (toggle.textContent !== 'Done') fail('want Done while editing');
area.value = '**new**';
area.fire('change');
if (saved.join() !== '**new**') fail('saved ' + saved.join());
toggle.fire('click');
if (view.children.join() !== 'md:**new**' || !area.classList.contains('hidden')) fail('Done should render the new text');
view.fire('dblclick');
if (area.classList.contains('hidden')) fail('a double click should edit');

[bar, view, area] = field('  ', () => {});
if (area.classList.contains('hidden') || area.focused) fail('an empty description should start in the unfocused textarea');
if (!bar.children[2].classList.contains('hidden')) fail('nothing to expand when empty');
`
	out, err := exec.Command(node, "-e", script, "--", src).CombinedOutput()
	if err != nil || len(out) > 0 {
		t.Fatalf("descriptionField: %v\n%s", err, strings.TrimSpace(string(out)))
	}
}
