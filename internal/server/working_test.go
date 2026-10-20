package server

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// A running turn shows "Working…" with its elapsed time from the moment the
// prompt is sent, and sending fetches the new turn at once instead of on the next poll.
func TestWorkingIndicator(t *testing.T) {
	js, err := webFS.ReadFile("web/app.js")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"const el = h('div', { class: 'session' }, head, summary, body, working, limits, foot);",
		"if (c.session.status !== 'running') { working.replaceChildren(); return; }",
		"if (m.role === 'user') turnStart = Date.parse(m.created_at);",
		"if (m.id <= last) continue;",
		"input.value = '';\n    await refresh();",
		"await api('POST', `/api/sessions/${c.session.id}/resume`);\n    await refresh();",
		"handoffDialog(s, refresh)",
	} {
		if !bytes.Contains(js, []byte(want)) {
			t.Fatalf("app.js lacks %q", want)
		}
	}
	if bytes.Contains(js, []byte("c.session.status = 'running'")) {
		t.Fatal("app.js guesses the running status instead of fetching the started turn")
	}

	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	start := bytes.Index(js, []byte("function fmtElapsed("))
	end := bytes.Index(js, []byte("function messageEl("))
	if start < 0 || end < start {
		t.Fatal("app.js lacks fmtElapsed")
	}
	src := filepath.Join(t.TempDir(), "elapsed.js")
	if err := os.WriteFile(src, js[start:end], 0o600); err != nil {
		t.Fatal(err)
	}
	script := `
const fmt = eval(require('fs').readFileSync(process.argv.at(-1), 'utf8') + '; fmtElapsed');
for (const [secs, want] of [[0, '0s'], [59, '59s'], [60, '1m 00s'], [125, '2m 05s'], [3599, '59m 59s'], [3720, '1h 02m']]) {
  if (fmt(secs) !== want) { console.log(secs, fmt(secs), 'want', want); process.exitCode = 1; }
}
`
	out, err := exec.Command(node, "-e", script, "--", src).CombinedOutput()
	if err != nil {
		t.Fatalf("fmtElapsed: %v\n%s", err, strings.TrimSpace(string(out)))
	}
}
