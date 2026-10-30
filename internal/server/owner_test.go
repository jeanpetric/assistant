package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jeanpetric/assistant/internal/store"
)

// A session another node runs is read-only here: the API refuses what would
// run its agent with 409 naming that node, as do the MCP tools, and its JSON
// says whose it is.
func TestForeignSessionAPI(t *testing.T) {
	c := newClient(t)
	ctx := context.Background()
	var task store.Task
	c.do("POST", "/api/tasks", map[string]any{"title": "t"}, 201, &task)
	var sess store.Session
	c.do("POST", "/api/tasks/"+itoa(task.ID)+"/sessions", map[string]any{"prompt": "hi"}, 201, &sess)
	c.app.Wait()
	if !sess.Owned || sess.NodeID != 1 {
		t.Fatalf("new session = %+v", sess)
	}
	sess, _ = c.app.Store.Session(ctx, sess.ID)
	row, _ := json.Marshal(map[string]any{"id": sess.ID, "status": sess.Status, "mode": sess.Mode,
		"created_at": sess.CreatedAt.UnixMilli(), "updated_at": sess.UpdatedAt.UnixMilli(), "node_id": 2, "node_name": "office-mac"})
	if err := c.app.Store.ApplyChanges(ctx, []store.Change{{Table: "sessions", Key: json.RawMessage(fmt.Sprintf("[%d]", sess.ID)),
		ChangedAtMs: time.Now().Add(time.Hour).UnixMilli(), NodeID: 2, Row: row}}); err != nil {
		t.Fatal(err)
	}
	q, _ := c.app.Store.QueueMessage(ctx, sess.ID, "later")
	sk, _ := c.app.Store.SaveSkill(ctx, store.Skill{Name: "Guard", Body: "Guard it."})
	sid := itoa(sess.ID)

	const want = "session runs on office-mac; open it there"
	for _, x := range []struct {
		method, path string
		body         any
	}{
		{"POST", "/api/sessions/" + sid + "/resume", nil},
		{"POST", "/api/sessions/" + sid + "/cancel", nil},
		{"POST", "/api/sessions/" + sid + "/messages", map[string]any{"text": "more"}},
		{"POST", "/api/sessions/" + sid + "/handoff", map[string]any{}},
		{"POST", "/api/sessions/" + sid + "/skill", map[string]any{"skill_id": sk.ID}},
		{"DELETE", "/api/queued-messages/" + itoa(q.ID), nil},
	} {
		var res struct {
			Error string `json:"error"`
		}
		c.do(x.method, x.path, x.body, http.StatusConflict, &res)
		if res.Error != want {
			t.Errorf("%s %s: %q", x.method, x.path, res.Error)
		}
	}
	if text, isErr := c.tool("send_message", map[string]any{"session_id": sess.ID, "text": "more"}); !isErr || !strings.Contains(text, want) {
		t.Fatalf("tool: %s", text)
	}
	var got struct {
		Session store.Session `json:"session"`
	}
	c.do("GET", "/api/sessions/"+sid, nil, 200, &got)
	if s := got.Session; s.Owned || s.NodeID != 2 || s.NodeName != "office-mac" || s.Status != store.StatusIdle || len(s.Queued) != 1 {
		t.Fatalf("session = %+v", s)
	}
	// Data only: where the task goes when it finishes can still change.
	c.do("PATCH", "/api/sessions/"+sid, map[string]any{"done_lane_id": nil}, 200, nil)
}

// A session another node runs shows read-only: no controls that would run its
// agent, a bar naming its node instead of the reply box, and no Answer for its
// questions on the task's thread.
func TestForeignSessionUI(t *testing.T) {
	js, err := webFS.ReadFile("web/app.js")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"const sessionNode = s => s.node_name || `node ${s.node_id}`;",
		"const resumable = owned && ['interrupted', 'cancelled', 'error'].includes(s.status);",
		"const resumeCmd = owned && s.claude_session_id ?",
		"running && owned ? h('button', { class: 'danger small'",
		"owned && !running && s.claude_session_id ? h('button', {",
		"readOnly.textContent = owned ? '' : `Started on ${sessionNode(s)} — read-only here`;",
		"for (const x of [input, send, runSkill]) if (x) x.hidden = !owned;",
		"hidden: c.session.owned === false,",
		"disabled: !!m.session_node, title: m.session_node ? `Started on ${m.session_node} — read-only here` : null,",
	} {
		if !strings.Contains(string(js), want) {
			t.Fatalf("app.js lacks %q", want)
		}
	}
}
