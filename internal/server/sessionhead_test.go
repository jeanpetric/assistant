package server

import (
	"bytes"
	"testing"
)

// The session head shows the title, the session's facts (status, cost, date and
// badges that differ from the usual) and a fixed group of buttons on the right of
// its first line, the collapse toggle last, so a longer status never pushes it
// to another line. Dismiss, Copy resume cmd, Hand off and the bypassPermissions
// badge are gone.
func TestSessionHead(t *testing.T) {
	js, err := webFS.ReadFile("web/app.js")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"const info = h('div', { class: 'session-info' }",
		"const actions = h('div', { class: 'session-actions' }",
		"s.mode && s.mode !== 'bypassPermissions' ? h('span', { class: 'badge' }, s.mode) : null,",
		"}, s.title || `Session ${s.id}`),\n      info, actions);",
	} {
		if !bytes.Contains(js, []byte(want)) {
			t.Fatalf("app.js lacks %q", want)
		}
	}
	// The collapse toggle is the last button of the actions group.
	a := bytes.Index(js, []byte("const actions = h('div', { class: 'session-actions' }"))
	end := bytes.Index(js[a:], []byte("].filter(Boolean));"))
	group := js[a : a+end]
	if !bytes.HasSuffix(bytes.TrimSpace(group), []byte("'▾'),")) || !bytes.Contains(group, []byte("'Fold turns'")) {
		t.Fatalf("actions group does not end with the collapse toggle:\n%s", group)
	}
	for _, gone := range []string{"'Dismiss'", "Copy resume cmd", "handoffDialog", "/needs-input"} {
		if bytes.Contains(js, []byte(gone)) {
			t.Fatalf("app.js still has %q", gone)
		}
	}
	css, err := webFS.ReadFile("web/style.css")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		".session-info { display: flex; flex: 0 1 auto; min-width: 0; flex-wrap: wrap;",
		".session-actions { display: flex; flex: none;",
	} {
		if !bytes.Contains(css, []byte(want)) {
			t.Fatalf("style.css lacks %q", want)
		}
	}
	if bytes.Contains(css, []byte(".session-head { display: flex; gap: 10px; align-items: flex-start; padding: 10px 14px; border-bottom: 1px solid var(--border); flex-wrap: wrap;")) {
		t.Fatal("the session head wraps its buttons to another line")
	}
}
