package server

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// markdown.js runs under node with an h() that builds a tiny fake DOM, serialised as HTML.
func TestMarkdown(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	cases := []struct{ in, want string }{
		{"", ""},
		{"plain text", "<p>plain text</p>"},
		{"line one\nline two\n\nnext para", "<p>line one\nline two</p><p>next para</p>"},
		{"# Title\n## Sub ##\n###### six\n#hashtag", "<h1>Title</h1><h2>Sub</h2><h6>six</h6><p>#hashtag</p>"},
		{"**bold** and *em* and _em_ and __strong__ and ~~gone~~", "<p><strong>bold</strong> and <em>em</em> and <em>em</em> and <strong>strong</strong> and <del>gone</del></p>"},
		{"***both*** *a **b** c*", "<p><strong><em>both</em></strong> <em>a <strong>b</strong> c</em></p>"},
		{"snake_case_name and 5 * 3 * 2 and a*b", "<p>snake_case_name and 5 * 3 * 2 and a*b</p>"},
		{"**unfinished", "<p>**unfinished</p>"},
		{"use `x *y* z` and ``a ` b``", "<p>use <code>x *y* z</code> and <code>a ` b</code></p>"},
		{`\*not em\* and \` + "`tick`", "<p>*not em* and `tick`</p>"},
		{"```go\nfunc main() {\n\n\t<b>\n}\n```\nafter", `<pre><code class="lang-go">func main() {` + "\n\n\t&lt;b&gt;\n}" + `</code></pre><p>after</p>`},
		{"```\nstreaming, not closed yet", "<pre><code>streaming, not closed yet</code></pre>"},
		{"- a\n- b\n  - nested\n- c", "<ul><li><p>a</p></li><li><p>b</p><ul><li><p>nested</p></li></ul></li><li><p>c</p></li></ul>"},
		{"1. one\n2. two\n   continued\n\n3. three", "<ol><li><p>one</p></li><li><p>two\ncontinued</p></li><li><p>three</p></li></ol>"},
		{"3) x\n4) y", `<ol start="3"><li><p>x</p></li><li><p>y</p></li></ol>`},
		{"1. step\n  - detail\n2. next", "<ol><li><p>step</p><ul><li><p>detail</p></li></ul></li><li><p>next</p></li></ol>"},
		{"Intro:\n- a\n- b\nlazy", "<p>Intro:</p><ul><li><p>a</p></li><li><p>b\nlazy</p></li></ul>"},
		{"- a\n\n  more of a\n- b", "<ul><li><p>a</p><p>more of a</p></li><li><p>b</p></li></ul>"},
		{"- a\n1. b", "<ul><li><p>a</p></li></ul><ol><li><p>b</p></li></ol>"},
		{"- [ ] todo\n- [x] done", `<ul><li class="task"><p><input checked="false" disabled="true" type="checkbox">todo</p></li><li class="task"><p><input checked="true" disabled="true" type="checkbox">done</p></li></ul>`},
		{"> quoted\n> **text**\nlazy\n\nout", "<blockquote><p>quoted\n<strong>text</strong>\nlazy</p></blockquote><p>out</p>"},
		{"a\n\n---\n***\n- - -", "<p>a</p><hr><hr><hr>"},
		{"| Name | Count |\n|:-----|------:|\n| `a\\|b` | **2** |\n| c |\n\nafter",
			`<div class="table-wrap"><table><thead><tr><th style="text-align:left">Name</th><th style="text-align:right">Count</th></tr></thead>` +
				`<tbody><tr><td style="text-align:left"><code>a|b</code></td><td style="text-align:right"><strong>2</strong></td></tr>` +
				`<tr><td style="text-align:left">c</td><td style="text-align:right"></td></tr></tbody></table></div><p>after</p>`},
		{"a | b\n--|--\n1 | 2", "<div class=\"table-wrap\"><table><thead><tr><th>a</th><th>b</th></tr></thead><tbody><tr><td>1</td><td>2</td></tr></tbody></table></div>"},
		{"[docs](https://x.io/a_(b)) and [**b**](http://y.io \"t\")",
			`<p><a href="https://x.io/a_(b)" rel="noopener noreferrer" target="_blank">docs</a> and <a href="http://y.io" rel="noopener noreferrer" target="_blank"><strong>b</strong></a></p>`},
		{"see https://x.io/p?q=1. or (https://y.io/z) <mailto:a@b.c>",
			`<p>see <a href="https://x.io/p?q=1" rel="noopener noreferrer" target="_blank">https://x.io/p?q=1</a>. or (<a href="https://y.io/z" rel="noopener noreferrer" target="_blank">https://y.io/z</a>) <a href="mailto:a@b.c" rel="noopener noreferrer" target="_blank">mailto:a@b.c</a></p>`},
		{"[x](javascript:alert(1)) [f](internal/app.go) [t](#/task/3) [no link]", `<p><span title="javascript:alert(1)">x</span> <span title="internal/app.go">f</span> <a href="#/task/3">t</a> [no link]</p>`},
		{"<script>alert(1)</script> & <b>", "<p>&lt;script&gt;alert(1)&lt;/script&gt; &amp; &lt;b&gt;</p>"},
		{"x\r\n\r\ny", "<p>x</p><p>y</p>"},
	}
	ins := make([]string, len(cases))
	for i, c := range cases {
		ins[i] = c.in
	}
	casesJSON, err := json.Marshal(ins)
	if err != nil {
		t.Fatal(err)
	}
	js, err := webFS.ReadFile("web/markdown.js")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	src := filepath.Join(dir, "markdown.js")
	in := filepath.Join(dir, "cases.json")
	if err := os.WriteFile(src, js, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(in, casesJSON, 0o600); err != nil {
		t.Fatal(err)
	}
	script := `
const fs = require('fs');
const [src, cases] = process.argv.slice(-2);
const esc = s => s.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;');
const html = n => typeof n === 'string' ? esc(n)
  : '<' + n.tag + Object.keys(n.attrs).sort().map(k => ' ' + k + '="' + n.attrs[k] + '"').join('') + '>' +
    (n.tag === 'hr' || n.tag === 'input' ? '' : n.children.map(html).join('') + '</' + n.tag + '>');
function h(tag, attrs, ...children) {
  const a = {};
  for (const [k, v] of Object.entries(attrs || {})) if (v != null) a[k] = String(v);
  const kids = children.flat(Infinity).filter(c => c != null && c !== false).map(c => typeof c === 'object' ? c : String(c));
  return { tag, tagName: tag.toUpperCase(), attrs: a, children: kids, prepend(c) { kids.unshift(c); } };
}
const markdown = eval(fs.readFileSync(src, 'utf8') + '; markdown');
console.log(JSON.stringify(JSON.parse(fs.readFileSync(cases, 'utf8')).map(c => markdown(c).map(html).join(''))));
`
	out, err := exec.Command(node, "-e", script, "--", src, in).CombinedOutput()
	if err != nil {
		t.Fatalf("node: %v\n%s", err, strings.TrimSpace(string(out)))
	}
	var got []string
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("node output: %v\n%s", err, out)
	}
	for i, c := range cases {
		if got[i] != c.want {
			t.Errorf("markdown(%q)\n got: %s\nwant: %s", c.in, got[i], c.want)
		}
	}
}
