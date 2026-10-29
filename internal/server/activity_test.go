package server

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/jeanpetric/assistant/internal/app"
	"github.com/jeanpetric/assistant/internal/store"
)

// An agent posts on the task's thread with post_task_message, get_task shows
// the thread, and the user's message answers the open question.
func TestTaskMessagesAPI(t *testing.T) {
	c := newClient(t)
	var task store.Task
	c.do("POST", "/api/tasks", map[string]any{"title": "t"}, 201, &task)
	var sess store.Session
	c.do("POST", "/api/tasks/"+itoa(task.ID)+"/sessions", map[string]any{"prompt": "go", "title": "Guard"}, 201, &sess)
	c.app.Wait()
	tid := itoa(task.ID)

	if text, isErr := c.tool("post_task_message", map[string]any{"task_id": task.ID, "session_id": sess.ID, "body": "Spec rejected, starting Spec refine."}); isErr {
		t.Fatalf("tool: %s", text)
	}
	if text, isErr := c.tool("post_task_message", map[string]any{"task_id": task.ID, "body": "no session"}); !isErr || !strings.Contains(text, "session_id") {
		t.Fatalf("without session_id: %s", text)
	}
	var q app.PostedMessage
	c.do("POST", "/api/tasks/"+tid+"/messages", map[string]any{"session_id": sess.ID, "body": "Show it?", "question": true}, 201, &q)
	if !q.Question || q.SessionTitle != "Guard" {
		t.Fatalf("question = %+v", q)
	}
	text, isErr := c.tool("get_task", map[string]any{"task_id": task.ID})
	if isErr {
		t.Fatal(text)
	}
	var got struct {
		Activity []store.TaskMessage `json:"activity"`
	}
	json.Unmarshal([]byte(text), &got)
	if len(got.Activity) != 2 || got.Activity[0].Body != "Spec rejected, starting Spec refine." || *got.Activity[0].SessionID != sess.ID || got.Activity[1].AnsweredAt != nil {
		t.Fatalf("get_task activity = %s", text)
	}
	var s struct {
		Session store.Session `json:"session"`
	}
	c.do("GET", "/api/sessions/"+itoa(sess.ID), nil, 200, &s)
	if s.Session.NeedsInput == "" {
		t.Fatal("question left the session not waiting")
	}

	var a app.PostedMessage
	c.do("POST", "/api/tasks/"+tid+"/messages", map[string]any{"body": "show it"}, 201, &a)
	c.app.Wait()
	if a.AnsweredID == nil || *a.AnsweredID != q.ID || a.SessionID != nil {
		t.Fatalf("answer = %+v", a)
	}
	var list []store.TaskMessage
	c.do("GET", "/api/tasks/"+tid+"/messages", nil, 200, &list)
	if len(list) != 3 || list[1].AnsweredAt == nil || list[2].Body != "show it" {
		t.Fatalf("thread = %+v", list)
	}
	var after struct {
		Session  store.Session   `json:"session"`
		Messages []store.Message `json:"messages"`
	}
	c.do("GET", "/api/sessions/"+itoa(sess.ID), nil, 200, &after)
	answered := false
	for _, m := range after.Messages {
		answered = answered || m.Role == "user" && strings.HasSuffix(m.Content, "\n\nshow it")
	}
	if after.Session.NeedsInput != "" || !answered {
		t.Fatalf("session after the answer = %+v, %+v", after.Session, after.Messages)
	}

	c.do("POST", "/api/tasks/"+tid+"/messages", map[string]any{"body": " "}, 400, nil)
	c.do("POST", "/api/tasks/"+tid+"/messages", map[string]any{"body": "x", "answer_to": list[0].ID}, 400, nil)
	c.do("POST", "/api/tasks/"+tid+"/messages", map[string]any{"body": "x", "session_id": 999}, 400, nil)
	c.do("POST", "/api/tasks/999/messages", map[string]any{"body": "x"}, 404, nil)
	c.do("GET", "/api/tasks/999/messages", nil, 404, nil)
}
