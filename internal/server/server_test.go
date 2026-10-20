package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
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
	emit(agent.Event{Kind: "rate_limit", Limits: &agent.RateLimits{
		FiveHour: &agent.LimitWindow{UsedPercent: 28, ResetsAt: time.Now().Add(2 * time.Hour)}}})
	emit(agent.Event{Kind: "usage", ContextTokens: 42000})
	if req.Prompt == "block" { // runs until cancelled
		<-ctx.Done()
		return ctx.Err()
	}
	emit(agent.Event{Role: "assistant", Kind: "text", Content: "did: " + req.Prompt})
	result := "ok"
	if req.Fork { // a session summary
		result = "SUMMARY of " + req.ResumeID
	}
	if req.Prompt == "ask" {
		result = "Which branch?"
	}
	emit(agent.Event{Role: "system", Kind: "result", Content: result})
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
	Projects  []store.Project  `json:"projects"`
	ProjectID int64            `json:"project_id"`
	Lanes     []store.Lane     `json:"lanes"`
	Tasks     []store.Task     `json:"tasks"`
	Fields    []store.FieldDef `json:"fields"`
}

func TestBoardFlow(t *testing.T) {
	c := newClient(t)
	var b board
	c.do("GET", "/api/board", nil, 200, &b)
	if len(b.Lanes) != 5 || len(b.Fields) != 4 {
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

func TestSessionLimits(t *testing.T) {
	c := newClient(t)
	var task store.Task
	c.do("POST", "/api/tasks", map[string]any{"title": "t"}, 201, &task)
	var sess store.Session
	c.do("POST", "/api/tasks/"+itoa(task.ID)+"/sessions", map[string]any{"prompt": "block"}, 201, &sess)

	// While it runs, the board shows the session's limits and context on its task's card.
	var b struct {
		RunningLimits  map[string]agent.RateLimits `json:"running_limits"`
		RunningContext map[string]struct {
			Tokens int `json:"tokens"`
		} `json:"running_context"`
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		c.do("GET", "/api/board", nil, 200, &b)
		if b.RunningLimits[itoa(task.ID)].FiveHour != nil || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if w := b.RunningLimits[itoa(task.ID)].FiveHour; w == nil || w.UsedPercent != 28 || b.RunningLimits[itoa(task.ID)].SevenDay != nil {
		t.Fatalf("running limits = %+v", b.RunningLimits)
	}
	if got := b.RunningContext[itoa(task.ID)].Tokens; got != 42000 {
		t.Fatalf("running context = %+v", b.RunningContext)
	}

	// After the turn the session keeps its last known limits; the board no longer shows them.
	c.do("POST", "/api/sessions/"+itoa(sess.ID)+"/cancel", nil, 202, nil)
	c.app.Wait()
	var got struct {
		Session struct {
			store.Session
			Limits agent.RateLimits `json:"limits"`
		} `json:"session"`
	}
	c.do("GET", "/api/sessions/"+itoa(sess.ID), nil, 200, &got)
	if w := got.Session.Limits.FiveHour; got.Session.Status != store.StatusCancelled || w == nil || w.UsedPercent != 28 || w.ResetsAt.Before(time.Now()) ||
		got.Session.ContextTokens != 42000 {
		t.Fatalf("session = %+v", got.Session)
	}
	b.RunningLimits, b.RunningContext = nil, nil
	c.do("GET", "/api/board", nil, 200, &b)
	if len(b.RunningLimits) != 0 || len(b.RunningContext) != 0 {
		t.Fatalf("running limits after the turn = %+v", b.RunningLimits)
	}
	c.do("GET", "/api/limits", nil, 404, nil)
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
	// The board opens a task in this dialog first; app.js looks it up by id.
	if !bytes.Contains(body, []byte(`<dialog id="task-dialog"`)) {
		t.Fatalf("index has no task dialog: %s", body)
	}
	res, err = http.Get(c.srv.URL + "/app.js")
	if err != nil {
		t.Fatal(err)
	}
	js, _ := io.ReadAll(res.Body)
	res.Body.Close()
	for _, want := range []string{"getElementById('task-dialog')", "function workdirSelect(", "workdirsPanel(settings.workdirs, rerender)",
		// Tool calls fold into one collapsed group per run instead of one row each.
		"appendMessage(body, m)", "h('details', { class: 'tools' }", "onclick: e => { if (e.metaKey || e.ctrlKey) location.hash = `#/task/${t.id}`; else openTaskDialog(t.id); }",
		// Keyboard shortcuts come from one table, which the "?" dialog lists.
		"document.addEventListener('keydown', onShortcut)", "{ keys: '⌥T', code: 'KeyT', label: 'New task', run: newTaskDialog }",
		"{ keys: '⌥B', code: 'KeyB', label: 'Go to Board'", "{ keys: '?', label: 'Show these shortcuts', run: shortcutsDialog }",
		// Shortcut hints are drawn greyed next to the controls they trigger.
		"function hint(keys)", "drawHeaderHints();", "'Cancel', hint('Esc')", "'Create', hint('⌘↵')", "submitOnCmdEnter([title, desc, kind, project, lane, due], create)", "filter, hint('/')",
		"addEventListener('click', shortcutsDialog)",
		// Esc is left to the dialogs (the task dialog included) and closes the notifications.
		"if (e.key === 'Escape' && !inField) { closeNotifications(); return; }",
		// A project's directory is picked in Settings and shown as the default for its tasks.
		"function taskWorkdir(t)", "workdir.addEventListener('change', saveProject)", "`Default (${taskWorkdir(task)})`",
		// The new task dialog drops its missing "Builds on" line instead of drawing "null".
		"dlg.replaceChildren(...[h('h2', {}, from ? 'New follow-up task' : opts.memory ? `New task on ${opts.memory.name}` : 'New task')", "hint('⌘↵')))].filter(Boolean));",
		// A new task is suggested memories by its words; a memory starts a task on its topic, a task a memory.
		"const memories = memoryPicker(title, desc);", "memories: memories.ids()", "newTaskDialog({ memory: m })", "memoryDialog({ task, done:",
		// Update memory shows only on a session whose task has a memory.
		"!running && !s.memorizing && task?.memories.length && s.claude_session_id",
		// A created task shows on its project's board, not full screen.
		"remember('project', t.project_id);\n    go('#/board');"} {
		if !bytes.Contains(js, []byte(want)) {
			t.Fatalf("app.js lacks %q", want)
		}
	}
	// List, Archive and Memory focus their filter on open; ⌥ shortcuts still work from inside it.
	if n := bytes.Count(js, []byte("filter.focus()")); n != 3 {
		t.Fatalf("app.js focuses a filter %d times, want List, Archive and Memory", n)
	}
	if bytes.Contains(js, []byte("blur()")) {
		t.Fatal("app.js blurs a field; Esc belongs to the dialogs")
	}
	// Every header hint names a real shortcut (or, like "[ ]", each key of one), so a hint never lies.
	hints := regexp.MustCompile(`data-shortcut="([^"]+)"`).FindAllSubmatch(body, -1)
	if len(hints) < 9 {
		t.Fatalf("index has %d shortcut hints, want the nav links and header buttons", len(hints))
	}
	for _, m := range hints {
		keys := string(m[1])
		if bytes.Contains(js, []byte("{ keys: '"+keys+"'")) {
			continue
		}
		for _, k := range strings.Fields(keys) {
			if !bytes.Contains(js, []byte("{ keys: '"+k+"'")) {
				t.Fatalf("hint %q names no shortcut", keys)
			}
		}
	}
	// The dialog's columns shrink and stack by its own width, so it never scrolls sideways.
	res, err = http.Get(c.srv.URL + "/style.css")
	if err != nil {
		t.Fatal(err)
	}
	css, _ := io.ReadAll(res.Body)
	res.Body.Close()
	for _, want := range []string{"container-type: inline-size", "@container (max-width: 860px)", ".task-dialog .task-page { grid-template-columns: minmax(0, 380px) minmax(0, 1fr); }"} {
		if !bytes.Contains(css, []byte(want)) {
			t.Fatalf("style.css lacks %q", want)
		}
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
	c.do("POST", "/api/sessions/"+itoa(sess.ID)+"/resume", nil, 202, nil)
	c.app.Wait()
	c.do("GET", "/api/sessions/"+itoa(sess.ID), nil, 200, &got)
	if n := len(got.Messages); n != 6 || got.Messages[3].Content != app.ResumePrompt {
		t.Fatalf("resume transcript = %+v", got.Messages)
	}
	c.do("POST", "/api/sessions/999/resume", nil, 404, nil)

	c.do("POST", "/api/sessions/"+itoa(sess.ID)+"/handoff", map[string]any{"note": "next"}, 202, nil)
	c.app.Wait()
	c.do("GET", "/api/sessions/"+itoa(sess.ID), nil, 200, &got)
	if got.Session.ChildID == nil {
		t.Fatalf("no new session after handoff: %+v", got.Session)
	}
	var child struct {
		Session store.Session `json:"session"`
	}
	c.do("GET", "/api/sessions/"+itoa(*got.Session.ChildID), nil, 200, &child)
	if child.Session.ParentID == nil || *child.Session.ParentID != sess.ID || child.Session.Workdir != dir || child.Session.Model != "sonnet" {
		t.Fatalf("child = %+v", child.Session)
	}
	c.do("POST", "/api/sessions/999/handoff", map[string]any{}, 404, nil)

	c.do("POST", "/api/sessions", map[string]any{"prompt": " "}, 400, nil)
	c.do("POST", "/api/sessions", map[string]any{"prompt": "x", "workdir": dir + "/missing"}, 400, nil)
	c.do("POST", "/api/sessions", map[string]any{"prompt": "x", "workdir": "relative/dir"}, 400, nil)
	c.do("POST", "/api/sessions", map[string]any{"prompt": "x", "model": "--dangerously-skip"}, 400, nil)
	c.do("POST", "/api/sessions", map[string]any{"prompt": "x", "mode": "yolo"}, 400, nil)
	c.do("POST", "/api/sessions", map[string]any{"prompt": "x", "project_id": 999}, 400, nil)

	var p store.Project
	c.do("POST", "/api/projects", map[string]any{"name": "Web", "workdir": dir}, 201, &p)
	var inProject store.Session
	c.do("POST", "/api/sessions", map[string]any{"prompt": "x", "project_id": p.ID}, 201, &inProject)
	c.app.Wait()
	if inProject.Workdir != dir {
		t.Fatalf("standalone session should run in its project's directory: %+v", inProject)
	}
}

func TestDoneLane(t *testing.T) {
	c := newClient(t)
	var b board
	c.do("GET", "/api/board", nil, 200, &b)
	lane := map[string]int64{}
	for _, l := range b.Lanes {
		lane[l.Name] = l.ID
	}
	var task store.Task
	c.do("POST", "/api/tasks", map[string]any{"title": "T", "lane_id": lane["In progress"]}, 201, &task)
	path := "/api/tasks/" + itoa(task.ID)

	var sess store.Session
	c.do("POST", path+"/sessions", map[string]any{"prompt": "p", "done_lane_id": lane["Review"]}, 201, &sess)
	c.app.Wait()
	got, _ := c.app.Store.Task(context.Background(), task.ID)
	if got.LaneID != lane["Review"] {
		t.Fatalf("task in lane %d, want Review", got.LaneID)
	}
	c.do("POST", path+"/sessions", map[string]any{"prompt": "p", "done_lane_id": 999}, 400, nil)

	var upd store.Session
	c.do("PATCH", "/api/sessions/"+itoa(sess.ID), map[string]any{"done_lane_id": nil}, 200, &upd)
	if upd.DoneLaneID != nil {
		t.Fatalf("done lane not cleared: %+v", upd)
	}
	c.do("PATCH", "/api/sessions/"+itoa(sess.ID), map[string]any{"done_lane_id": lane["Done"]}, 200, &upd)
	if upd.DoneLaneID == nil || *upd.DoneLaneID != lane["Done"] {
		t.Fatalf("done lane not set: %+v", upd)
	}
	c.do("PATCH", "/api/sessions/"+itoa(sess.ID), map[string]any{"done_lane_id": 999}, 400, nil)
	c.do("PATCH", "/api/sessions/999", map[string]any{"done_lane_id": nil}, 404, nil)

	var standalone store.Session
	c.do("POST", "/api/sessions", map[string]any{"prompt": "p"}, 201, &standalone)
	c.do("PATCH", "/api/sessions/"+itoa(standalone.ID), map[string]any{"done_lane_id": lane["Review"]}, 400, nil)

	var rule store.Rule
	c.do("POST", "/api/rules", map[string]any{"name": "r", "to_lane_id": lane["Todo"], "prompt": "p", "done_lane_id": lane["Review"]}, 200, &rule)
	if rule.DoneLaneID == nil || *rule.DoneLaneID != lane["Review"] {
		t.Fatalf("rule = %+v", rule)
	}
	c.do("POST", "/api/rules", map[string]any{"name": "r", "to_lane_id": lane["Todo"], "prompt": "p", "done_lane_id": 999}, 400, nil)

	var sched store.Schedule
	c.do("POST", path+"/schedules", map[string]any{"cron": "@daily", "action": "agent", "prompt": "p", "done_lane_id": lane["Review"]}, 200, &sched)
	if sched.DoneLaneID == nil || *sched.DoneLaneID != lane["Review"] {
		t.Fatalf("schedule = %+v", sched)
	}
	c.do("POST", path+"/schedules", map[string]any{"cron": "@daily", "action": "agent", "prompt": "p", "done_lane_id": 999}, 400, nil)
}

func TestArchive(t *testing.T) {
	c := newClient(t)
	var b board
	c.do("GET", "/api/board", nil, 200, &b)
	lane := map[string]int64{}
	for _, l := range b.Lanes {
		lane[l.Name] = l.ID
	}

	var set store.Settings
	c.do("GET", "/api/settings", nil, 200, &set)
	if set.ArchiveLaneID == nil || *set.ArchiveLaneID != lane["Done"] || set.ArchiveAfterMinutes != 7*24*60 {
		t.Fatalf("default settings = %+v", set)
	}
	c.do("PUT", "/api/settings", map[string]any{"archive_lane_id": lane["Review"], "archive_after_minutes": 30}, 200, &set)
	c.do("GET", "/api/settings", nil, 200, &set)
	if *set.ArchiveLaneID != lane["Review"] || set.ArchiveAfterMinutes != 30 {
		t.Fatalf("saved settings = %+v", set)
	}
	c.do("PUT", "/api/settings", map[string]any{"archive_lane_id": 999, "archive_after_minutes": 30}, 400, nil)
	c.do("PUT", "/api/settings", map[string]any{"archive_lane_id": nil, "archive_after_minutes": -1}, 400, nil)
	c.do("PUT", "/api/settings", map[string]any{"archive_lane_id": nil, "archive_after_minutes": 0}, 200, nil)

	// Workdirs are resolved, deduplicated and kept when a save leaves them out.
	a, b2 := t.TempDir(), t.TempDir()
	c.do("PUT", "/api/settings", map[string]any{"archive_after_minutes": 0, "workdirs": []string{a + "/", " ", b2, a}}, 200, &set)
	if len(set.Workdirs) != 2 || set.Workdirs[0] != a || set.Workdirs[1] != b2 {
		t.Fatalf("workdirs = %v", set.Workdirs)
	}
	c.do("PUT", "/api/settings", map[string]any{"archive_after_minutes": 0, "workdirs": []string{"relative"}}, 400, nil)
	c.do("PUT", "/api/settings", map[string]any{"archive_after_minutes": 0, "workdirs": []string{a + "/missing"}}, 400, nil)
	c.do("PUT", "/api/settings", map[string]any{"archive_after_minutes": 5}, 200, nil)
	c.do("GET", "/api/settings", nil, 200, &set)
	if len(set.Workdirs) != 2 || set.ArchiveAfterMinutes != 5 {
		t.Fatalf("after save without workdirs = %+v", set)
	}
	c.do("PUT", "/api/settings", map[string]any{"archive_after_minutes": 0, "workdirs": []string{}}, 200, &set)
	if set.Workdirs == nil || len(set.Workdirs) != 0 {
		t.Fatalf("cleared workdirs = %v", set.Workdirs)
	}

	var task store.Task
	c.do("POST", "/api/tasks", map[string]any{"title": "T", "lane_id": lane["Done"]}, 201, &task)
	path := "/api/tasks/" + itoa(task.ID)
	var got store.Task
	c.do("POST", path+"/archive", nil, 200, &got)
	if got.ArchivedAt == nil {
		t.Fatalf("archive = %+v", got)
	}
	c.do("GET", "/api/board", nil, 200, &b)
	if len(b.Tasks) != 0 {
		t.Fatalf("archived task on the board: %+v", b.Tasks)
	}
	var arch []store.Task
	c.do("GET", "/api/archive", nil, 200, &arch)
	if len(arch) != 1 || arch[0].ID != task.ID {
		t.Fatalf("archive list = %+v", arch)
	}
	var detail struct {
		Task store.Task `json:"task"`
	}
	c.do("GET", path, nil, 200, &detail)
	if detail.Task.ArchivedAt == nil {
		t.Fatal("task page should see archived_at")
	}

	c.do("POST", path+"/unarchive", nil, 200, &got)
	if got.ArchivedAt != nil || got.LaneID != lane["Done"] {
		t.Fatalf("unarchive = %+v", got)
	}
	c.do("GET", "/api/archive", nil, 200, &arch)
	c.do("GET", "/api/board", nil, 200, &b)
	if len(arch) != 0 || len(b.Tasks) != 1 {
		t.Fatalf("after restore: archive %d, board %d", len(arch), len(b.Tasks))
	}
	c.do("POST", "/api/tasks/999/archive", nil, 404, nil)
	c.do("POST", "/api/tasks/999/unarchive", nil, 404, nil)
}

func TestProjects(t *testing.T) {
	c := newClient(t)
	var b board
	c.do("GET", "/api/board", nil, 200, &b)
	if len(b.Projects) != 1 || b.ProjectID != b.Projects[0].ID {
		t.Fatalf("board = %+v", b)
	}
	work := b.ProjectID
	var private store.Project
	c.do("POST", "/api/projects", map[string]any{"name": " Private "}, 201, &private)
	c.do("POST", "/api/projects", map[string]any{"name": ""}, 400, nil)
	c.do("PATCH", "/api/projects/"+itoa(private.ID), map[string]any{"name": "Home", "color": "#0a0"}, 204, nil)
	c.do("PATCH", "/api/projects/999", map[string]any{"name": "x"}, 404, nil)

	// The workdir is resolved like any other and kept when a save leaves it out.
	dir := t.TempDir()
	c.do("PATCH", "/api/projects/"+itoa(private.ID), map[string]any{"name": "Home", "workdir": dir + "/"}, 204, nil)
	c.do("PATCH", "/api/projects/"+itoa(private.ID), map[string]any{"name": "Home", "color": "#0a0"}, 204, nil)
	c.do("PATCH", "/api/projects/"+itoa(private.ID), map[string]any{"name": "Home", "workdir": "relative"}, 400, nil)
	c.do("POST", "/api/projects", map[string]any{"name": "Bad", "workdir": dir + "/missing"}, 400, nil)
	if p, _ := c.app.Store.Project(context.Background(), private.ID); p.Workdir != dir || p.Color != "#0a0" {
		t.Fatalf("project = %+v", p)
	}

	var a, x store.Task
	c.do("POST", "/api/tasks", map[string]any{"title": "a"}, 201, &a)
	c.do("POST", "/api/tasks", map[string]any{"title": "x", "project_id": private.ID}, 201, &x)
	c.do("POST", "/api/tasks", map[string]any{"title": "bad", "project_id": 999}, 400, nil)
	if a.ProjectID != work || x.ProjectID != private.ID {
		t.Fatalf("a = %+v, x = %+v", a, x)
	}

	c.do("GET", "/api/board?project_id="+itoa(private.ID), nil, 200, &b)
	if b.ProjectID != private.ID || len(b.Tasks) != 1 || b.Tasks[0].ID != x.ID || b.Projects[1].Name != "Home" {
		t.Fatalf("private board = %+v", b)
	}
	// An unknown project falls back to the first.
	c.do("GET", "/api/board?project_id=999", nil, 200, &b)
	if b.ProjectID != work || len(b.Tasks) != 1 || b.Tasks[0].ID != a.ID {
		t.Fatalf("fallback board = %+v", b)
	}
	c.do("GET", "/api/board?project_id=nope", nil, 400, nil)

	// PATCH project_id moves the task; other edits in the same request apply too.
	var got store.Task
	c.do("PATCH", "/api/tasks/"+itoa(a.ID), map[string]any{"project_id": private.ID, "title": "a2"}, 200, &got)
	if got.ProjectID != private.ID || got.Title != "a2" {
		t.Fatalf("moved = %+v", got)
	}
	c.do("PATCH", "/api/tasks/"+itoa(a.ID), map[string]any{"project_id": 999}, 400, nil)

	c.do("POST", "/api/tasks/"+itoa(x.ID)+"/archive", nil, 200, nil)
	var arch []store.Task
	c.do("GET", "/api/archive?project_id="+itoa(work), nil, 200, &arch)
	if len(arch) != 0 {
		t.Fatalf("work archive = %+v", arch)
	}
	c.do("GET", "/api/archive?project_id="+itoa(private.ID), nil, 200, &arch)
	if len(arch) != 1 {
		t.Fatalf("private archive = %+v", arch)
	}

	c.do("DELETE", "/api/projects/"+itoa(private.ID), nil, 409, nil)
	c.do("DELETE", "/api/projects/"+itoa(work), nil, 204, nil)
	c.do("POST", "/api/projects/reorder", map[string]any{"ids": []int64{private.ID}}, 204, nil)
	c.do("DELETE", "/api/tasks/"+itoa(a.ID), nil, 204, nil)
	c.do("DELETE", "/api/tasks/"+itoa(x.ID), nil, 204, nil)
	c.do("DELETE", "/api/projects/"+itoa(private.ID), nil, 409, nil) // the last one stays
}

func TestTaskContextSharing(t *testing.T) {
	c := newClient(t)
	var research, follow store.Task
	c.do("POST", "/api/tasks", map[string]any{"title": "Research"}, 201, &research)
	c.do("POST", "/api/tasks/"+itoa(research.ID)+"/sessions", map[string]any{"prompt": "find things"}, 201, nil)
	c.app.Wait()

	c.do("POST", "/api/tasks", map[string]any{"title": "Follow-up", "context_from": []int64{research.ID}}, 201, &follow)
	if len(follow.ContextFrom) != 1 || follow.ContextFrom[0] != research.ID {
		t.Fatalf("follow = %+v", follow)
	}
	c.do("POST", "/api/tasks", map[string]any{"title": "x", "context_from": []int64{999}}, 400, nil)

	var page struct {
		BuildsOn []taskRef `json:"builds_on"`
		UsedBy   []taskRef `json:"used_by"`
	}
	c.do("GET", "/api/tasks/"+itoa(follow.ID), nil, 200, &page)
	if len(page.BuildsOn) != 1 || page.BuildsOn[0].Title != "Research" || len(page.UsedBy) != 0 {
		t.Fatalf("follow page = %+v", page)
	}
	c.do("GET", "/api/tasks/"+itoa(research.ID), nil, 200, &page)
	if len(page.UsedBy) != 1 || page.UsedBy[0].ID != follow.ID {
		t.Fatalf("research page = %+v", page)
	}

	// The research agent's result reaches the follow-up's agent with the task context.
	var sess store.Session
	c.do("POST", "/api/tasks/"+itoa(follow.ID)+"/sessions", map[string]any{"prompt": "make tasks", "include_context": true}, 201, &sess)
	c.app.Wait()
	var got struct {
		Messages []store.Message `json:"messages"`
	}
	c.do("GET", "/api/sessions/"+itoa(sess.ID), nil, 200, &got)
	if p := got.Messages[0].Content; !strings.Contains(p, app.SharedIntro) || !strings.Contains(p, "## Task #"+itoa(research.ID)+": Research") ||
		!strings.Contains(p, "\n\nok\n") || !strings.HasSuffix(p, "make tasks") {
		t.Fatalf("prompt = %q", p)
	}
	// Without include_context the agent starts from scratch.
	c.do("POST", "/api/tasks/"+itoa(follow.ID)+"/sessions", map[string]any{"prompt": "plain"}, 201, &sess)
	c.app.Wait()
	c.do("GET", "/api/sessions/"+itoa(sess.ID), nil, 200, &got)
	if p := got.Messages[0].Content; p != "plain" {
		t.Fatalf("prompt = %q", p)
	}

	// That brief had the final reply and summarised the research session in
	// the background; the next brief has the summary instead.
	var sessions []sessionOut
	c.do("GET", "/api/sessions?task_id="+itoa(research.ID), nil, 200, &sessions)
	if len(sessions) != 1 || sessions[0].Summary != "SUMMARY of c-1" || sessions[0].SummaryStale || sessions[0].Summarizing {
		t.Fatalf("research sessions = %+v", sessions)
	}
	researchSess := sessions[0].ID
	c.do("POST", "/api/tasks/"+itoa(follow.ID)+"/sessions", map[string]any{"prompt": "again", "include_context": true}, 201, &sess)
	c.app.Wait()
	c.do("GET", "/api/sessions/"+itoa(sess.ID), nil, 200, &got)
	if p := got.Messages[0].Content; !strings.Contains(p, "### Summary of session "+itoa(researchSess)) || !strings.Contains(p, "SUMMARY of c-1") ||
		strings.Contains(p, "Final reply of session") {
		t.Fatalf("prompt = %q", p)
	}

	// A follow-up in the research session makes its summary stale until it is
	// rewritten (here after the follow-up's turn, as a task builds on it).
	c.do("POST", "/api/sessions/"+itoa(researchSess)+"/messages", map[string]any{"text": "block"}, 202, nil)
	c.do("POST", "/api/sessions/"+itoa(researchSess)+"/summary", nil, 409, nil)
	var one struct {
		Session sessionOut `json:"session"`
	}
	c.do("GET", "/api/sessions/"+itoa(researchSess), nil, 200, &one)
	if !one.Session.SummaryStale {
		t.Fatalf("session = %+v", one.Session)
	}
	c.do("POST", "/api/sessions/"+itoa(researchSess)+"/cancel", nil, 202, nil)
	c.app.Wait()
	c.do("POST", "/api/sessions/"+itoa(researchSess)+"/summary", nil, 202, nil)
	c.app.Wait()
	c.do("GET", "/api/sessions/"+itoa(researchSess), nil, 200, &one)
	if one.Session.SummaryStale || one.Session.Summary != "SUMMARY of c-1" {
		t.Fatalf("session = %+v", one.Session)
	}
	c.do("POST", "/api/sessions/999/summary", nil, 404, nil)

	// MCP: get_session has the summary, list_sessions leaves it out, and
	// summarize_session rewrites it.
	text, isErr := c.tool("get_session", map[string]any{"session_id": researchSess})
	if isErr || !strings.Contains(text, `"summary":"SUMMARY of c-1"`) || !strings.Contains(text, `"summary_stale":false`) {
		t.Fatalf("get_session = %s", text)
	}
	text, isErr = c.tool("list_sessions", map[string]any{"task_id": research.ID})
	if isErr || strings.Contains(text, "SUMMARY") || !strings.Contains(text, `"summary_at":"`) {
		t.Fatalf("list_sessions = %s", text)
	}
	if text, isErr = c.tool("summarize_session", map[string]any{"session_id": researchSess}); isErr {
		t.Fatalf("summarize_session = %s", text)
	}
	c.app.Wait()

	// PATCH replaces the list; a bad list changes nothing, not even the other keys.
	c.do("PATCH", "/api/tasks/"+itoa(follow.ID), map[string]any{"title": "renamed", "context_from": []int64{follow.ID}}, 400, nil)
	c.do("GET", "/api/tasks/"+itoa(follow.ID), nil, 200, &page)
	if len(page.BuildsOn) != 1 {
		t.Fatalf("builds_on = %+v", page.BuildsOn)
	}
	c.do("PATCH", "/api/tasks/"+itoa(follow.ID), map[string]any{"title": "renamed"}, 200, &follow)
	if follow.Title != "renamed" || len(follow.ContextFrom) != 1 {
		t.Fatalf("leaving context_from out keeps it: %+v", follow)
	}
	c.do("PATCH", "/api/tasks/"+itoa(follow.ID), map[string]any{"context_from": []int64{}}, 200, &follow)
	if len(follow.ContextFrom) != 0 {
		t.Fatalf("follow = %+v", follow)
	}

	// MCP: an agent can create a follow-up of its own task, and get_task shows the links.
	text, isErr = c.tool("create_task", map[string]any{"title": "From MCP", "context_from": []int64{research.ID}})
	if isErr || !strings.Contains(text, `"context_from":[`+itoa(research.ID)+`]`) {
		t.Fatalf("create_task = %s", text)
	}
	text, isErr = c.tool("get_task", map[string]any{"task_id": research.ID})
	if isErr || !strings.Contains(text, `"used_by":[{`) || !strings.Contains(text, "From MCP") {
		t.Fatalf("get_task = %s", text)
	}
}

func TestSkills(t *testing.T) {
	c := newClient(t)
	var b board
	c.do("GET", "/api/board", nil, 200, &b)
	var skills []store.Skill
	c.do("GET", "/api/skills", nil, 200, &skills)
	if len(skills) != 1 || skills[0].Name != "Research summary" {
		t.Fatalf("seeded = %+v", skills)
	}

	c.do("POST", "/api/skills", map[string]any{"name": " ", "body": "x"}, 400, nil)
	c.do("POST", "/api/skills", map[string]any{"name": "x", "body": "{{.Nope"}, 400, nil)
	c.do("POST", "/api/skills", map[string]any{"name": "research SUMMARY", "body": "x"}, 400, nil)
	c.do("POST", "/api/skills", map[string]any{"name": "x", "body": "x", "model": "--dangerous"}, 400, nil)
	c.do("POST", "/api/skills", map[string]any{"name": "x", "body": "x", "model": "fa ble"}, 400, nil)
	c.do("POST", "/api/skills", map[string]any{"name": "x", "body": "x", "automations": []map[string]any{{"kind": "weird"}}}, 400, nil)
	c.do("POST", "/api/skills", map[string]any{"name": "x", "body": "x", "automations": []map[string]any{{"lane_id": 999}}}, 400, nil)
	c.do("POST", "/api/skills", map[string]any{"name": "x", "body": "x", "automations": []map[string]any{{"project_id": 999}}}, 400, nil)

	var sk store.Skill
	c.do("POST", "/api/skills", map[string]any{"name": "Digest", "body": "Digest {{.Task.Title}}", "artifact": true, "model": " fable ",
		"automations": []map[string]any{{"kind": "research", "project_id": b.ProjectID, "enabled": true}}}, 200, &sk)
	if sk.ID == 0 || len(sk.Automations) != 1 || *sk.Automations[0].ProjectID != b.ProjectID || sk.Model != "fable" {
		t.Fatalf("created = %+v", sk)
	}
	// A patch keeps what it leaves out.
	c.do("PATCH", "/api/skills/"+itoa(sk.ID), map[string]any{"description": "short"}, 200, &sk)
	if sk.Description != "short" || sk.Body != "Digest {{.Task.Title}}" || len(sk.Automations) != 1 || !sk.Artifact || sk.Model != "fable" {
		t.Fatalf("patched = %+v", sk)
	}

	// The automation runs the skill after a research task's turn; its reply becomes an artifact.
	var task store.Task
	c.do("POST", "/api/tasks", map[string]any{"title": "Dig", "kind": "research"}, 201, &task)
	var sess store.Session
	c.do("POST", "/api/tasks/"+itoa(task.ID)+"/sessions", map[string]any{"prompt": "dig"}, 201, &sess)
	c.app.Wait()
	var arts []store.Artifact
	c.do("GET", "/api/tasks/"+itoa(task.ID)+"/artifacts", nil, 200, &arts)
	if len(arts) != 1 || arts[0].Skill != "Digest" {
		t.Fatalf("artifacts = %+v", arts)
	}
	var detail struct {
		Artifacts []store.Artifact `json:"artifacts"`
	}
	c.do("GET", "/api/tasks/"+itoa(task.ID), nil, 200, &detail)
	if len(detail.Artifacts) != 1 {
		t.Fatalf("task detail = %+v", detail)
	}

	// By hand; [] clears the automations.
	c.do("PATCH", "/api/skills/"+itoa(sk.ID), map[string]any{"automations": []any{}}, 200, &sk)
	if len(sk.Automations) != 0 {
		t.Fatalf("automations = %+v", sk.Automations)
	}
	c.do("POST", "/api/sessions/"+itoa(sess.ID)+"/skill", map[string]any{"skill_id": 999}, 400, nil)
	c.do("POST", "/api/sessions/999/skill", map[string]any{"skill_id": sk.ID}, 404, nil)
	c.do("POST", "/api/sessions/"+itoa(sess.ID)+"/skill", map[string]any{"skill_id": sk.ID}, 202, nil)
	c.app.Wait()
	c.do("GET", "/api/tasks/"+itoa(task.ID)+"/artifacts", nil, 200, &arts)
	if len(arts) != 2 {
		t.Fatalf("artifacts = %+v", arts)
	}

	c.do("DELETE", "/api/artifacts/"+itoa(arts[0].ID), nil, 204, nil)
	c.do("DELETE", "/api/artifacts/"+itoa(arts[0].ID), nil, 404, nil)
	c.do("DELETE", "/api/skills/"+itoa(sk.ID), nil, 204, nil)
	c.do("PATCH", "/api/skills/"+itoa(sk.ID), map[string]any{"name": "x"}, 404, nil)
}

func TestNeedsInputOnBoardAndNotification(t *testing.T) {
	c := newClient(t)
	var task store.Task
	c.do("POST", "/api/tasks", map[string]any{"title": "t"}, 201, &task)
	var sess store.Session
	c.do("POST", "/api/tasks/"+itoa(task.ID)+"/sessions", map[string]any{"prompt": "ask"}, 201, &sess)
	c.app.Wait()

	var b struct {
		NeedsInput map[string]int `json:"needs_input"`
	}
	c.do("GET", "/api/board", nil, 200, &b)
	if b.NeedsInput[itoa(task.ID)] != 1 {
		t.Fatalf("board needs_input = %v", b.NeedsInput)
	}
	var notes []store.Notification
	c.do("GET", "/api/notifications?unread=1", nil, 200, &notes)
	if len(notes) != 1 || notes[0].SessionID == nil || *notes[0].SessionID != sess.ID {
		t.Fatalf("notifications = %+v", notes)
	}

	c.do("DELETE", "/api/sessions/"+itoa(sess.ID)+"/needs-input", nil, 204, nil)
	b.NeedsInput = nil
	c.do("GET", "/api/board", nil, 200, &b)
	if len(b.NeedsInput) != 0 {
		t.Fatalf("after dismissing: %v", b.NeedsInput)
	}
	c.do("DELETE", "/api/sessions/999/needs-input", nil, 404, nil)

	// The same through MCP.
	c.do("POST", "/api/sessions/"+itoa(sess.ID)+"/messages", map[string]any{"text": "ask"}, 202, nil)
	c.app.Wait()
	if text, _ := c.tool("list_sessions", map[string]any{"task_id": task.ID}); !strings.Contains(text, `"needs_input":"asked a question"`) {
		t.Fatalf("list_sessions = %s", text)
	}
	if text, isErr := c.tool("dismiss_needs_input", map[string]any{"session_id": sess.ID}); isErr {
		t.Fatalf("dismiss_needs_input: %s", text)
	}
	if got, _ := c.app.Store.Session(context.Background(), sess.ID); got.NeedsInput != "" {
		t.Fatalf("session = %+v", got)
	}
}

func TestAgentStatusOnBoardAndTask(t *testing.T) {
	c := newClient(t)
	var b board
	c.do("GET", "/api/board", nil, 200, &b)
	lane := map[string]int64{}
	for _, l := range b.Lanes {
		lane[l.Name] = l.ID
	}
	var task store.Task
	c.do("POST", "/api/tasks", map[string]any{"title": "T", "lane_id": lane["In progress"]}, 201, &task)
	path := "/api/tasks/" + itoa(task.ID)
	status := func() *store.AgentStatus {
		t.Helper()
		var bs struct {
			AgentStatus map[string]store.AgentStatus `json:"agent_status"`
		}
		c.do("GET", "/api/board", nil, 200, &bs)
		var ts struct {
			AgentStatus *store.AgentStatus `json:"agent_status"`
		}
		c.do("GET", path, nil, 200, &ts)
		st, ok := bs.AgentStatus[itoa(task.ID)]
		if ok != (ts.AgentStatus != nil) || ok && st != *ts.AgentStatus {
			t.Fatalf("board %+v and task %+v disagree", bs.AgentStatus, ts.AgentStatus)
		}
		return ts.AgentStatus
	}
	if st := status(); st != nil {
		t.Fatalf("no sessions yet: %+v", st)
	}

	// A turn that moves the task to its done lane is done.
	var sess store.Session
	c.do("POST", path+"/sessions", map[string]any{"prompt": "p", "done_lane_id": lane["Review"]}, 201, &sess)
	c.app.Wait()
	if st := status(); st == nil || st.Status != store.AgentDone || st.SessionID != sess.ID {
		t.Fatalf("after the turn: %+v", st)
	}
	// Moving the task by hand: seen.
	c.do("POST", path+"/move", map[string]any{"lane_id": lane["Done"], "index": 0}, 200, nil)
	if st := status(); st != nil {
		t.Fatalf("after moving: %+v", st)
	}

	// Another turn without a done lane only finished; dismissing it: seen.
	c.do("PATCH", "/api/sessions/"+itoa(sess.ID), map[string]any{"done_lane_id": nil}, 200, nil)
	c.do("POST", "/api/sessions/"+itoa(sess.ID)+"/messages", map[string]any{"text": "more"}, 202, nil)
	c.app.Wait()
	if st := status(); st == nil || st.Status != store.AgentFinished {
		t.Fatalf("after the second turn: %+v", st)
	}
	if text, _ := c.tool("get_task", map[string]any{"task_id": task.ID}); !strings.Contains(text, `"agent_status":{"status":"finished"`) {
		t.Fatalf("get_task = %s", text)
	}
	if text, _ := c.tool("get_board", map[string]any{}); !strings.Contains(text, `"agent_status":{"`+itoa(task.ID)+`":{"status":"finished"`) {
		t.Fatalf("get_board = %s", text)
	}
	c.do("DELETE", "/api/sessions/"+itoa(sess.ID)+"/needs-input", nil, 204, nil)
	if st := status(); st != nil {
		t.Fatalf("after dismissing: %+v", st)
	}

	// A question needs the user; answering it runs a new turn.
	c.do("POST", "/api/sessions/"+itoa(sess.ID)+"/messages", map[string]any{"text": "ask"}, 202, nil)
	c.app.Wait()
	if st := status(); st == nil || st.Status != store.AgentNeedsYou || st.Reason != "asked a question" {
		t.Fatalf("after asking: %+v", st)
	}
}
