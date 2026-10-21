package server

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// A session started from the ⌥P dialog runs in the background: only the
// Sessions pages move to it, every other page stays put.
func TestSessionDialogStaysInBackground(t *testing.T) {
	js, err := webFS.ReadFile("web/app.js")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(js, []byte("const next = afterSessionStart(location.hash, s.id);")) {
		t.Fatal("sessionDialog does not use afterSessionStart")
	}
	if bytes.Contains(js, []byte("dlg.close();\n    location.hash = `#/session/${s.id}`;")) {
		t.Fatal("sessionDialog still jumps to the new session")
	}

	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	start := bytes.Index(js, []byte("function afterSessionStart("))
	end := bytes.Index(js, []byte("// ---------- memory ----------"))
	if start < 0 || end < start {
		t.Fatal("app.js lacks afterSessionStart")
	}
	src := filepath.Join(t.TempDir(), "after.js")
	if err := os.WriteFile(src, js[start:end], 0o600); err != nil {
		t.Fatal(err)
	}
	script := `
const f = eval(require('fs').readFileSync(process.argv.at(-1), 'utf8') + '; afterSessionStart');
for (const [hash, want] of [['', null], ['#/board', null], ['#/list', null], ['#/task/3', null], ['#/task/3/s/4', null],
  ['#/memory', null], ['#/sessions', '#/sessions'], ['#/session/5', '#/session/9']]) {
  if (f(hash, 9) !== want) { console.log(hash, f(hash, 9), 'want', want); process.exitCode = 1; }
}
`
	out, err := exec.Command(node, "-e", script, "--", src).CombinedOutput()
	if err != nil {
		t.Fatalf("afterSessionStart: %v\n%s", err, strings.TrimSpace(string(out)))
	}
}

// The Sessions page shows which project each session comes from.
func TestSessionsListShowsProject(t *testing.T) {
	js, err := webFS.ReadFile("web/app.js")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"['Status', 'Project', 'Task / session',", "s.project_id ? projectName(s.project_id)"} {
		if !bytes.Contains(js, []byte(want)) {
			t.Fatalf("app.js lacks %q", want)
		}
	}
}

// ⌥T and ⌥P on an open task make the new task build on it: both dialogs start
// their builds-on picker from it and send what is left of it as context_from.
func TestNewTaskDialogsBuildOnOpenTask(t *testing.T) {
	js, err := webFS.ReadFile("web/app.js")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"run: async () => newTaskDialog({ buildsOn: await shownTask() })",
		"run: async () => sessionDialog({ buildsOn: await shownTask() })",
		"const id = shownTaskId(location.hash, taskDialog.id);",
		"const buildsOn = buildsOnPicker(from ? [from] : [], projectId);",
		"mode: mode.value, context_from: buildsOn.ids() }",
		"context_from: buildsOn.ids(), memories: memories.ids(),",
	} {
		if !bytes.Contains(js, []byte(want)) {
			t.Fatalf("app.js lacks %q", want)
		}
	}
}
