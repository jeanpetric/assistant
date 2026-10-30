package server

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/jeanpetric/assistant/internal/store"
)

// A session started with report_to (API, standalone or MCP) carries it in its
// JSON, and its finished turn becomes that session's next message. An
// unknown report_to is refused before anything is made.
func TestSessionReportTo(t *testing.T) {
	c := newClient(t)
	ctx := t.Context()
	task, _ := c.app.Store.CreateTask(ctx, store.Task{Title: "Orchestrate"})
	var orch store.Session
	c.do("POST", "/api/tasks/"+itoa(task.ID)+"/sessions", map[string]any{"prompt": "orchestrate"}, 201, &orch)
	c.app.Wait()

	var sub store.Session
	c.do("POST", "/api/tasks/"+itoa(task.ID)+"/sessions", map[string]any{"prompt": "fix it", "title": "Fix", "report_to": orch.ID}, 201, &sub)
	c.app.Wait()
	if sub.ReportTo == nil || *sub.ReportTo != orch.ID {
		t.Fatalf("session = %+v", sub)
	}
	var got struct {
		Session  store.Session   `json:"session"`
		Messages []store.Message `json:"messages"`
	}
	c.do("GET", "/api/sessions/"+itoa(orch.ID), nil, 200, &got)
	want := fmt.Sprintf("Session #%d \"Fix\" (task #%d \"Orchestrate\") finished:\n\nok", sub.ID, task.ID)
	if !hasUserMessage(got.Messages, want) {
		t.Fatalf("orchestrator messages = %+v", got.Messages)
	}
	c.do("GET", "/api/sessions/"+itoa(sub.ID), nil, 200, &got)
	if got.Session.ReportTo == nil || *got.Session.ReportTo != orch.ID {
		t.Fatalf("session JSON = %+v", got.Session)
	}

	var solo store.Session
	c.do("POST", "/api/sessions", map[string]any{"prompt": "chat", "report_to": orch.ID}, 201, &solo)
	c.app.Wait()
	if solo.ReportTo == nil || *solo.ReportTo != orch.ID {
		t.Fatalf("standalone = %+v", solo)
	}

	text, isErr := c.tool("start_session", map[string]any{"task_id": task.ID, "prompt": "dig", "title": "Dig", "report_to": orch.ID})
	var viaMCP store.Session
	if isErr || json.Unmarshal([]byte(text), &viaMCP) != nil || viaMCP.ReportTo == nil || *viaMCP.ReportTo != orch.ID {
		t.Fatalf("start_session: %s", text)
	}
	c.app.Wait()
	c.do("GET", "/api/sessions/"+itoa(orch.ID), nil, 200, &got)
	if !hasUserMessage(got.Messages, fmt.Sprintf("Session #%d \"Dig\"", viaMCP.ID)) {
		t.Fatalf("orchestrator messages = %+v", got.Messages)
	}

	before, _ := c.app.Store.Sessions(ctx, 0, "")
	tasks, _ := c.app.Store.Tasks(ctx, 0)
	c.do("POST", "/api/tasks/"+itoa(task.ID)+"/sessions", map[string]any{"prompt": "x", "report_to": 999}, 400, nil)
	c.do("POST", "/api/sessions", map[string]any{"prompt": "x", "report_to": 999}, 400, nil)
	if text, isErr = c.tool("start_session", map[string]any{"task_id": task.ID, "prompt": "x", "report_to": 999}); !isErr ||
		!strings.Contains(text, "unknown report_to") {
		t.Fatalf("start_session: %s", text)
	}
	after, _ := c.app.Store.Sessions(ctx, 0, "")
	tasksAfter, _ := c.app.Store.Tasks(ctx, 0)
	if len(after) != len(before) || len(tasksAfter) != len(tasks) {
		t.Fatalf("a refused start made %d sessions, %d tasks", len(after)-len(before), len(tasksAfter)-len(tasks))
	}
}

func hasUserMessage(msgs []store.Message, prefix string) bool {
	for _, m := range msgs {
		if m.Role == "user" && strings.HasPrefix(m.Content, prefix) {
			return true
		}
	}
	return false
}
