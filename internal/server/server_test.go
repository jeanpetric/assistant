package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jeanpetric/assistant/internal/agent"
	"github.com/jeanpetric/assistant/internal/app"
	"github.com/jeanpetric/assistant/internal/store"
)

type echoRunner struct{}

func (echoRunner) Run(ctx context.Context, req agent.Request, emit func(agent.Event)) error {
	emit(agent.Event{Kind: "init", ClaudeSessionID: "c-1"})
	emit(agent.Event{Role: "assistant", Kind: "text", Content: "did: " + req.Prompt})
	emit(agent.Event{Role: "system", Kind: "result", Content: "ok"})
	return nil
}

type client struct {
	t   *testing.T
	srv *httptest.Server
	app *app.App
}

func newClient(t *testing.T) *client {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	a := app.New(st, echoRunner{}, "/tmp", "plan")
	srv := httptest.NewServer(New(a))
	t.Cleanup(func() { srv.Close(); a.Wait(); st.Close() })
	return &client{t, srv, a}
}

// do sends a JSON request and decodes the response into out (if non-nil).
func (c *client) do(method, path string, body any, wantStatus int, out any) {
	c.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, c.srv.URL+path, rd)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	if res.StatusCode != wantStatus {
		c.t.Fatalf("%s %s: status %d, want %d: %s", method, path, res.StatusCode, wantStatus, raw)
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			c.t.Fatalf("decode %s: %v", raw, err)
		}
	}
}

type board struct {
	Lanes  []store.Lane     `json:"lanes"`
	Tasks  []store.Task     `json:"tasks"`
	Fields []store.FieldDef `json:"fields"`
}

func TestBoardFlow(t *testing.T) {
	c := newClient(t)
	var b board
	c.do("GET", "/api/board", nil, 200, &b)
	if len(b.Lanes) != 5 || len(b.Fields) != 3 {
		t.Fatalf("board = %+v", b)
	}
	todo, wip := b.Lanes[1].ID, b.Lanes[2].ID

	var task store.Task
	c.do("POST", "/api/tasks", map[string]any{"title": "Write spec", "lane_id": todo, "kind": "research"}, 201, &task)
	c.do("POST", "/api/tasks", map[string]any{"title": ""}, 400, nil)

	c.do("PATCH", "/api/tasks/"+itoa(task.ID), map[string]any{"fields": map[string]string{"pull_request": "https://github.com/a/b/pull/9"}}, 200, &task)
	if task.Fields["pull_request"] == "" || task.Title != "Write spec" {
		t.Fatalf("patched %+v", task)
	}

	var rule store.Rule
	c.do("POST", "/api/rules", map[string]any{"name": "Kickoff", "to_lane_id": wip, "prompt": "Work on {{.Task.Title}}", "enabled": true}, 200, &rule)
	c.do("POST", "/api/rules", map[string]any{"name": "Bad", "to_lane_id": wip, "prompt": "{{.Broken"}, 400, nil)
	c.do("POST", "/api/rules", map[string]any{"name": "Bad", "to_lane_id": wip, "prompt": "x", "mode": "yolo"}, 400, nil)

	var moved struct {
		Started []store.Session `json:"started"`
	}
	c.do("POST", "/api/tasks/"+itoa(task.ID)+"/move", map[string]any{"lane_id": wip, "index": 0}, 200, &moved)
	if len(moved.Started) != 1 || moved.Started[0].Mode != "plan" {
		t.Fatalf("moved = %+v", moved)
	}
	c.app.Wait()

	var detail struct {
		Task     store.Task `json:"task"`
		Sessions []struct {
			store.Session
			Messages []store.Message `json:"messages"`
		} `json:"sessions"`
	}
	c.do("GET", "/api/tasks/"+itoa(task.ID), nil, 200, &detail)
	if detail.Task.LaneID != wip || len(detail.Sessions) != 1 || len(detail.Sessions[0].Messages) != 3 {
		t.Fatalf("detail = %+v", detail)
	}
	if got := detail.Sessions[0].Messages[1].Content; got != "did: Work on Write spec" {
		t.Fatalf("assistant said %q", got)
	}

	sid := itoa(detail.Sessions[0].ID)
	c.do("POST", "/api/sessions/"+sid+"/messages", map[string]any{"text": "more"}, 202, nil)
	c.app.Wait()
	var sess struct {
		Session  store.Session   `json:"session"`
		Messages []store.Message `json:"messages"`
	}
	c.do("GET", "/api/sessions/"+sid+"?after="+itoa(detail.Sessions[0].Messages[2].ID), nil, 200, &sess)
	if len(sess.Messages) != 3 || sess.Session.Status != store.StatusIdle {
		t.Fatalf("follow-up = %+v", sess)
	}

	var all []store.Session
	c.do("GET", "/api/sessions", nil, 200, &all)
	if len(all) != 1 || all[0].TaskTitle != "Write spec" {
		t.Fatalf("sessions = %+v", all)
	}

	// Manual session with task context.
	var manual store.Session
	c.do("POST", "/api/tasks/"+itoa(task.ID)+"/sessions", map[string]any{"prompt": "summarise", "include_context": true}, 201, &manual)
	c.app.Wait()
	c.do("GET", "/api/sessions/"+itoa(manual.ID), nil, 200, &sess)
	if !strings.HasPrefix(sess.Messages[0].Content, "Task #1: Write spec") || !strings.HasSuffix(sess.Messages[0].Content, "summarise") {
		t.Fatalf("prompt = %q", sess.Messages[0].Content)
	}

	c.do("DELETE", "/api/tasks/"+itoa(task.ID), nil, 204, nil)
	c.do("GET", "/api/tasks/"+itoa(task.ID), nil, 404, nil)
}

