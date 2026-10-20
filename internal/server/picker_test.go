package server

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The ⌥K project picker shows each project's running sessions in brackets.
func TestProjectPickerShowsRunning(t *testing.T) {
	js, err := webFS.ReadFile("web/app.js")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"const running = await api('GET', '/api/sessions?status=running');", "pickerLabel(p, running)"} {
		if !bytes.Contains(js, []byte(want)) {
			t.Fatalf("projectPicker lacks %q", want)
		}
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	start := bytes.Index(js, []byte("function pickerLabel("))
	end := bytes.Index(js, []byte("// ⌥K: a list of the projects"))
	if start < 0 || end < start {
		t.Fatal("app.js lacks pickerLabel")
	}
	src := filepath.Join(t.TempDir(), "picker.js")
	if err := os.WriteFile(src, js[start:end], 0o600); err != nil {
		t.Fatal(err)
	}
	script := `
const f = eval(require('fs').readFileSync(process.argv.at(-1), 'utf8') + '; pickerLabel');
const running = [{ project_id: 1 }, { project_id: 1 }, { project_id: 2 }, {}];
for (const [p, want] of [[{ id: 1, name: 'Work' }, 'Work (2)'], [{ id: 2, name: 'Home' }, 'Home (1)'], [{ id: 3, name: 'Idle' }, 'Idle (0)']]) {
  if (f(p, running) !== want) { console.log(f(p, running), 'want', want); process.exitCode = 1; }
}
`
	out, err := exec.Command(node, "-e", script, "--", src).CombinedOutput()
	if err != nil || len(out) > 0 {
		t.Fatalf("pickerLabel: %v\n%s", err, strings.TrimSpace(string(out)))
	}
}
