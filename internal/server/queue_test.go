package server

import (
	"strings"
	"testing"

	"github.com/jeanpetric/assistant/internal/store"
)

// A message sent while the agent works is queued: the session lists it until
// it is sent or dropped, by the API or the MCP tool.
func TestQueuedMessagesAPI(t *testing.T) {
	c := newClient(t)
	var task store.Task
	c.do("POST", "/api/tasks", map[string]any{"title": "t"}, 201, &task)
	var sess store.Session
	c.do("POST", "/api/tasks/"+itoa(task.ID)+"/sessions", map[string]any{"prompt": "block"}, 201, &sess)
	sid := itoa(sess.ID)

	var res struct {
		Queued bool `json:"queued"`
	}
	for _, text := range []string{"one", "two"} {
		c.do("POST", "/api/sessions/"+sid+"/messages", map[string]any{"text": text}, 202, &res)
		if !res.Queued {
			t.Fatalf("%q not queued", text)
		}
	}
	var got struct {
		Session store.Session `json:"session"`
	}
	c.do("GET", "/api/sessions/"+sid, nil, 200, &got)
	if q := got.Session.Queued; len(q) != 2 || q[0].Content != "one" || q[1].Content != "two" {
		t.Fatalf("queued = %+v", q)
	}
	c.do("DELETE", "/api/queued-messages/"+itoa(got.Session.Queued[0].ID), nil, 204, nil)
	c.do("DELETE", "/api/queued-messages/"+itoa(got.Session.Queued[0].ID), nil, 404, nil)
	if text, isErr := c.tool("delete_queued_message", map[string]any{"queued_message_id": got.Session.Queued[1].ID}); isErr {
		t.Fatalf("tool: %s", text)
	}
	var list []store.Session
	c.do("GET", "/api/sessions?task_id="+itoa(task.ID), nil, 200, &list)
	if len(list) != 1 || list[0].Queued == nil || len(list[0].Queued) != 0 {
		t.Fatalf("sessions = %+v", list)
	}

	// Stopped, the session takes the next message at once.
	c.do("POST", "/api/sessions/"+sid+"/cancel", nil, 202, nil)
	c.app.Wait()
	c.do("POST", "/api/sessions/"+sid+"/messages", map[string]any{"text": "now"}, 202, &res)
	c.app.Wait()
	if res.Queued {
		t.Fatal("message to an idle session queued")
	}
}

// The session shows its queued messages under "Working…", each droppable, and
// a reply can be sent while the agent works.
func TestQueuedMessagesUI(t *testing.T) {
	js, err := webFS.ReadFile("web/app.js")
	if err != nil {
		t.Fatal(err)
	}
	css, err := webFS.ReadFile("web/style.css")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"const el = h('div', { class: 'session' }, head, summary, body, working, queued, limits, foot);",
		"send.disabled = !running && !s.claude_session_id;",
		"if (runSkill) runSkill.disabled = running || !s.claude_session_id;",
		"(c.session.queued || []).map(q => h('div', { class: 'queued-msg' },",
		"await api('DELETE', `/api/queued-messages/${q.id}`);",
		"drawWorking();\n    drawQueued();",
	} {
		if !strings.Contains(string(js), want) {
			t.Fatalf("app.js lacks %q", want)
		}
	}
	if !strings.Contains(string(css), "background: color-mix(in srgb, var(--warn) 18%, transparent)") {
		t.Fatal("queued messages lack their orange background")
	}
}
