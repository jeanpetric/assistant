package server

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// Choosing Move hides the prompt, mode and directory; saving must not need
// them, and a Move to the rule's own lane is caught before the server sees it.
func TestRuleDialogBody(t *testing.T) {
	js, err := webFS.ReadFile("web/app.js")
	if err != nil {
		t.Fatal(err)
	}
	start := bytes.Index(js, []byte("function ruleBody("))
	end := bytes.Index(js, []byte("function ruleDialog("))
	if start < 0 || end < start {
		t.Fatal("app.js lacks ruleBody before ruleDialog")
	}
	if !bytes.Contains(js, []byte("if (err) return toast(err);")) {
		t.Fatal("ruleDialog does not stop on ruleBody's error")
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	src := filepath.Join(t.TempDir(), "rule.js")
	if err := os.WriteFile(src, js[start:end], 0o600); err != nil {
		t.Fatal(err)
	}
	script := `
const body = eval(require('fs').readFileSync(process.argv.at(-1), 'utf8') + '; ruleBody');
const fail = (...a) => { console.log(...a); process.exitCode = 1; };
const v = { name: 'Review it', project: '', from: '', to: '3', action: 'move',
  prompt: '{{.Context}}', mode: 'plan', workdir: '/w', moveTo: '4', doneLane: '' };
let [b, err] = body({ enabled: true }, v);
if (err || b.prompt !== '' || b.mode !== '' || b.workdir !== '' || b.skill_id !== null || b.done_lane_id !== 4 || b.to_lane_id !== 3 || !b.enabled)
  fail('move', err, JSON.stringify(b));
[b, err] = body({}, { ...v, moveTo: '3' });
if (b || !/other than To/.test(err)) fail('same lane', err);
[b, err] = body({}, { ...v, name: ' ' });
if (b || !err) fail('no name', err);
[b, err] = body({}, { ...v, action: '', doneLane: '' });
if (err || b.prompt !== '{{.Context}}' || b.mode !== 'plan' || b.done_lane_id !== null) fail('session', err, JSON.stringify(b));
[b, err] = body({}, { ...v, action: '7', doneLane: '5' });
if (err || b.skill_id !== 7 || b.prompt !== '' || b.done_lane_id !== 5) fail('skill', err, JSON.stringify(b));
`
	if out, err := exec.Command(node, "-e", script, "--", src).CombinedOutput(); err != nil || len(out) > 0 {
		t.Fatalf("node: %v\n%s", err, out)
	}
}