func TestLanesFieldsAndSchedules(t *testing.T) {
	c := newClient(t)
	var lane store.Lane
	c.do("POST", "/api/lanes", map[string]any{"name": "Blocked"}, 201, &lane)
	c.do("POST", "/api/lanes", map[string]any{"name": " "}, 400, nil)
	c.do("PATCH", "/api/lanes/"+itoa(lane.ID), map[string]any{"name": "Waiting", "color": "#f00"}, 204, nil)

	var task store.Task
	c.do("POST", "/api/tasks", map[string]any{"title": "x", "lane_id": lane.ID}, 201, &task)
	c.do("DELETE", "/api/lanes/"+itoa(lane.ID), nil, 409, nil)

	c.do("POST", "/api/fields", map[string]any{"key": "Bad Key", "type": "url"}, 400, nil)
	c.do("POST", "/api/fields", map[string]any{"key": "figma", "label": "Figma", "type": "url"}, 204, nil)
	c.do("PATCH", "/api/tasks/"+itoa(task.ID), map[string]any{"fields": map[string]string{"figma": "https://figma.com/x"}}, 200, &task)
	if task.Fields["figma"] == "" {
		t.Fatal("custom field not saved")
	}

	var sched store.Schedule
	c.do("POST", "/api/tasks/"+itoa(task.ID)+"/schedules", map[string]any{"cron": "0 9 * * 1-5", "action": "remind", "enabled": true}, 200, &sched)
	if sched.NextRunAt == nil {
		t.Fatal("next run not computed")
	}
	c.do("POST", "/api/tasks/"+itoa(task.ID)+"/schedules", map[string]any{"cron": "nope", "action": "remind"}, 400, nil)
	c.do("POST", "/api/tasks/"+itoa(task.ID)+"/schedules", map[string]any{"cron": "@daily", "action": "agent"}, 400, nil)
	c.do("PUT", "/api/schedules/"+itoa(sched.ID), map[string]any{"cron": "@hourly", "action": "remind", "enabled": false}, 200, &sched)
	if sched.Enabled || sched.Cron != "@hourly" || sched.TaskID != task.ID {
		t.Fatalf("updated = %+v", sched)
	}
	c.do("DELETE", "/api/schedules/"+itoa(sched.ID), nil, 204, nil)
}

func TestTaskDueAt(t *testing.T) {
	c := newClient(t)
	var task store.Task
	c.do("POST", "/api/tasks", map[string]any{"title": "Call Ana", "kind": "reminder", "due_at": "2026-10-20T15:00:00Z"}, 201, &task)
	want := time.Date(2026, 10, 20, 15, 0, 0, 0, time.UTC)
	if task.DueAt == nil || !task.DueAt.Equal(want) {
		t.Fatalf("created %+v", task)
	}
	c.do("PATCH", "/api/tasks/"+itoa(task.ID), map[string]any{"title": "Call Ana back"}, 200, &task)
	if task.DueAt == nil || !task.DueAt.Equal(want) {
		t.Fatalf("absent due_at changed it: %+v", task)
	}
	c.do("PATCH", "/api/tasks/"+itoa(task.ID), map[string]any{"due_at": "tomorrow"}, 400, nil)
	c.do("PATCH", "/api/tasks/"+itoa(task.ID), map[string]any{"due_at": nil}, 200, &task)
	if task.DueAt != nil {
		t.Fatalf("null due_at kept %v", task.DueAt)
	}
}

