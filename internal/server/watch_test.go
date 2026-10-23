package server

import (
	"bytes"
	"testing"
)

// The title heads the task's top bar on the left; the buttons follow it on the right.
// The watch toggle is an eye in the task dialog's top bar, beside close, not a button among the side
// panel's actions. There is no full page any more: no Full page button, no task page bar.
func TestWatchToggleInTopBar(t *testing.T) {
	js, err := webFS.ReadFile("web/app.js")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"h('div', { class: 'task-dialog-bar' },\n      page.title,\n      page.done,\n      page.watch,\n      h('button', { class: 'ghost small', title: 'Close (Esc)'",
		"class: 'ghost small watch-toggle'",
	} {
		if !bytes.Contains(js, []byte(want)) {
			t.Fatalf("app.js lacks %q", want)
		}
	}
	for _, gone := range []string{"Full page", "task-page-bar", "async function renderTask(", "archived,\n    h('input', { class: 'title-input'", "'Unwatch' : 'Watch'", "fmtTime(t.created_at)),\n      watch,"} {
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

// Every way to a task opens its dialog over the page on screen: cards, list and archive rows, sessions,
// notifications, ⌥J and task links; a task's address (#/task/7/s/3) opens the board with it in the dialog.
func TestTasksOpenInDialog(t *testing.T) {
	js, err := webFS.ReadFile("web/app.js")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"task: () => renderTaskLink(Number(parts[1]), Number(parts[3]) || 0),",
		"history.replaceState(null, '', '#/board');",
		"await openTaskDialog(id, focusSession);",
		"const el = h('article', { class: 'card', draggable: 'true', onclick: () => openTaskDialog(t.id) },",
		"if (s.task_id) openTaskDialog(s.task_id, s.id);",
		"if (n.task_id) openTaskDialog(n.task_id, n.session_id || 0);",
		"onPick: t => openTaskDialog(t.id),",
		"openTaskDialog(taskId, st.session_id);",
		"document.addEventListener('click', openTaskLink);",
		"page.focus(focusSession);",
	} {
		if !bytes.Contains(js, []byte(want)) {
			t.Fatalf("app.js lacks %q", want)
		}
	}
	if n := bytes.Count(js, []byte("h('tr', { class: 'clickable', onclick: () => openTaskDialog(t.id) },")); n != 2 {
		t.Fatalf("list and archive rows opening the dialog: %d, want 2", n)
	}
	if bytes.Contains(js, []byte("location.hash = `#/task/")) {
		t.Fatal("app.js still navigates to a task's page")
	}
}

// Done in the task dialog's top bar (⌥D) moves the task to the lane named Done; it hides once the task
// is there or archived. A move, by hand or by an agent turn, redraws the lane select and the button.
func TestDoneButtonInTopBar(t *testing.T) {
	js, err := webFS.ReadFile("web/app.js")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"const doneLane = state.board.lanes.find(l => l.name.toLowerCase() === 'done');",
		"onclick: async () => { await moveTo(doneLane.id); toast('Moved to ' + doneLane.name); },\n  }, '✓ Done', hint('⌥D'));",
		"done.classList.toggle('hidden', !doneLane || t.lane_id === doneLane.id || !!t.archived_at);",
		"h('label', {}, 'Lane'),\n    lane,",
		"if (document.activeElement !== lane) lane.value = t.lane_id;",
		"{ keys: '⌥D', code: 'KeyD', label: 'Move the open task to Done', run: moveShownToDone },",
		"const btn = document.querySelector('.done-toggle');",
	} {
		if !bytes.Contains(js, []byte(want)) {
			t.Fatalf("app.js lacks %q", want)
		}
	}
}
