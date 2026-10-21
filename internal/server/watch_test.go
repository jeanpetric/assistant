package server

import (
	"bytes"
	"testing"
)

// The title heads the task's top bar on the left; the buttons follow it on the right.
// The watch toggle is an eye in the task's top bar, beside Full page in the dialog and alone on the full
// page, not a button among the side panel's actions.
func TestWatchToggleInTopBar(t *testing.T) {
	js, err := webFS.ReadFile("web/app.js")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"h('div', { class: 'task-dialog-bar' },\n      page.title,\n      page.watch,\n      h('a', { class: 'btn ghost small', href: `#/task/${id}`",
		"view.replaceChildren(h('div', { class: 'task-page-bar' }, page.title, page.watch), page.el);",
		"class: 'ghost small watch-toggle'",
	} {
		if !bytes.Contains(js, []byte(want)) {
			t.Fatalf("app.js lacks %q", want)
		}
	}
	for _, gone := range []string{"archived,\n    h('input', { class: 'title-input'", "'Unwatch' : 'Watch'", "fmtTime(t.created_at)),\n      watch,"} {
		if bytes.Contains(js, []byte(gone)) {
			t.Fatalf("app.js still has %q", gone)
		}
	}
}

// Opening a task from another project (a notification, ⌥J) switches to the task's project.
func TestTaskViewSwitchesProject(t *testing.T) {
	js, err := webFS.ReadFile("web/app.js")
	if err != nil {
		t.Fatal(err)
	}
	want := "const t = data.task;\n  // A task opened from another project (a notification, ⌥J) brings its project into focus.\n  if (t.project_id !== state.projectId) {\n    state.projectId = t.project_id;\n    remember('project', t.project_id);\n    await loadBoard();\n  }"
	if !bytes.Contains(js, []byte(want)) {
		t.Fatalf("app.js lacks %q", want)
	}
}
