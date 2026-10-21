package server

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The ⌥K project picker shows each project's watched tasks in brackets, counted afresh.
func TestProjectPickerShowsWatched(t *testing.T) {
	js, err := webFS.ReadFile("web/app.js")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"await loadBoard(); // fresh watched counts", "pickerLabel(p)"} {
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
for (const [p, want] of [[{ id: 1, name: 'Work', watched: 2 }, 'Work (2)'], [{ id: 3, name: 'Idle', watched: 0 }, 'Idle (0)']]) {
  if (f(p) !== want) { console.log(f(p), 'want', want); process.exitCode = 1; }
}
`
	out, err := exec.Command(node, "-e", script, "--", src).CombinedOutput()
	if err != nil || len(out) > 0 {
		t.Fatalf("pickerLabel: %v\n%s", err, strings.TrimSpace(string(out)))
	}
}

// The ⌥J picker names a watched task by id and title, then its project and lane.
func TestWatchedPickerLabel(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	js, err := webFS.ReadFile("web/app.js")
	if err != nil {
		t.Fatal(err)
	}
	start := bytes.Index(js, []byte("function watchedLabel("))
	end := bytes.Index(js, []byte("// ⌥J: the watched tasks"))
	if start < 0 || end < start {
		t.Fatal("app.js lacks watchedLabel")
	}
	src := filepath.Join(t.TempDir(), "watched.js")
	if err := os.WriteFile(src, js[start:end], 0o600); err != nil {
		t.Fatal(err)
	}
	script := `
const f = eval(require('fs').readFileSync(process.argv.at(-1), 'utf8') + '; watchedLabel');
const ps = [{ id: 1, name: 'Work' }], ls = [{ id: 3, name: 'In progress' }];
for (const [t, want] of [
  [{ id: 7, title: 'Fix it', project_id: 1, lane_id: 3 }, '#7 Fix it| · Work · In progress'],
  [{ id: 8, title: 'Gone', project_id: 9, lane_id: 3 }, '#8 Gone| · In progress'],
  [{ id: 9, title: 'Nowhere', project_id: 9, lane_id: 9 }, '#9 Nowhere|'],
]) {
  const got = f(t, ps, ls).join('|');
  if (got !== want) { console.log(got, 'want', want); process.exitCode = 1; }
}
`
	out, err := exec.Command(node, "-e", script, "--", src).CombinedOutput()
	if err != nil || len(out) > 0 {
		t.Fatalf("watchedLabel: %v\n%s", err, strings.TrimSpace(string(out)))
	}
}