func TestGuard(t *testing.T) {
	c := newClient(t)
	post := func(ct, origin, host string) int {
		req, _ := http.NewRequest("POST", c.srv.URL+"/api/tasks", strings.NewReader(`{"title":"x"}`))
		if ct != "" {
			req.Header.Set("Content-Type", ct)
		}
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		if host != "" {
			req.Host = host
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		return res.StatusCode
	}
	self := strings.TrimPrefix(c.srv.URL, "http://")
	if got := post("text/plain", "", ""); got != http.StatusUnsupportedMediaType {
		t.Errorf("text/plain: %d", got)
	}
	if got := post("application/json", "https://evil.example", ""); got != http.StatusForbidden {
		t.Errorf("foreign origin: %d", got)
	}
	if got := post("application/json", "", "evil.example:7777"); got != http.StatusForbidden {
		t.Errorf("foreign host: %d", got)
	}
	if got := post("application/json", "http://"+self, ""); got != http.StatusCreated {
		t.Errorf("same origin: %d", got)
	}
}

func TestStaticUI(t *testing.T) {
	c := newClient(t)
	res, err := http.Get(c.srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != 200 || !bytes.Contains(body, []byte("app.js")) {
		t.Fatalf("index: %d %s", res.StatusCode, body)
	}
}

func itoa(n int64) string {
	b, _ := json.Marshal(n)
	return string(b)
}

func TestStandaloneSession(t *testing.T) {
	c := newClient(t)
	dir := t.TempDir()
	var sess store.Session
	c.do("POST", "/api/sessions", map[string]any{"prompt": "hello", "model": "sonnet", "workdir": dir + "/"}, 201, &sess)
	c.app.Wait()
	if sess.TaskID != 0 || sess.Model != "sonnet" || sess.Workdir != dir {
		t.Fatalf("session = %+v", sess)
	}
	var got struct {
		Session  store.Session   `json:"session"`
		Messages []store.Message `json:"messages"`
	}
	c.do("GET", "/api/sessions/"+itoa(sess.ID), nil, 200, &got)
	if got.Session.Status != store.StatusIdle || len(got.Messages) != 3 || got.Messages[1].Content != "did: hello" {
		t.Fatalf("got = %+v", got)
	}

	var b struct {
		Running map[string]int `json:"running"`
	}
	c.do("GET", "/api/board", nil, 200, &b)
	if len(b.Running) != 0 {
		t.Fatalf("standalone sessions must not count on the board: %v", b.Running)
	}
	var dirs []string
	c.do("GET", "/api/workdirs", nil, 200, &dirs)
	if len(dirs) != 1 || dirs[0] != dir {
		t.Fatalf("workdirs = %v", dirs)
	}

	c.do("POST", "/api/sessions/"+itoa(sess.ID)+"/resume", nil, 202, nil)
	c.app.Wait()
	c.do("GET", "/api/sessions/"+itoa(sess.ID), nil, 200, &got)
	if n := len(got.Messages); n != 6 || got.Messages[3].Content != app.ResumePrompt {
		t.Fatalf("resume transcript = %+v", got.Messages)
	}
	c.do("POST", "/api/sessions/999/resume", nil, 404, nil)

	c.do("POST", "/api/sessions", map[string]any{"prompt": " "}, 400, nil)
	c.do("POST", "/api/sessions", map[string]any{"prompt": "x", "workdir": dir + "/missing"}, 400, nil)
	c.do("POST", "/api/sessions", map[string]any{"prompt": "x", "workdir": "relative/dir"}, 400, nil)
	c.do("POST", "/api/sessions", map[string]any{"prompt": "x", "model": "--dangerously-skip"}, 400, nil)
	c.do("POST", "/api/sessions", map[string]any{"prompt": "x", "mode": "yolo"}, 400, nil)
}
