package server

import (
	"bytes"
	"testing"
)

// The task dialog opens at the end of the linked session, or else of the newest
// open one, so a long transcript needs no scrolling down.
func TestTaskDialogOpensAtSessionEnd(t *testing.T) {
	js, err := webFS.ReadFile("web/app.js")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"const c = sessionId ? components.get(sessionId) : [...components.values()].reverse().find(c => !c.el.classList.contains('collapsed'));",
		"c.el.scrollIntoView({ block: 'end' });",
		"page.focus(focusSession);",
	} {
		if !bytes.Contains(js, []byte(want)) {
			t.Fatalf("app.js lacks %q", want)
		}
	}
}
