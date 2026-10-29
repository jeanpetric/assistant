package app

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jeanpetric/assistant/internal/agent"
	"github.com/jeanpetric/assistant/internal/store"
)

// fakeRunner records requests and replies with a canned transcript.
type fakeRunner struct {
	mu     sync.Mutex
	reqs   []agent.Request
	err    error
	failOn string            // if set, prompts starting with it fail
	block  chan struct{}     // if set, Run waits for close or ctx cancel
	limits *agent.RateLimits // if set, emitted as a rate_limit event
	// If set, the final reply and the error of a request; otherwise "ok" and err.
	result func(agent.Request) string
	fail   func(agent.Request) error
	tools  []string      // if set, emitted as tool_use events
	events []agent.Event // if set, emitted before the result
	denied []string      // if set, the result's permission denials
	// If set, the session's model, the context sizes after each message and
	// the result's context windows.
	model   string
	usage   []int
	windows map[string]int
}

func (f *fakeRunner) Run(ctx context.Context, req agent.Request, emit func(agent.Event)) error {
	f.mu.Lock()
	f.reqs = append(f.reqs, req)
	f.mu.Unlock()
	f.mu.Lock()
	model, usage, windows := f.model, f.usage, f.windows
	f.mu.Unlock()
	emit(agent.Event{Role: "system", Kind: "init", ClaudeSessionID: "claude-1", Model: model})
	for _, n := range usage {
		emit(agent.Event{Role: "system", Kind: "usage", ContextTokens: n})
	}
	f.mu.Lock()
	limits := f.limits
	f.mu.Unlock()
	if limits != nil {
		emit(agent.Event{Role: "system", Kind: "rate_limit", Limits: limits})
	}
	if f.block != nil {
		select {
		case <-f.block:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	f.mu.Lock()
	result, fail, failOn, err, tools, denied, events := f.result, f.fail, f.failOn, f.err, f.tools, f.denied, f.events
	f.mu.Unlock()
	if fail != nil {
		if err := fail(req); err != nil {
			return err
		}
	}
	reply := "ok"
	if result != nil {
		reply = result(req)
	}
	emit(agent.Event{Role: "assistant", Kind: "text", Content: "echo: " + req.Prompt})
	for _, t := range tools {
		emit(agent.Event{Role: "assistant", Kind: "tool_use", Content: t})
	}
	for _, ev := range events {
		emit(ev)
	}
	emit(agent.Event{Role: "system", Kind: "result", Content: reply, CostUSD: 0.5, Denied: denied, ContextWindows: windows})
	if failOn != "" && strings.HasPrefix(req.Prompt, failOn) {
		return errors.New("failed: " + failOn)
	}
	return err
}

func (f *fakeRunner) requests() []agent.Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]agent.Request(nil), f.reqs...)
}

func newApp(t *testing.T) (*App, *fakeRunner) {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	r := &fakeRunner{}
	return New(s, r, "/default/dir", "bypassPermissions"), r
}

func lanesByName(t *testing.T, a *App) map[string]int64 {
	t.Helper()
	lanes, err := a.Store.Lanes(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	m := map[string]int64{}
	for _, l := range lanes {
		m[l.Name] = l.ID
	}
	return m
}

func TestSessionLifecycle(t *testing.T) {
	a, r := newApp(t)
	ctx := context.Background()
	task, _ := a.Store.CreateTask(ctx, store.Task{Title: "Investigate", Workdir: "/repo"})

	sess, err := a.StartSession(ctx, StartOpts{TaskID: task.ID, Prompt: "look around"})
	if err != nil {
		t.Fatal(err)
	}
	a.Wait()

	got, _ := a.Store.Session(ctx, sess.ID)
	if got.Status != store.StatusIdle || got.ClaudeSessionID != "claude-1" || got.CostUSD != 0.5 {
		t.Fatalf("unexpected session %+v", got)
	}
	if got.Mode != "bypassPermissions" || got.Workdir != "/repo" || got.Title != "look around" {
		t.Fatalf("defaults not applied: %+v", got)
	}

	if _, err := a.SendMessage(ctx, sess.ID, "now fix it"); err != nil {
		t.Fatal(err)
	}
	a.Wait()
	reqs := r.requests()
	if len(reqs) != 2 || reqs[1].ResumeID != "claude-1" || reqs[1].Prompt != "now fix it" {
		t.Fatalf("follow-up should resume: %+v", reqs)
	}

	msgs, _ := a.Store.Messages(ctx, sess.ID, 0)
	var transcript []string
	for _, m := range msgs {
		transcript = append(transcript, m.Role+":"+m.Content)
	}
	want := "user:look around|assistant:echo: look around|system:ok|user:now fix it|assistant:echo: now fix it|system:ok"
	if strings.Join(transcript, "|") != want {
		t.Fatalf("transcript = %s", strings.Join(transcript, "|"))
	}
	got, _ = a.Store.Session(ctx, sess.ID)
	if got.CostUSD != 1.0 || got.LastMessage != "ok" {
		t.Fatalf("after follow-up: %+v", got)
	}
}

func TestSessionErrorAndCancel(t *testing.T) {
	a, r := newApp(t)
	ctx := context.Background()
	task, _ := a.Store.CreateTask(ctx, store.Task{Title: "T"})

	r.err = errors.New("cli exploded")
	sess, _ := a.StartSession(ctx, StartOpts{TaskID: task.ID, Prompt: "p"})
	a.Wait()
	got, _ := a.Store.Session(ctx, sess.ID)
	if got.Status != store.StatusError || got.Error != "cli exploded" {
		t.Fatalf("got %+v", got)
	}

	r.err = nil
	r.block = make(chan struct{})
	sess, _ = a.StartSession(ctx, StartOpts{TaskID: task.ID, Prompt: "long"})
	if queued, err := a.SendMessage(ctx, sess.ID, "again"); err != nil || !queued {
		t.Fatalf("a message to a running session should be queued: %v %v", queued, err)
	}
	a.Cancel(sess.ID)
	a.Wait()
	got, _ = a.Store.Session(ctx, sess.ID)
	if got.Status != store.StatusCancelled {
		t.Fatalf("expected cancelled, got %+v", got)
	}
	// Stopping the turn does not send the queued message; it waits.
	if len(got.Queued) != 1 || got.Queued[0].Content != "again" || len(r.requests()) != 2 {
		t.Fatalf("queue after cancel = %+v, %d requests", got.Queued, len(r.requests()))
	}
}

// A message sent while the agent works waits in the queue and starts the next
// turn once the running one ends, before the done-lane move; several go in order.
func TestQueuedMessages(t *testing.T) {
	a, r := newApp(t)
	ctx := context.Background()
	lanes := lanesByName(t, a)
	task, _ := a.Store.CreateTask(ctx, store.Task{Title: "T", LaneID: lanes["In progress"]})
	done := lanes["Done"]

	r.block = make(chan struct{})
	sess, err := a.StartSession(ctx, StartOpts{TaskID: task.ID, Prompt: "first", DoneLaneID: &done})
	if err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"second", "dropped", "third"} {
		if queued, err := a.SendMessage(ctx, sess.ID, text); err != nil || !queued {
			t.Fatalf("send %q: queued %v, %v", text, queued, err)
		}
	}
	got, _ := a.Store.Session(ctx, sess.ID)
	if len(got.Queued) != 3 || got.Queued[0].Content != "second" || got.Queued[2].Content != "third" {
		t.Fatalf("queue = %+v", got.Queued)
	}
	if err := a.Store.DeleteQueuedMessage(ctx, got.Queued[1].ID); err != nil {
		t.Fatal(err)
	}
	if err := a.Store.DeleteQueuedMessage(ctx, got.Queued[1].ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("deleting it twice: %v", err)
	}
	if tk, _ := a.Store.Task(ctx, task.ID); tk.LaneID != lanes["In progress"] {
		t.Fatalf("task moved while the agent worked: lane %d", tk.LaneID)
	}
	close(r.block)
	a.Wait() // a turn starts the next before it counts as finished
	reqs := r.requests()
	var prompts []string
	for _, q := range reqs {
		prompts = append(prompts, q.Prompt)
	}
	if strings.Join(prompts, "|") != "first|second|third" || reqs[1].ResumeID != "claude-1" || reqs[2].ResumeID != "claude-1" {
		t.Fatalf("requests = %+v", reqs)
	}
	msgs, _ := a.Store.Messages(ctx, sess.ID, 0)
	var users []string
	for _, m := range msgs {
		if m.Role == "user" {
			users = append(users, m.Content)
		}
	}
	if strings.Join(users, "|") != "first|second|third" {
		t.Fatalf("user messages = %v", users)
	}
	if tk, _ := a.Store.Task(ctx, task.ID); tk.LaneID != done {
		t.Fatalf("task should move to done after the last queued turn, lane %d", tk.LaneID)
	}

	// An idle session takes a message at once.
	if queued, err := a.SendMessage(ctx, sess.ID, "now"); err != nil || queued {
		t.Fatalf("idle send: queued %v, %v", queued, err)
	}
	a.Wait()
	if n := len(r.requests()); n != 4 {
		t.Fatalf("%d requests", n)
	}
}

// An agent that asks a question the user already answered in the queue does
// not wait for input.
func TestQueuedAnswerSkipsNeedsInput(t *testing.T) {
	a, r := newApp(t)
	ctx := context.Background()
	task, _ := a.Store.CreateTask(ctx, store.Task{Title: "T"})
	r.result = func(req agent.Request) string {
		if req.Prompt == "ask" {
			return "Which branch?"
		}
		return "ok"
	}
	r.block = make(chan struct{})
	sess, _ := a.StartSession(ctx, StartOpts{TaskID: task.ID, Prompt: "ask"})
	a.SendMessage(ctx, sess.ID, "main")
	close(r.block)
	a.Wait()
	got, _ := a.Store.Session(ctx, sess.ID)
	if got.NeedsInput != "" || len(r.requests()) != 2 {
		t.Fatalf("session %+v, %d requests", got, len(r.requests()))
	}
	if ns, _ := a.Store.Notifications(ctx, false); slices.ContainsFunc(ns, func(n store.Notification) bool {
		return strings.Contains(n.Message, "needs your input")
	}) {
		t.Fatalf("notified: %+v", ns)
	}
}

// A failed turn still hands the session to the queued message.
func TestQueuedMessageAfterError(t *testing.T) {
	a, r := newApp(t)
	ctx := context.Background()
	task, _ := a.Store.CreateTask(ctx, store.Task{Title: "T"})
	r.failOn = "boom"
	r.block = make(chan struct{})
	sess, _ := a.StartSession(ctx, StartOpts{TaskID: task.ID, Prompt: "boom"})
	a.SendMessage(ctx, sess.ID, "retry")
	close(r.block)
	a.Wait()
	got, _ := a.Store.Session(ctx, sess.ID)
	if got.Status != store.StatusIdle || len(got.Queued) != 0 || r.requests()[1].Prompt != "retry" {
		t.Fatalf("session %+v, requests %+v", got, r.requests())
	}
}

func TestStartSessionValidation(t *testing.T) {
	a, _ := newApp(t)
	ctx := context.Background()
	if _, err := a.StartSession(ctx, StartOpts{TaskID: 999, Prompt: "x"}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("expected not found, got %v", err)
	}
	task, _ := a.Store.CreateTask(ctx, store.Task{Title: "T"})
	if _, err := a.StartSession(ctx, StartOpts{TaskID: task.ID, Prompt: "  "}); err == nil {
		t.Fatal("expected error for empty prompt")
	}
	if got, _ := a.StartSession(ctx, StartOpts{TaskID: task.ID, Prompt: "x"}); got.Workdir != "/default/dir" {
		t.Fatalf("expected default workdir, got %q", got.Workdir)
	}

	// The project's workdir comes between the task's and the app default.
	p, _ := a.Store.CreateProject(ctx, store.Project{Name: "P", Workdir: "/project/dir"})
	inProject, _ := a.Store.CreateTask(ctx, store.Task{Title: "In P", ProjectID: p.ID})
	if got, _ := a.StartSession(ctx, StartOpts{TaskID: inProject.ID, Prompt: "x"}); got.Workdir != "/project/dir" {
		t.Fatalf("expected project workdir, got %q", got.Workdir)
	}
	own, _ := a.Store.CreateTask(ctx, store.Task{Title: "Own", ProjectID: p.ID, Workdir: "/task/dir"})
	if got, _ := a.StartSession(ctx, StartOpts{TaskID: own.ID, Prompt: "x"}); got.Workdir != "/task/dir" {
		t.Fatalf("expected task workdir, got %q", got.Workdir)
	}
	if got, _ := a.StartSession(ctx, StartOpts{TaskID: inProject.ID, Prompt: "x", Workdir: "/picked"}); got.Workdir != "/picked" {
		t.Fatalf("expected picked workdir, got %q", got.Workdir)
	}
	a.Wait()
}

func TestRateLimits(t *testing.T) {
	a, r := newApp(t)
	ctx := context.Background()
	now := time.Now().Truncate(time.Millisecond)

	week := &agent.LimitWindow{UsedPercent: 17, ResetsAt: now.Add(72 * time.Hour)}
	r.limits = &agent.RateLimits{FiveHour: &agent.LimitWindow{UsedPercent: 10, ResetsAt: now.Add(time.Hour)}, SevenDay: week}
	sess, err := a.StartSession(ctx, StartOpts{Prompt: "hi"})
	if err != nil {
		t.Fatal(err)
	}
	a.Wait()
	// A later report of one window keeps the other.
	r.mu.Lock()
	r.limits = &agent.RateLimits{FiveHour: &agent.LimitWindow{UsedPercent: 30, ResetsAt: now.Add(time.Hour)}}
	r.mu.Unlock()
	if _, err := a.SendMessage(ctx, sess.ID, "more"); err != nil {
		t.Fatal(err)
	}
	a.Wait()
	if l := a.Limits(sess.ID); l.FiveHour == nil || l.FiveHour.UsedPercent != 30 || l.SevenDay == nil ||
		l.SevenDay.UsedPercent != 17 || !l.SevenDay.ResetsAt.Equal(week.ResetsAt) {
		t.Fatalf("limits = %+v", l)
	}
	msgs, _ := a.Store.Messages(ctx, sess.ID, 0)
	for _, m := range msgs {
		if m.Kind == "rate_limit" {
			t.Fatalf("rate limits should not land in the transcript: %+v", m)
		}
	}

	// Limits belong to the session that reported them.
	r.mu.Lock()
	r.limits = nil
	r.mu.Unlock()
	other, _ := a.StartSession(ctx, StartOpts{Prompt: "other"})
	a.Wait()
	if l := a.Limits(other.ID); l.FiveHour != nil || l.SevenDay != nil {
		t.Fatalf("other session limits = %+v", l)
	}
}

func TestSessionContext(t *testing.T) {
	a, r := newApp(t)
	ctx := context.Background()
	// The session's own model's window counts, not its subagents'.
	r.model, r.usage, r.windows = "main", []int{1000, 5000}, map[string]int{"main": 200000, "sub": 1000000}
	sess, err := a.StartSession(ctx, StartOpts{Prompt: "hi"})
	if err != nil {
		t.Fatal(err)
	}
	a.Wait()
	got, _ := a.Store.Session(ctx, sess.ID)
	if got.ContextTokens != 5000 || got.ContextWindow != 200000 {
		t.Fatalf("session = %+v", got)
	}
	msgs, _ := a.Store.Messages(ctx, sess.ID, 0)
	for _, m := range msgs {
		if m.Kind == "usage" {
			t.Fatalf("usage should not land in the transcript: %+v", m)
		}
	}

	// A turn whose windows don't name the model keeps the known window.
	r.mu.Lock()
	r.usage, r.windows = []int{8000}, map[string]int{"sub": 1000000, "other": 500000}
	r.mu.Unlock()
	a.SendMessage(ctx, sess.ID, "more")
	a.Wait()
	if got, _ := a.Store.Session(ctx, sess.ID); got.ContextTokens != 8000 || got.ContextWindow != 200000 {
		t.Fatalf("after second turn = %+v", got)
	}

	// With only one model in the turn, that is the session's.
	r.mu.Lock()
	r.model, r.usage, r.windows = "", []int{100}, map[string]int{"x": 300000}
	r.mu.Unlock()
	other, _ := a.StartSession(ctx, StartOpts{Prompt: "other"})
	a.Wait()
	if got, _ := a.Store.Session(ctx, other.ID); got.ContextTokens != 100 || got.ContextWindow != 300000 {
		t.Fatalf("other = %+v", got)
	}
}

func TestStandaloneSession(t *testing.T) {
	a, r := newApp(t)
	ctx := context.Background()
	sess, err := a.StartSession(ctx, StartOpts{Prompt: "hello", Model: "sonnet", Workdir: "/elsewhere"})
	if err != nil {
		t.Fatal(err)
	}
	a.Wait()
	if _, err := a.SendMessage(ctx, sess.ID, "more"); err != nil {
		t.Fatal(err)
	}
	a.Wait()
	got, _ := a.Store.Session(ctx, sess.ID)
	if got.TaskID != 0 || got.Status != store.StatusIdle || got.Model != "sonnet" || got.Workdir != "/elsewhere" {
		t.Fatalf("session = %+v", got)
	}
	reqs := r.requests()
	if len(reqs) != 2 || reqs[0].Model != "sonnet" || reqs[1].Model != "sonnet" || reqs[1].Workdir != "/elsewhere" {
		t.Fatalf("model and workdir should carry over to resumed turns: %+v", reqs)
	}
	if got, _ := a.StartSession(ctx, StartOpts{Prompt: "x"}); got.Workdir != "/default/dir" || got.Model != "" {
		t.Fatalf("defaults: %+v", got)
	}
	p, _ := a.Store.CreateProject(ctx, store.Project{Name: "Web", Workdir: "/project/dir"})
	if got, _ := a.StartSession(ctx, StartOpts{Prompt: "x", ProjectID: p.ID}); got.Workdir != "/project/dir" || got.TaskID != 0 || got.ProjectID != p.ID {
		t.Fatalf("a standalone session runs in its project's directory: %+v", got)
	}
	a.Wait()
}

// A session started in a new task that builds on others (⌥P on an open task):
// the task links them and inherits their memories, and the first prompt starts
// with both; without links the prompt is sent as typed.
func TestSessionInNewTaskBuildsOn(t *testing.T) {
	a, r := newApp(t)
	ctx := context.Background()
	mem, _ := a.Store.CreateMemory(ctx, store.Memory{Name: "API", Body: "- Pages hold 200 items"})
	src, _ := a.Store.CreateTask(ctx, store.Task{Title: "Research caching", Memories: []int64{mem.ID}})
	sess, err := a.StartSessionInNewTask(ctx, StartOpts{Prompt: "build it", ContextFrom: []int64{src.ID}})
	if err != nil {
		t.Fatal(err)
	}
	a.Wait()
	task, _ := a.Store.Task(ctx, sess.TaskID)
	if !slices.Equal(task.ContextFrom, []int64{src.ID}) || !slices.Equal(task.Memories, []int64{mem.ID}) || task.Title != "build it" {
		t.Fatalf("task = %+v", task)
	}
	prompt := r.requests()[0].Prompt
	for _, want := range []string{MemoryIntro, SharedIntro, "## Task #" + itoa(src.ID) + ": Research caching"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt lacks %q:\n%s", want, prompt)
		}
	}
	if !strings.HasSuffix(prompt, "\n\n---\n\nbuild it") || strings.Contains(prompt, "Task #"+itoa(task.ID)) {
		t.Fatalf("the prompt ends with what was typed, without the new task's own context:\n%s", prompt)
	}

	plain, _ := a.StartSessionInNewTask(ctx, StartOpts{Prompt: "just this"})
	a.Wait()
	if reqs := r.requests(); reqs[len(reqs)-1].Prompt != "just this" {
		t.Fatalf("plain prompt = %q", reqs[len(reqs)-1].Prompt)
	}
	if task, _ := a.Store.Task(ctx, plain.TaskID); len(task.ContextFrom) != 0 {
		t.Fatalf("plain task = %+v", task)
	}
	if _, err := a.StartSessionInNewTask(ctx, StartOpts{Prompt: "x", ContextFrom: []int64{999}}); !errors.Is(err, store.ErrBadContext) {
		t.Fatalf("unknown source: %v", err)
	}
}

// A new session's task starts in the lane set for new sessions (In progress
// by default), or the first lane when none is set.
func TestSessionInNewTaskLane(t *testing.T) {
	a, _ := newApp(t)
	ctx := context.Background()
	lanes := lanesByName(t, a)
	sess, err := a.StartSessionInNewTask(ctx, StartOpts{Prompt: "go"})
	if err != nil {
		t.Fatal(err)
	}
	a.Wait()
	if task, _ := a.Store.Task(ctx, sess.TaskID); task.LaneID != lanes["In progress"] {
		t.Fatalf("lane = %d, want In progress (%d)", task.LaneID, lanes["In progress"])
	}
	set, _ := a.Store.Settings(ctx)
	set.SessionLaneID = nil
	if err := a.Store.SaveSettings(ctx, set); err != nil {
		t.Fatal(err)
	}
	sess, _ = a.StartSessionInNewTask(ctx, StartOpts{Prompt: "go"})
	a.Wait()
	if task, _ := a.Store.Task(ctx, sess.TaskID); task.LaneID != lanes["Backlog"] {
		t.Fatalf("lane = %d, want the first lane (%d)", task.LaneID, lanes["Backlog"])
	}
}

// A new session's task never moves into its lane, so the session takes the
// done lane of a rule into that lane, preferring the project's rule.
func TestSessionInNewTaskTakesRuleDoneLane(t *testing.T) {
	a, _ := newApp(t)
	ctx := context.Background()
	lanes := lanesByName(t, a)
	todo, wip, review, done := lanes["Todo"], lanes["In progress"], lanes["Review"], lanes["Done"]
	p, _ := a.Store.CreateProject(ctx, store.Project{Name: "P"})

	sess, err := a.StartSessionInNewTask(ctx, StartOpts{Prompt: "no rule"})
	if err != nil {
		t.Fatal(err)
	}
	a.Wait()
	if sess.DoneLaneID != nil {
		t.Fatalf("done lane = %d without a rule", *sess.DoneLaneID)
	}

	a.Store.SaveRule(ctx, store.Rule{Name: "Disabled", ToLaneID: wip, DoneLaneID: &done, Enabled: false, Prompt: "x"})
	a.Store.SaveRule(ctx, store.Rule{Name: "Start", FromLaneID: &todo, ToLaneID: wip, DoneLaneID: &review, Enabled: true, Prompt: "x"})
	a.Store.SaveRule(ctx, store.Rule{Name: "P only", ProjectID: &p.ID, ToLaneID: wip, DoneLaneID: &done, Enabled: true, Prompt: "x"})

	sess, _ = a.StartSessionInNewTask(ctx, StartOpts{Prompt: "go"})
	a.Wait()
	if sess.DoneLaneID == nil || *sess.DoneLaneID != review {
		t.Fatalf("done lane = %v, want Review (%d)", sess.DoneLaneID, review)
	}
	if task, _ := a.Store.Task(ctx, sess.TaskID); task.LaneID != review {
		t.Fatalf("task lane = %d, want Review after the turn", task.LaneID)
	}
	sess, _ = a.StartSessionInNewTask(ctx, StartOpts{Prompt: "go", ProjectID: p.ID})
	a.Wait()
	if sess.DoneLaneID == nil || *sess.DoneLaneID != done {
		t.Fatalf("project done lane = %v, want Done (%d)", sess.DoneLaneID, done)
	}
	sess, _ = a.StartSessionInNewTask(ctx, StartOpts{Prompt: "go", DoneLaneID: &todo})
	a.Wait()
	if sess.DoneLaneID == nil || *sess.DoneLaneID != todo {
		t.Fatalf("explicit done lane = %v, want Todo (%d)", sess.DoneLaneID, todo)
	}
}

func TestMoveTaskFiresRules(t *testing.T) {
	a, r := newApp(t)
	ctx := context.Background()
	lanes := lanesByName(t, a)
	todo, wip, done := lanes["Todo"], lanes["In progress"], lanes["Done"]

	task, _ := a.Store.CreateTask(ctx, store.Task{Title: "Fix bug", LaneID: todo,
		Fields: map[string]string{"pull_request": "https://github.com/x/y/pull/1"}})
	a.Store.SaveRule(ctx, store.Rule{Name: "Start coding", FromLaneID: &todo, ToLaneID: wip, Enabled: true,
		Prompt: "/p-dev:code {{.Task.Title}} {{.Fields.pull_request}}", Mode: "acceptEdits", Workdir: "/rule/dir"})
	a.Store.SaveRule(ctx, store.Rule{Name: "Any to done", ToLaneID: done, Enabled: true, Prompt: "wrap up"})
	a.Store.SaveRule(ctx, store.Rule{Name: "Disabled", ToLaneID: wip, Enabled: false, Prompt: "never"})

	started, err := a.MoveTask(ctx, task.ID, wip, 0)
	if err != nil {
		t.Fatal(err)
	}
	a.Wait()
	if len(started) != 1 || started[0].Trigger != "rule:Start coding" || started[0].Workdir != "/rule/dir" || started[0].Mode != "acceptEdits" {
		t.Fatalf("started = %+v", started)
	}
	if p := r.requests()[0].Prompt; p != "/p-dev:code Fix bug https://github.com/x/y/pull/1" {
		t.Fatalf("prompt = %q", p)
	}

	// Reordering within the lane doesn't fire anything.
	if started, _ := a.MoveTask(ctx, task.ID, wip, 0); len(started) != 0 {
		t.Fatalf("same-lane move fired %d rules", len(started))
	}
	// Moving back to Todo has no rule; moving wip -> done matches the any-lane rule.
	if started, _ := a.MoveTask(ctx, task.ID, todo, 0); len(started) != 0 {
		t.Fatal("unexpected rule for wip -> todo")
	}
	if started, _ := a.MoveTask(ctx, task.ID, done, 0); len(started) != 1 {
		t.Fatal("expected any-lane rule to fire")
	}
	a.Wait()
}

func TestMoveTaskFiresRulesForItsKind(t *testing.T) {
	a, _ := newApp(t)
	ctx := context.Background()
	lanes := lanesByName(t, a)
	todo, wip := lanes["Todo"], lanes["In progress"]
	a.Store.SaveRule(ctx, store.Rule{Name: "Code it", Kind: "coding", FromLaneID: &todo, ToLaneID: wip, Enabled: true, Prompt: "/p-dev:code"})

	research, _ := a.Store.CreateTask(ctx, store.Task{Title: "Look into it", Kind: "research", LaneID: todo})
	if started, _ := a.MoveTask(ctx, research.ID, wip, 0); len(started) != 0 {
		t.Fatalf("coding rule fired for a research task: %+v", started)
	}
	coding, _ := a.Store.CreateTask(ctx, store.Task{Title: "Build it", Kind: "coding", LaneID: todo})
	if started, _ := a.MoveTask(ctx, coding.ID, wip, 0); len(started) != 1 || started[0].Trigger != "rule:Code it" {
		t.Fatalf("started = %+v", started)
	}
	a.Wait()
}

func TestDoneLaneMovesTaskAfterSuccessfulTurn(t *testing.T) {
	a, r := newApp(t)
	ctx := context.Background()
	lanes := lanesByName(t, a)
	todo, wip, review := lanes["Todo"], lanes["In progress"], lanes["Review"]
	task, _ := a.Store.CreateTask(ctx, store.Task{Title: "Ship it", LaneID: todo})
	a.Store.SaveRule(ctx, store.Rule{Name: "Code", ToLaneID: wip, Enabled: true, Prompt: "code", DoneLaneID: &review})
	a.Store.SaveRule(ctx, store.Rule{Name: "Review", ToLaneID: review, Enabled: true, Prompt: "review"})

	started, err := a.MoveTask(ctx, task.ID, wip, 0)
	if err != nil || len(started) != 1 || started[0].DoneLaneID == nil || *started[0].DoneLaneID != review {
		t.Fatalf("started = %+v, %v", started, err)
	}
	a.Wait()
	if got, _ := a.Store.Task(ctx, task.ID); got.LaneID != review {
		t.Fatalf("task in lane %d, want review %d", got.LaneID, review)
	}
	// Arriving in Review fires that lane's rule, which has no done lane: the chain stops there.
	reqs := r.requests()
	if len(reqs) != 2 || reqs[1].Prompt != "review" {
		t.Fatalf("requests = %+v", reqs)
	}
	n, _ := a.Store.Notifications(ctx, true)
	if len(n) != 1 || !strings.Contains(n[0].Message, "moved to Review") {
		t.Fatalf("notifications = %+v", n)
	}
}

func TestDoneLaneSkipped(t *testing.T) {
	ctx := context.Background()

	t.Run("no done lane", func(t *testing.T) {
		a, _ := newApp(t)
		lanes := lanesByName(t, a)
		task, _ := a.Store.CreateTask(ctx, store.Task{Title: "T", LaneID: lanes["In progress"]})
		a.StartSession(ctx, StartOpts{TaskID: task.ID, Prompt: "p"})
		a.Wait()
		if got, _ := a.Store.Task(ctx, task.ID); got.LaneID != lanes["In progress"] {
			t.Fatal("task moved without a done lane")
		}
	})

	t.Run("failed turn", func(t *testing.T) {
		a, r := newApp(t)
		lanes := lanesByName(t, a)
		review := lanes["Review"]
		task, _ := a.Store.CreateTask(ctx, store.Task{Title: "T", LaneID: lanes["In progress"]})
		r.err = errors.New("boom")
		a.StartSession(ctx, StartOpts{TaskID: task.ID, Prompt: "p", DoneLaneID: &review})
		a.Wait()
		if got, _ := a.Store.Task(ctx, task.ID); got.LaneID != lanes["In progress"] {
			t.Fatal("failed turn must not move the task")
		}
	})

	t.Run("moved by hand during the turn", func(t *testing.T) {
		a, r := newApp(t)
		lanes := lanesByName(t, a)
		review := lanes["Review"]
		task, _ := a.Store.CreateTask(ctx, store.Task{Title: "T", LaneID: lanes["In progress"]})
		r.block = make(chan struct{})
		a.StartSession(ctx, StartOpts{TaskID: task.ID, Prompt: "p", DoneLaneID: &review})
		a.Store.MoveTask(ctx, task.ID, lanes["Done"], 0)
		close(r.block)
		a.Wait()
		if got, _ := a.Store.Task(ctx, task.ID); got.LaneID != lanes["Done"] {
			t.Fatal("a manual move must win over the done lane")
		}
	})

	t.Run("done lane switched off mid-turn", func(t *testing.T) {
		a, r := newApp(t)
		lanes := lanesByName(t, a)
		review := lanes["Review"]
		task, _ := a.Store.CreateTask(ctx, store.Task{Title: "T", LaneID: lanes["In progress"]})
		r.block = make(chan struct{})
		sess, _ := a.StartSession(ctx, StartOpts{TaskID: task.ID, Prompt: "p", DoneLaneID: &review})
		a.Store.SetSessionDoneLane(ctx, sess.ID, nil)
		close(r.block)
		a.Wait()
		if got, _ := a.Store.Task(ctx, task.ID); got.LaneID != lanes["In progress"] {
			t.Fatal("cleared done lane must not move the task")
		}
	})

	t.Run("standalone session", func(t *testing.T) {
		a, _ := newApp(t)
		review := lanesByName(t, a)["Review"]
		sess, err := a.StartSession(ctx, StartOpts{Prompt: "p", DoneLaneID: &review})
		a.Wait()
		if err != nil || sess.DoneLaneID != nil {
			t.Fatalf("standalone session kept a done lane: %+v, %v", sess, err)
		}
	})
}

func TestBadRuleTemplateNotifies(t *testing.T) {
	a, _ := newApp(t)
	ctx := context.Background()
	lanes := lanesByName(t, a)
	task, _ := a.Store.CreateTask(ctx, store.Task{Title: "T", LaneID: lanes["Todo"]})
	a.Store.SaveRule(ctx, store.Rule{Name: "Broken", ToLaneID: lanes["Review"], Enabled: true, Prompt: "{{.Nope"})
	if _, err := a.MoveTask(ctx, task.ID, lanes["Review"], 0); err != nil {
		t.Fatal(err)
	}
	n, _ := a.Store.Notifications(ctx, true)
	if len(n) != 1 || !strings.Contains(n[0].Message, "Broken") {
		t.Fatalf("notifications = %+v", n)
	}
}

func TestSchedules(t *testing.T) {
	a, r := newApp(t)
	ctx := context.Background()
	now := time.Date(2026, 10, 20, 8, 59, 30, 0, time.Local)
	a.Now = func() time.Time { return now }
	task, _ := a.Store.CreateTask(ctx, store.Task{Title: "Daily digest"})
	review := lanesByName(t, a)["Review"]

	if _, err := a.SaveSchedule(ctx, store.Schedule{TaskID: task.ID, Cron: "bad", Action: "agent"}); err == nil {
		t.Fatal("expected cron error")
	}
	if _, err := a.SaveSchedule(ctx, store.Schedule{TaskID: task.ID, Cron: "* * * * *", Action: "dance"}); !errors.Is(err, ErrBadAction) {
		t.Fatal("expected bad action")
	}
	agentSched, err := a.SaveSchedule(ctx, store.Schedule{TaskID: task.ID, Cron: "0 9 * * *", Action: "agent",
		Prompt: "Summarise: {{.Context}}", Enabled: true, DoneLaneID: &review})
	if err != nil {
		t.Fatal(err)
	}
	if want := time.Date(2026, 10, 20, 9, 0, 0, 0, time.Local); !agentSched.NextRunAt.Equal(want) {
		t.Fatalf("next = %v", agentSched.NextRunAt)
	}
	a.SaveSchedule(ctx, store.Schedule{TaskID: task.ID, Cron: "0 9 * * *", Action: "remind", Enabled: true})

	// Not due yet.
	a.RunDueSchedules(ctx)
	if len(r.requests()) != 0 {
		t.Fatal("ran too early")
	}

	now = now.Add(2 * time.Minute)
	if err := a.RunDueSchedules(ctx); err != nil {
		t.Fatal(err)
	}
	a.Wait()
	reqs := r.requests()
	if len(reqs) != 1 || !strings.Contains(reqs[0].Prompt, "Task #1: Daily digest") {
		t.Fatalf("reqs = %+v", reqs)
	}
	var msgs []string
	notes, _ := a.Store.Notifications(ctx, true)
	for _, n := range notes {
		msgs = append(msgs, n.Message)
	}
	if all := strings.Join(msgs, "|"); len(notes) != 2 || !strings.Contains(all, "Reminder: Daily digest") || !strings.Contains(all, "moved to Review") {
		t.Fatalf("notes = %v", msgs)
	}
	if moved, _ := a.Store.Task(ctx, task.ID); moved.LaneID != review {
		t.Fatal("scheduled session should carry the schedule's done lane")
	}
	got, _ := a.Store.Schedule(ctx, agentSched.ID)
	if want := time.Date(2026, 10, 21, 9, 0, 0, 0, time.Local); !got.NextRunAt.Equal(want) || got.LastRunAt == nil {
		t.Fatalf("after run: %+v", got)
	}

	// Running again in the same minute is a no-op.
	a.RunDueSchedules(ctx)
	a.Wait()
	if len(r.requests()) != 1 {
		t.Fatal("schedule fired twice")
	}
}

func TestDueReminders(t *testing.T) {
	a, _ := newApp(t)
	ctx := context.Background()
	now := time.Date(2026, 10, 20, 14, 59, 58, 0, time.Local)
	a.Now = func() time.Time { return now }
	due := time.Date(2026, 10, 20, 15, 0, 0, 0, time.Local)
	task, _ := a.Store.CreateTask(ctx, store.Task{Title: "Call Ana", Kind: "reminder", DueAt: &due})

	if err := a.RunDueReminders(ctx); err != nil {
		t.Fatal(err)
	}
	if n, _ := a.Store.Notifications(ctx, true); len(n) != 0 {
		t.Fatalf("notified early: %+v", n)
	}

	now = due
	a.RunDueReminders(ctx)
	a.RunDueReminders(ctx)
	n, _ := a.Store.Notifications(ctx, true)
	if len(n) != 1 || n[0].Message != "Due: Call Ana" || *n[0].TaskID != task.ID {
		t.Fatalf("notifications = %+v", n)
	}
	if got, _ := a.Store.Task(ctx, task.ID); got.RemindedAt == nil || !got.RemindedAt.Equal(due) {
		t.Fatalf("reminded_at = %v", got.RemindedAt)
	}
}

func TestTaskContext(t *testing.T) {
	got := TaskContext(store.Task{ID: 7, Title: "T", Description: "desc",
		Fields: map[string]string{"slack": "s", "productive": "p"}})
	want := "Task #7: T\n\ndesc\n\nLinks:\n- productive: p\n- slack: s"
	if got != want {
		t.Fatalf("got %q", got)
	}
}

func TestLinksBrief(t *testing.T) {
	task := store.Task{
		Description: "See https://app.productive.io/1-org/tasks/42. Also (https://x.slack.com/archives/C1/p2) and https://app.productive.io/1-org/tasks/42 again.",
		Fields:      map[string]string{"pull_request": "https://github.com/o/r/pull/3", "note": "no url"},
	}
	want := []string{"https://app.productive.io/1-org/tasks/42", "https://x.slack.com/archives/C1/p2", "https://github.com/o/r/pull/3"}
	if got := TaskURLs(task); strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("urls = %q", got)
	}
	if got := LinksBrief(task); got != LinksIntro+"\n\n- "+strings.Join(want, "\n- ") {
		t.Fatalf("brief = %q", got)
	}
	if got := LinksBrief(store.Task{Description: "nothing linked"}); got != "" {
		t.Fatalf("no links should give no brief: %q", got)
	}

	a, _ := newApp(t)
	if brief := a.TaskBrief(context.Background(), task); !strings.Contains(brief, LinksIntro) || !strings.Contains(brief, "- https://github.com/o/r/pull/3") {
		t.Fatalf("task brief lacks the links:\n%s", brief)
	}
}

func TestShutdownInterruptsAndResume(t *testing.T) {
	a, r := newApp(t)
	ctx := context.Background()
	r.block = make(chan struct{})
	sess, _ := a.StartSession(ctx, StartOpts{Prompt: "long job"})
	a.Shutdown()
	got, _ := a.Store.Session(ctx, sess.ID)
	if got.Status != store.StatusInterrupted {
		t.Fatalf("shutdown should mark interrupted, got %+v", got)
	}
	// Nothing new starts once shut down, e.g. skills after a turn that just finished.
	if _, err := a.SendMessage(ctx, sess.ID, "more"); !errors.Is(err, ErrStopping) {
		t.Fatalf("send after shutdown: %v", err)
	}

	// A fresh process picks the session up again.
	b := New(a.Store, r, "/default/dir", "bypassPermissions")
	close(r.block)
	if err := b.Resume(ctx, sess.ID); err != nil {
		t.Fatal(err)
	}
	b.Wait()
	reqs := r.requests()
	if last := reqs[len(reqs)-1]; last.ResumeID != "claude-1" || last.Prompt != ResumePrompt {
		t.Fatalf("resume should continue the agent session: %+v", last)
	}
	if got, _ := b.Store.Session(ctx, sess.ID); got.Status != store.StatusIdle {
		t.Fatalf("after resume: %+v", got)
	}
}

func TestResumeWithoutAgentSessionReruns(t *testing.T) {
	a, r := newApp(t)
	ctx := context.Background()
	sess, _ := a.Store.CreateSession(ctx, store.Session{Title: "t", Workdir: "/w"})
	if err := a.Resume(ctx, sess.ID); !errors.Is(err, ErrNoResume) {
		t.Fatalf("no prompt to rerun: got %v", err)
	}
	a.Store.AddMessage(ctx, store.Message{SessionID: sess.ID, Role: "user", Kind: "text", Content: "first prompt"})
	a.Store.SetSessionStatus(ctx, sess.ID, store.StatusInterrupted, "")
	if err := a.Resume(ctx, sess.ID); err != nil {
		t.Fatal(err)
	}
	a.Wait()
	reqs := r.requests()
	if len(reqs) != 1 || reqs[0].ResumeID != "" || reqs[0].Prompt != "first prompt" || reqs[0].Workdir != "/w" {
		t.Fatalf("should rerun the first prompt: %+v", reqs)
	}
	if err := a.Resume(ctx, 999); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("expected not found, got %v", err)
	}
}

func TestAutoArchive(t *testing.T) {
	a, _ := newApp(t)
	ctx := context.Background()
	now := time.Date(2026, 10, 20, 9, 0, 0, 0, time.Local)
	a.Now = func() time.Time { return now }
	a.Store.SetClock(func() time.Time { return now })
	lane := lanesByName(t, a)
	task, _ := a.Store.CreateTask(ctx, store.Task{Title: "T", LaneID: lane["In progress"]})
	a.Store.SaveSettings(ctx, store.Settings{ArchiveLaneID: ptr(lane["Done"]), ArchiveAfterMinutes: 60})

	archived := func() bool {
		t.Helper()
		if err := a.RunAutoArchive(ctx); err != nil {
			t.Fatal(err)
		}
		got, _ := a.Store.Task(ctx, task.ID)
		return got.ArchivedAt != nil
	}
	now = now.Add(2 * time.Hour) // time in other lanes doesn't count
	a.MoveTask(ctx, task.ID, lane["Done"], -1)
	now = now.Add(59 * time.Minute)
	if archived() {
		t.Fatal("archived too early")
	}
	now = now.Add(time.Minute)
	if !archived() {
		t.Fatal("not archived after an hour in Done")
	}

	// Off: no lane, or no duration.
	a.Store.UnarchiveTask(ctx, task.ID)
	now = now.Add(24 * time.Hour)
	a.Store.SaveSettings(ctx, store.Settings{ArchiveAfterMinutes: 60})
	if archived() {
		t.Fatal("archived with no lane")
	}
	a.Store.SaveSettings(ctx, store.Settings{ArchiveLaneID: ptr(lane["Done"])})
	if archived() {
		t.Fatal("archived with no duration")
	}
}

func ptr(id int64) *int64 { return &id }

func TestHandoff(t *testing.T) {
	a, r := newApp(t)
	ctx := context.Background()
	lanes := lanesByName(t, a)
	wip, review := lanes["In progress"], lanes["Review"]
	task, _ := a.Store.CreateTask(ctx, store.Task{Title: "Big refactor", LaneID: wip, Workdir: "/repo"})

	old, _ := a.StartSession(ctx, StartOpts{TaskID: task.ID, Prompt: "start", Title: "Refactor", Mode: "plan", Model: "sonnet"})
	a.Wait()
	a.Store.SetSessionDoneLane(ctx, old.ID, &review)

	if err := a.Handoff(ctx, old.ID, "then open a PR"); err != nil {
		t.Fatal(err)
	}
	a.Wait()

	reqs := r.requests()
	if len(reqs) != 3 || reqs[1].Prompt != HandoffPrompt || reqs[1].ResumeID != "claude-1" {
		t.Fatalf("the brief should be asked of the old session: %+v", reqs)
	}
	next := reqs[2]
	if next.ResumeID != "" || next.Workdir != "/repo" || next.Mode != "plan" || next.Model != "sonnet" {
		t.Fatalf("the new session should start fresh with the same settings: %+v", next)
	}
	for _, want := range []string{HandoffIntro, "Task #", "Big refactor", "\n\nok\n\n", "then open a PR"} {
		if !strings.Contains(next.Prompt, want) {
			t.Fatalf("new prompt lacks %q:\n%s", want, next.Prompt)
		}
	}

	got, _ := a.Store.Session(ctx, old.ID)
	if got.ChildID == nil || got.Status != store.StatusIdle {
		t.Fatalf("old session = %+v", got)
	}
	child, _ := a.Store.Session(ctx, *got.ChildID)
	if child.ParentID == nil || *child.ParentID != old.ID || child.TaskID != task.ID || child.Title != "Refactor" ||
		child.Trigger != "handoff" || child.DoneLaneID == nil || *child.DoneLaneID != review {
		t.Fatalf("new session = %+v", child)
	}
	// Only the new session's turn finishes the work; writing the brief does not.
	if tk, _ := a.Store.Task(ctx, task.ID); tk.LaneID != review {
		t.Fatalf("task in lane %d, want review %d", tk.LaneID, review)
	}
	if n, _ := a.Store.Notifications(ctx, true); len(n) != 1 {
		t.Fatalf("notifications = %+v", n)
	}
}

func TestHandoffWithoutNoteAndFailures(t *testing.T) {
	a, r := newApp(t)
	ctx := context.Background()

	if err := a.Handoff(ctx, 999, ""); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("missing session: %v", err)
	}
	r.err = errors.New("no claude")
	broken, _ := a.StartSession(ctx, StartOpts{Prompt: "p"})
	a.Wait()
	a.Store.SetClaudeSessionID(ctx, broken.ID, "")
	if err := a.Handoff(ctx, broken.ID, ""); !errors.Is(err, ErrNoResume) {
		t.Fatalf("no agent session: %v", err)
	}

	// The brief turn fails: no new session, a notification instead.
	sess, _ := a.StartSession(ctx, StartOpts{Prompt: "p"})
	a.Wait()
	a.Handoff(ctx, sess.ID, "")
	a.Wait()
	if got, _ := a.Store.Session(ctx, sess.ID); got.ChildID != nil {
		t.Fatalf("failed brief must not start a session: %+v", got)
	}
	n, _ := a.Store.Notifications(ctx, true)
	if len(n) != 1 || !strings.Contains(n[0].Message, "Could not hand off") {
		t.Fatalf("notifications = %+v", n)
	}

	r.err = nil
	r.block = make(chan struct{})
	busy, _ := a.StartSession(ctx, StartOpts{Prompt: "p"})
	for len(r.requests()) < 4 {
		time.Sleep(time.Millisecond)
	}
	if err := a.Handoff(ctx, busy.ID, ""); !errors.Is(err, ErrBusy) {
		t.Fatalf("running session: %v", err)
	}
	close(r.block)
	a.Wait()
	r.block = nil

	if err := a.Handoff(ctx, busy.ID, ""); err != nil {
		t.Fatal(err)
	}
	a.Wait()
	reqs := r.requests()
	if last := reqs[len(reqs)-1].Prompt; !strings.HasSuffix(last, "Continue with the next steps.") {
		t.Fatalf("without a note the new session goes on: %q", last)
	}
}

func TestTaskBriefSharesEarlierResults(t *testing.T) {
	a, r := newApp(t)
	ctx := context.Background()
	lanes := lanesByName(t, a)
	research, _ := a.Store.CreateTask(ctx, store.Task{Title: "Research caching", Description: "Compare options"})
	// A finished session, one handed off to a newer one, and one still running.
	addSession := func(title, status, reply string, parent *int64) store.Session {
		x, _ := a.Store.CreateSession(ctx, store.Session{TaskID: research.ID, Title: title, Mode: "plan", ParentID: parent})
		a.Store.AddMessage(ctx, store.Message{SessionID: x.ID, Role: "user", Kind: "text", Content: "go"})
		a.Store.AddMessage(ctx, store.Message{SessionID: x.ID, Role: "system", Kind: "result", Content: reply})
		a.Store.SetSessionStatus(ctx, x.ID, status, "")
		return x
	}
	addSession("First pass", store.StatusIdle, "Redis wins.", nil)
	old := addSession("Deep dive", store.StatusIdle, "BRIEF FOR THE NEXT AGENT", nil)
	addSession("Deep dive", store.StatusIdle, "Use Redis with a 5 minute TTL. "+strings.Repeat("x", sharedRunes), &old.ID)
	addSession("Still going", store.StatusRunning, "half done", nil)
	empty, _ := a.Store.CreateTask(ctx, store.Task{Title: "Nothing yet"})

	follow, _ := a.Store.CreateTask(ctx, store.Task{Title: "Make tasks", LaneID: lanes["Todo"], ContextFrom: []int64{research.ID, empty.ID}})
	r.result = func(req agent.Request) string {
		if strings.Contains(req.Prompt, "Redis wins.") {
			return "SUMMARY: compared Redis and Memcached; Redis wins."
		}
		return "SUMMARY: settled on a 5 minute TTL."
	}
	brief := a.TaskBrief(ctx, follow)
	for _, want := range []string{"Task #" + itoa(follow.ID) + ": Make tasks", SharedIntro, "## Task #" + itoa(research.ID) + ": Research caching",
		"Compare options", "Final reply of session", "First pass", "Redis wins.", "Use Redis with a 5 minute TTL.", "(cut;", "Nothing yet", "(No agent results yet.)"} {
		if !strings.Contains(brief, want) {
			t.Fatalf("brief lacks %q:\n%s", want, brief)
		}
	}
	for _, unwanted := range []string{"BRIEF FOR THE NEXT AGENT", "half done"} {
		if strings.Contains(brief, unwanted) {
			t.Fatalf("brief has %q:\n%s", unwanted, brief)
		}
	}
	if strings.Index(brief, "Redis wins.") > strings.Index(brief, "5 minute TTL") {
		t.Fatal("sessions should read oldest first")
	}
	if solo, _ := a.Store.Task(ctx, empty.ID); a.TaskBrief(ctx, solo) != TaskContext(solo) {
		t.Fatal("a task that builds on nothing gets only its own context")
	}

	// Without summaries the brief has the final replies, and the sessions get
	// summarised in the background for the next brief, from their transcripts
	// as they have no agent conversation to fork.
	a.Wait()
	reqs := r.requests()
	if len(reqs) != 2 {
		t.Fatalf("want a summary of the two finished sessions: %+v", reqs)
	}
	for _, req := range reqs {
		if !strings.HasPrefix(req.Prompt, TranscriptSummaryPrompt) || req.Model != SummaryModel || req.Mode != "plan" || req.ResumeID != "" {
			t.Fatalf("summary request = %+v", req)
		}
	}
	brief = a.TaskBrief(ctx, follow)
	for _, want := range []string{"### Summary of session", "SUMMARY: compared Redis and Memcached; Redis wins.", "SUMMARY: settled on a 5 minute TTL."} {
		if !strings.Contains(brief, want) {
			t.Fatalf("brief lacks %q:\n%s", want, brief)
		}
	}
	if strings.Contains(brief, "Final reply of session") || strings.Contains(brief, "xxxxx") {
		t.Fatalf("summaries should replace the final replies:\n%s", brief)
	}
	a.Wait()
	if n := len(r.requests()); n != 2 {
		t.Fatalf("fresh summaries are not written again: %d requests", n)
	}

	// Rules get the shared results through {{.Context}}.
	wip := lanes["In progress"]
	a.Store.SaveRule(ctx, store.Rule{Name: "Plan", ToLaneID: wip, Enabled: true, Prompt: "{{.Context}}\n\nPlan it."})
	if _, err := a.MoveTask(ctx, follow.ID, wip, 0); err != nil {
		t.Fatal(err)
	}
	a.Wait()
	if p := r.requests()[2].Prompt; !strings.Contains(p, "SUMMARY: compared Redis") || !strings.HasSuffix(p, "Plan it.") {
		t.Fatalf("rule prompt = %q", p)
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

func TestSkillAutomationRunsAfterTurn(t *testing.T) {
	a, r := newApp(t)
	ctx := context.Background()
	lanes := lanesByName(t, a)
	review := lanes["Review"]
	task, _ := a.Store.CreateTask(ctx, store.Task{Title: "Find a queue", Kind: "research", LaneID: lanes["In progress"]})
	other, _ := a.Store.CreateTask(ctx, store.Task{Title: "Code it", Kind: "coding", LaneID: lanes["In progress"]})
	sum, err := a.Store.SaveSkill(ctx, store.Skill{Name: "Summary", Body: "Summarise {{.Task.Title}}.", Artifact: true, Model: "fable",
		Automations: []store.SkillAutomation{{Kind: "research", Enabled: true}}})
	if err != nil {
		t.Fatal(err)
	}
	a.Store.SaveSkill(ctx, store.Skill{Name: "Notes", Body: "Note it.", Automations: []store.SkillAutomation{{Enabled: true}}})

	sess, err := a.StartSession(ctx, StartOpts{TaskID: task.ID, Prompt: "research", Model: "sonnet", DoneLaneID: &review})
	if err != nil {
		t.Fatal(err)
	}
	a.Wait()
	reqs := r.requests()
	if len(reqs) != 3 {
		t.Fatalf("want the turn and two skill turns, got %+v", reqs)
	}
	// A skill's model runs its turn only; one without keeps the session's.
	if reqs[0].Model != "sonnet" || reqs[1].Model != "fable" || reqs[2].Model != "sonnet" {
		t.Fatalf("models = %q, %q, %q", reqs[0].Model, reqs[1].Model, reqs[2].Model)
	}
	// Skills run in automation order, each continuing the session.
	if !strings.HasPrefix(reqs[1].Prompt, `Apply the skill "Summary"`) || !strings.Contains(reqs[1].Prompt, ArtifactNote) ||
		!strings.HasSuffix(reqs[1].Prompt, "Summarise Find a queue.") || reqs[1].ResumeID != "claude-1" {
		t.Fatalf("summary prompt = %+v", reqs[1])
	}
	if !strings.HasPrefix(reqs[2].Prompt, `Apply the skill "Notes"`) || strings.Contains(reqs[2].Prompt, ArtifactNote) {
		t.Fatalf("notes prompt = %q", reqs[2].Prompt)
	}
	arts, _ := a.Store.Artifacts(ctx, task.ID)
	if len(arts) != 1 || arts[0].Skill != "Summary" || arts[0].Content != "ok" || *arts[0].SessionID != sess.ID {
		t.Fatalf("artifacts = %+v", arts)
	}
	// The done lane waits for the skills.
	if got, _ := a.Store.Task(ctx, task.ID); got.LaneID != review {
		t.Fatalf("task in lane %d, want %d", got.LaneID, review)
	}
	if got, _ := a.Store.Session(ctx, sess.ID); got.Status != store.StatusIdle || got.Model != "sonnet" {
		t.Fatalf("session = %+v", got)
	}

	// A coding task only matches the any-kind skill; skill turns don't trigger skills.
	if _, err := a.StartSession(ctx, StartOpts{TaskID: other.ID, Prompt: "code"}); err != nil {
		t.Fatal(err)
	}
	a.Wait()
	if reqs = r.requests()[3:]; len(reqs) != 2 || !strings.HasPrefix(reqs[1].Prompt, `Apply the skill "Notes"`) {
		t.Fatalf("coding requests = %+v", reqs)
	}

	// Standalone sessions run no automations.
	if _, err := a.StartSession(ctx, StartOpts{Prompt: "chat"}); err != nil {
		t.Fatal(err)
	}
	a.Wait()
	if n := len(r.requests()); n != 6 {
		t.Fatalf("standalone ran skills: %d requests", n)
	}

	// Run by hand.
	if err := a.RunSkill(ctx, sess.ID, sum.ID); err != nil {
		t.Fatal(err)
	}
	a.Wait()
	if arts, _ = a.Store.Artifacts(ctx, task.ID); len(arts) != 2 {
		t.Fatalf("artifacts = %+v", arts)
	}
	if reqs = r.requests(); len(reqs) != 7 || reqs[6].Model != "fable" {
		t.Fatalf("a skill run by hand triggered automations, or lost its model: %+v", reqs)
	}
}

func TestFailedSkillStopsTheChain(t *testing.T) {
	a, r := newApp(t)
	ctx := context.Background()
	lanes := lanesByName(t, a)
	review := lanes["Review"]
	task, _ := a.Store.CreateTask(ctx, store.Task{Title: "T", LaneID: lanes["Todo"]})
	a.Store.SaveSkill(ctx, store.Skill{Name: "A", Body: "a", Automations: []store.SkillAutomation{{Enabled: true}}})
	a.Store.SaveSkill(ctx, store.Skill{Name: "B", Body: "{{.Task.Nope}}", Automations: []store.SkillAutomation{{Enabled: true}}})

	// B's template is broken: it never runs and the task stays put.
	sess, _ := a.Store.CreateSession(ctx, store.Session{TaskID: task.ID, Title: "s", Mode: "plan", DoneLaneID: &review})
	if err := a.launch(sess, "work", "", nil); err != nil {
		t.Fatal(err)
	}
	a.Wait() // the work turn and skill A both succeed; B's template fails
	if n := len(r.requests()); n != 2 {
		t.Fatalf("requests = %d", n)
	}
	if got, _ := a.Store.Task(ctx, task.ID); got.LaneID != lanes["Todo"] {
		t.Fatalf("task moved despite a failed skill: %+v", got)
	}
	notes, _ := a.Store.Notifications(ctx, true)
	if len(notes) != 1 || !strings.Contains(notes[0].Message, `Skill "B" could not run`) {
		t.Fatalf("notifications = %+v", notes)
	}

	// A skill turn that fails ends the chain the same way.
	r.mu.Lock()
	r.failOn = `Apply the skill "A"`
	r.mu.Unlock()
	a.Store.MarkNotificationRead(ctx, notes[0].ID)
	sess, _ = a.Store.CreateSession(ctx, store.Session{TaskID: task.ID, Title: "s2", Mode: "plan", DoneLaneID: &review})
	if err := a.launch(sess, "work", "", nil); err != nil {
		t.Fatal(err)
	}
	a.Wait()
	if n := len(r.requests()); n != 4 {
		t.Fatalf("requests = %d", n)
	}
	if got, _ := a.Store.Task(ctx, task.ID); got.LaneID != lanes["Todo"] {
		t.Fatalf("task moved despite a failed skill: %+v", got)
	}
	notes, _ = a.Store.Notifications(ctx, true)
	if len(notes) != 1 || !strings.Contains(notes[0].Message, `Skill "A" ended error`) {
		t.Fatalf("notifications = %+v", notes)
	}
}

func TestSummaryAfterTurnForksTheConversation(t *testing.T) {
	a, r := newApp(t)
	ctx := context.Background()
	lanes := lanesByName(t, a)
	wip, review := lanes["In progress"], lanes["Review"]
	research, _ := a.Store.CreateTask(ctx, store.Task{Title: "Research", LaneID: wip, Workdir: "/repo"})
	r.result = func(req agent.Request) string {
		if req.Fork {
			return "Whole session: found X in /repo/x.go."
		}
		return "Final: X."
	}

	// Nobody builds on the task yet: no summary is written.
	solo, _ := a.StartSession(ctx, StartOpts{TaskID: research.ID, Prompt: "look", Mode: "bypassPermissions", Model: "sonnet"})
	a.Wait()
	if n := len(r.requests()); n != 1 {
		t.Fatalf("no users, no summary: %d requests", n)
	}

	a.Store.CreateTask(ctx, store.Task{Title: "Build", ContextFrom: []int64{research.ID}})
	sess, _ := a.StartSession(ctx, StartOpts{TaskID: research.ID, Prompt: "dig", Mode: "bypassPermissions", Model: "sonnet", DoneLaneID: &review})
	a.Wait()
	reqs := r.requests()
	if len(reqs) != 3 {
		t.Fatalf("requests = %+v", reqs)
	}
	var sum agent.Request
	for _, req := range reqs {
		if req.Fork {
			sum = req
		}
	}
	if sum.Prompt != SummaryPrompt || sum.ResumeID != "claude-1" || sum.Mode != "plan" || sum.Model != "sonnet" || sum.Workdir != "/repo" {
		t.Fatalf("summary request = %+v", sum)
	}
	got, _ := a.Store.Session(ctx, sess.ID)
	msgs, _ := a.Store.Messages(ctx, sess.ID, 0)
	if got.Summary != "Whole session: found X in /repo/x.go." || got.SummaryStale || got.SummaryUpto != msgs[len(msgs)-1].ID {
		t.Fatalf("session = %+v", got)
	}
	if len(msgs) != 3 {
		t.Fatalf("the summary must stay out of the transcript: %+v", msgs)
	}
	if got.CostUSD != 1 {
		t.Fatalf("cost should include the summary: %v", got.CostUSD)
	}
	if other, _ := a.Store.Session(ctx, solo.ID); other.Summary != "" {
		t.Fatalf("only the finished session is summarised: %+v", other)
	}

	// A follow-up makes the summary stale; regenerating it moves nothing and
	// fires no rules.
	if tk, _ := a.Store.Task(ctx, research.ID); tk.LaneID != review {
		t.Fatalf("the turn should move the task: lane %d", tk.LaneID)
	}
	a.Store.SaveRule(ctx, store.Rule{Name: "Check", ToLaneID: review, Enabled: true, Prompt: "check"})
	a.Store.MoveTask(ctx, research.ID, wip, 0)
	a.SendMessage(ctx, solo.ID, "more")
	a.Wait()
	before := len(r.requests())
	stale, _ := a.Store.Session(ctx, sess.ID)
	a.Store.AddMessage(ctx, store.Message{SessionID: sess.ID, Role: "user", Kind: "text", Content: "and Y?"})
	if stale, _ = a.Store.Session(ctx, sess.ID); !stale.SummaryStale {
		t.Fatalf("summary should be stale: %+v", stale)
	}
	if err := a.Summarize(ctx, sess.ID); err != nil {
		t.Fatal(err)
	}
	a.Wait()
	if reqs = r.requests(); len(reqs) != before+1 || !reqs[before].Fork {
		t.Fatalf("one request for the summary: %+v", reqs[before:])
	}
	if got, _ = a.Store.Session(ctx, sess.ID); got.SummaryStale || got.Summary == "" {
		t.Fatalf("regenerated = %+v", got)
	}
	if tk, _ := a.Store.Task(ctx, research.ID); tk.LaneID != wip {
		t.Fatalf("a summary must not move the task: lane %d", tk.LaneID)
	}
	if xs, _ := a.Store.Sessions(ctx, research.ID, ""); len(xs) != 2 {
		t.Fatalf("a summary must not start sessions: %d", len(xs))
	}
}

func TestSummaryFallbackAndFailures(t *testing.T) {
	a, r := newApp(t)
	ctx := context.Background()
	if err := a.Summarize(ctx, 999); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("missing session: %v", err)
	}
	task, _ := a.Store.CreateTask(ctx, store.Task{Title: "Research"})
	sess, _ := a.StartSession(ctx, StartOpts{TaskID: task.ID, Prompt: "find the bug", Model: "opus"})
	a.Wait()

	// The fork fails: the summary comes from the transcript, with the cheap model.
	r.fail = func(req agent.Request) error {
		if req.Fork {
			return errors.New("no conversation")
		}
		return nil
	}
	r.result = func(req agent.Request) string { return "From the transcript." }
	if err := a.Summarize(ctx, sess.ID); err != nil {
		t.Fatal(err)
	}
	a.Wait()
	reqs := r.requests()
	last := reqs[len(reqs)-1]
	if len(reqs) != 3 || !reqs[1].Fork || last.Fork || last.ResumeID != "" || last.Model != SummaryModel ||
		!strings.Contains(last.Prompt, "[User]\nfind the bug") || !strings.Contains(last.Prompt, "[Final reply of the turn]\nok") {
		t.Fatalf("requests = %+v", reqs)
	}
	if got, _ := a.Store.Session(ctx, sess.ID); got.Summary != "From the transcript." {
		t.Fatalf("session = %+v", got)
	}

	// Both fail: the summary stays as it was and a notification says why.
	r.fail = func(agent.Request) error { return errors.New("no claude") }
	a.Store.AddMessage(ctx, store.Message{SessionID: sess.ID, Role: "user", Kind: "text", Content: "more"})
	a.Summarize(ctx, sess.ID)
	a.Wait()
	if got, _ := a.Store.Session(ctx, sess.ID); got.Summary != "From the transcript." || !got.SummaryStale {
		t.Fatalf("session = %+v", got)
	}
	n, _ := a.Store.Notifications(ctx, true)
	if len(n) != 1 || !strings.Contains(n[0].Message, "Could not summarise") || !strings.Contains(n[0].Message, "no claude") {
		t.Fatalf("notifications = %+v", n)
	}

	// While a turn runs there is nothing settled to sum up; while a summary is
	// being written, asking again does nothing.
	r.fail = nil
	r.block = make(chan struct{})
	a.SendMessage(ctx, sess.ID, "go on")
	if err := a.Summarize(ctx, sess.ID); !errors.Is(err, ErrBusy) {
		t.Fatalf("running session: %v", err)
	}
	close(r.block)
	a.Wait()
	r.block = make(chan struct{})
	before := len(r.requests())
	a.Summarize(ctx, sess.ID)
	if !a.Summarizing(sess.ID) {
		t.Fatal("should be summarising")
	}
	a.Summarize(ctx, sess.ID)
	close(r.block)
	a.Wait()
	if a.Summarizing(sess.ID) || len(r.requests()) != before+1 {
		t.Fatalf("one summary at a time: %d requests, want %d", len(r.requests()), before+1)
	}
}

func TestTranscriptCutsLongSessions(t *testing.T) {
	msgs := []store.Message{
		{ID: 1, Role: "user", Kind: "text", Content: "start"},
		{ID: 2, Role: "tool", Kind: "tool_result", Content: strings.Repeat("r", 5000)},
	}
	for i := 0; i < 10; i++ {
		msgs = append(msgs, store.Message{ID: int64(3 + i), Role: "assistant", Kind: "text", Content: strings.Repeat("a", 20000)})
	}
	msgs = append(msgs, store.Message{ID: 13, Role: "system", Kind: "result", Content: "the end"},
		store.Message{ID: 14, Role: "user", Kind: "text", Content: "after the summary"})
	got := transcript(msgs, 13)
	if !strings.HasPrefix(got, "[User]\nstart") || !strings.Contains(got, "left out") || !strings.HasSuffix(got, "the end\n\n") {
		t.Fatalf("transcript = %.200q … %q", got, got[len(got)-50:])
	}
	if strings.Contains(got, strings.Repeat("r", 2001)) || strings.Contains(got, "after the summary") {
		t.Fatal("tool output should be cut and later messages left out")
	}
	if n := len([]rune(got)); n > maxTranscriptRunes+100 {
		t.Fatalf("transcript has %d runes", n)
	}
}

func TestSummaryWaitsForSkills(t *testing.T) {
	a, r := newApp(t)
	ctx := context.Background()
	task, _ := a.Store.CreateTask(ctx, store.Task{Title: "Research"})
	a.Store.CreateTask(ctx, store.Task{Title: "Build", ContextFrom: []int64{task.ID}})
	a.Store.SaveSkill(ctx, store.Skill{Name: "Notes", Body: "Note it.", Automations: []store.SkillAutomation{{Enabled: true}}})
	sess, _ := a.StartSession(ctx, StartOpts{TaskID: task.ID, Prompt: "dig"})
	a.Wait()
	reqs := r.requests()
	if len(reqs) != 3 || !strings.HasPrefix(reqs[1].Prompt, `Apply the skill "Notes"`) || !reqs[2].Fork {
		t.Fatalf("want the turn, the skill, then the summary: %+v", reqs)
	}
	if got, _ := a.Store.Session(ctx, sess.ID); got.Summary == "" || got.SummaryStale {
		t.Fatalf("the summary should cover the skill turn too: %+v", got)
	}
}

func TestNeedsInput(t *testing.T) {
	a, r := newApp(t)
	ctx := context.Background()
	task, _ := a.Store.CreateTask(ctx, store.Task{Title: "Ask me"})
	set := func(reply string, tools, denied []string) {
		r.mu.Lock()
		r.result = func(agent.Request) string { return reply }
		r.tools, r.denied = tools, denied
		r.mu.Unlock()
	}
	needs := func(id int64) string {
		t.Helper()
		got, err := a.Store.Session(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		return got.NeedsInput
	}

	set("Done. Which **branch** should I push to?", nil, nil)
	sess, err := a.StartSession(ctx, StartOpts{TaskID: task.ID, Prompt: "work"})
	if err != nil {
		t.Fatal(err)
	}
	a.Wait()
	if got := needs(sess.ID); got != "asked a question" {
		t.Fatalf("needs input = %q", got)
	}
	notes, _ := a.Store.Notifications(ctx, true)
	if len(notes) != 1 || notes[0].SessionID == nil || *notes[0].SessionID != sess.ID || notes[0].TaskID == nil ||
		notes[0].Message != `Agent needs your input in "work": it asked a question` {
		t.Fatalf("notifications = %+v", notes)
	}

	// Replying clears it; a finished turn without a question leaves it clear.
	set("All done.", nil, nil)
	a.SendMessage(ctx, sess.ID, "main")
	a.Wait()
	if got := needs(sess.ID); got != "" {
		t.Fatalf("after reply: %q", got)
	}

	set("I could not run it.", []string{`Bash {"command":"rm x"}`}, []string{"Bash", "Write", "Bash"})
	a.SendMessage(ctx, sess.ID, "again")
	a.Wait()
	if got := needs(sess.ID); got != "needs permission for Bash, Write" {
		t.Fatalf("denied: %q", got)
	}
	if err := a.DismissNeedsInput(ctx, sess.ID); err != nil || needs(sess.ID) != "" {
		t.Fatalf("dismiss: %v", err)
	}

	set("Here is the plan.", []string{`ExitPlanMode {"plan":"x"}`}, nil)
	a.SendMessage(ctx, sess.ID, "plan it")
	a.Wait()
	if got := needs(sess.ID); got != "waits for its plan to be approved" {
		t.Fatalf("plan: %q", got)
	}
	set("ok", []string{`AskUserQuestion {"questions":[]}`}, nil)
	a.SendMessage(ctx, sess.ID, "ask")
	a.Wait()
	if got := needs(sess.ID); got != "asked a question" {
		t.Fatalf("ask tool: %q", got)
	}

	// A failed turn is an error, not a wait; a standalone session notifies without a task.
	r.mu.Lock()
	r.failOn = "boom"
	r.mu.Unlock()
	set("Shall I retry?", nil, nil)
	a.SendMessage(ctx, sess.ID, "boom")
	a.Wait()
	if got, _ := a.Store.Session(ctx, sess.ID); got.Status != store.StatusError || got.NeedsInput != "" {
		t.Fatalf("failed turn: %+v", got)
	}
	solo, _ := a.StartSession(ctx, StartOpts{Prompt: "hi"})
	a.Wait()
	notes, _ = a.Store.Notifications(ctx, true)
	if needs(solo.ID) != "asked a question" || notes[0].TaskID != nil || *notes[0].SessionID != solo.ID {
		t.Fatalf("standalone: %+v", notes[0])
	}
}

func TestEndsWithQuestion(t *testing.T) {
	for reply, want := range map[string]bool{
		"Should I continue?":                   true,
		"Done.\n\n**Want me to open a PR?**\n": true,
		"Is it `main`?`":                       true,
		"(or should I wait?)":                  true,
		"Why? Because it was broken. Fixed.":   false,
		"":                                     false,
		"All done.":                            false,
	} {
		if got := endsWithQuestion(reply); got != want {
			t.Errorf("endsWithQuestion(%q) = %v", reply, got)
		}
	}
}

func TestRulesPerProject(t *testing.T) {
	a, r := newApp(t)
	ctx := context.Background()
	lanes := lanesByName(t, a)
	wip, review := lanes["In progress"], lanes["Review"]
	backyard, _ := a.Store.CreateProject(ctx, store.Project{Name: "Backyard"})
	mine, _ := a.Store.CreateTask(ctx, store.Task{Title: "Assistant task", LaneID: wip, ProjectID: 1})
	theirs, _ := a.Store.CreateTask(ctx, store.Task{Title: "Backyard task", LaneID: wip, ProjectID: backyard.ID})
	assistant := int64(1)
	a.Store.SaveRule(ctx, store.Rule{Name: "Assistant review", ProjectID: &assistant, FromLaneID: &wip, ToLaneID: review, Enabled: true, Prompt: "assistant"})
	a.Store.SaveRule(ctx, store.Rule{Name: "Backyard review", ProjectID: &backyard.ID, FromLaneID: &wip, ToLaneID: review, Enabled: true, Prompt: "backyard"})

	if started, err := a.MoveTask(ctx, mine.ID, review, 0); err != nil || len(started) != 1 || started[0].Title != "Assistant review" {
		t.Fatalf("assistant: %+v, %v", started, err)
	}
	if started, err := a.MoveTask(ctx, theirs.ID, review, 0); err != nil || len(started) != 1 || started[0].Title != "Backyard review" {
		t.Fatalf("backyard: %+v, %v", started, err)
	}
	a.Wait()
	if reqs := r.requests(); len(reqs) != 2 || reqs[0].Prompt != "assistant" || reqs[1].Prompt != "backyard" {
		t.Fatalf("requests = %+v", reqs)
	}
}

func TestRuleRunsSkillInLatestSession(t *testing.T) {
	a, r := newApp(t)
	ctx := context.Background()
	lanes := lanesByName(t, a)
	wip, review, done := lanes["In progress"], lanes["Review"], lanes["Done"]
	task, _ := a.Store.CreateTask(ctx, store.Task{Title: "Ship it", LaneID: wip})
	sk, _ := a.Store.SaveSkill(ctx, store.Skill{Name: "Self-review", Body: "Review {{.Task.Title}}.", Artifact: true})
	a.Store.SaveRule(ctx, store.Rule{Name: "Review it", FromLaneID: &wip, ToLaneID: review, SkillID: &sk.ID, DoneLaneID: &done, Enabled: true})
	// A prompt rule on the same move starts a new session; the skill still goes to the one that did the work.
	a.Store.SaveRule(ctx, store.Rule{Name: "Also", ToLaneID: review, Prompt: "also", Enabled: true})

	// The session's turn ends by moving the task to Review, which fires the rule in that same session.
	sess, err := a.StartSession(ctx, StartOpts{TaskID: task.ID, Prompt: "code", DoneLaneID: &review})
	if err != nil {
		t.Fatal(err)
	}
	a.Wait()
	reqs := r.requests()
	if len(reqs) != 3 {
		t.Fatalf("requests = %+v", reqs)
	}
	var skillReq agent.Request
	for _, q := range reqs[1:] {
		if strings.HasPrefix(q.Prompt, `Apply the skill "Self-review"`) {
			skillReq = q
		}
	}
	if !strings.HasSuffix(skillReq.Prompt, "Review Ship it.") || skillReq.ResumeID != "claude-1" {
		t.Fatalf("skill request = %+v", skillReq)
	}
	arts, _ := a.Store.Artifacts(ctx, task.ID)
	if len(arts) != 1 || arts[0].Skill != "Self-review" || *arts[0].SessionID != sess.ID {
		t.Fatalf("artifacts = %+v", arts)
	}
	// The skill went fine, so the task moves on to the rule's done lane.
	if got, _ := a.Store.Task(ctx, task.ID); got.LaneID != done {
		t.Fatalf("task in lane %d, want done %d", got.LaneID, done)
	}
}

// With no session on the task, a skill rule starts one with the rule's
// settings; the skill is its first turn, and the rule's done lane follows it.
func TestRuleSkillWithoutSessionStartsOne(t *testing.T) {
	a, r := newApp(t)
	ctx := context.Background()
	lanes := lanesByName(t, a)
	todo, review, done := lanes["Todo"], lanes["Review"], lanes["Done"]
	task, _ := a.Store.CreateTask(ctx, store.Task{Title: "Fresh", LaneID: todo})
	sk, _ := a.Store.SaveSkill(ctx, store.Skill{Name: "Guard", Body: "Guard {{.Task.Title}}.", Artifact: true, Model: "opus"})
	a.Store.SaveRule(ctx, store.Rule{Name: "Guard it", ToLaneID: review, SkillID: &sk.ID, DoneLaneID: &done, Mode: "plan",
		Workdir: "/rule/dir", Enabled: true})

	started, err := a.MoveTask(ctx, task.ID, review, 0)
	if err != nil || len(started) != 1 {
		t.Fatalf("started = %+v, %v", started, err)
	}
	if s := started[0]; s.Title != "Guard it" || s.Trigger != "rule:Guard it" || s.Mode != "plan" || s.Workdir != "/rule/dir" || s.Model != "opus" {
		t.Fatalf("session = %+v", s)
	}
	a.Wait()
	reqs := r.requests()
	if len(reqs) != 1 || reqs[0].ResumeID != "" || !strings.HasPrefix(reqs[0].Prompt, fmt.Sprintf(NewSkillIntro, "Guard")) ||
		!strings.Contains(reqs[0].Prompt, "Fresh") || !strings.HasSuffix(reqs[0].Prompt, "Guard Fresh.") {
		t.Fatalf("requests = %+v", reqs)
	}
	if arts, _ := a.Store.Artifacts(ctx, task.ID); len(arts) != 1 || arts[0].Skill != "Guard" || *arts[0].SessionID != started[0].ID {
		t.Fatalf("artifacts = %+v", arts)
	}
	if got, _ := a.Store.Task(ctx, task.ID); got.LaneID != done {
		t.Fatalf("task in lane %d, want done %d", got.LaneID, done)
	}
}

// While the task's latest session works, a skill rule runs its skill in a new
// session rather than failing or waiting.
func TestRuleSkillWhileLatestRunsStartsNewSession(t *testing.T) {
	a, r := newApp(t)
	ctx := context.Background()
	lanes := lanesByName(t, a)
	wip, review, done := lanes["In progress"], lanes["Review"], lanes["Done"]
	task, _ := a.Store.CreateTask(ctx, store.Task{Title: "Busy", LaneID: wip})
	sk, _ := a.Store.SaveSkill(ctx, store.Skill{Name: "Guard", Body: "Guard it."})
	a.Store.SaveRule(ctx, store.Rule{Name: "Guard it", ToLaneID: review, SkillID: &sk.ID, DoneLaneID: &done, Enabled: true})
	first, err := a.StartSession(ctx, StartOpts{TaskID: task.ID, Prompt: "code"})
	if err != nil {
		t.Fatal(err)
	}
	a.Wait()
	r.block = make(chan struct{})
	if _, err := a.SendMessage(ctx, first.ID, "more"); err != nil {
		t.Fatal(err)
	}

	started, err := a.MoveTask(ctx, task.ID, review, 0)
	if err != nil || len(started) != 1 || started[0].ID == first.ID {
		t.Fatalf("started = %+v, %v", started, err)
	}
	close(r.block)
	a.Wait()
	var skillReq agent.Request
	for _, q := range r.requests() {
		if strings.HasPrefix(q.Prompt, fmt.Sprintf(NewSkillIntro, "Guard")) {
			skillReq = q
		}
	}
	if skillReq.ResumeID != "" || !strings.HasSuffix(skillReq.Prompt, "Guard it.") {
		t.Fatalf("skill request = %+v", skillReq)
	}
	if got, _ := a.Store.Task(ctx, task.ID); got.LaneID != done {
		t.Fatalf("task in lane %d, want done %d", got.LaneID, done)
	}
	if n, _ := a.Store.Notifications(ctx, true); slices.ContainsFunc(n, func(x store.Notification) bool { return strings.HasPrefix(x.Message, "Rule") }) {
		t.Fatalf("notifications = %+v", n)
	}
}

// A session can start with a skill: its first turn applies the skill to the
// task, with the prompt as more context, and ends like any first turn.
func TestStartSessionWithSkill(t *testing.T) {
	a, r := newApp(t)
	ctx := context.Background()
	lanes := lanesByName(t, a)
	task, _ := a.Store.CreateTask(ctx, store.Task{Title: "Spec", Description: "Write the spec", LaneID: lanes["Todo"]})
	sk, _ := a.Store.SaveSkill(ctx, store.Skill{Name: "Refine", Body: "Refine the spec."})
	done := lanes["Review"]

	sess, err := a.StartSession(ctx, StartOpts{TaskID: task.ID, SkillID: &sk.ID, Prompt: "The guard found: no tests.", DoneLaneID: &done})
	if err != nil || sess.Title != "Refine" {
		t.Fatalf("session = %+v, %v", sess, err)
	}
	a.Wait()
	reqs := r.requests()
	if len(reqs) != 1 {
		t.Fatalf("requests = %+v", reqs)
	}
	p := reqs[0].Prompt
	brief, body, extra := strings.Index(p, "Write the spec"), strings.Index(p, "Refine the spec."), strings.Index(p, SkillContextIntro+"\n\nThe guard found: no tests.")
	if !strings.HasPrefix(p, fmt.Sprintf(NewSkillIntro, "Refine")) || brief < 0 || body < brief || extra < body {
		t.Fatalf("prompt = %q", p)
	}
	if got, _ := a.Store.Task(ctx, task.ID); got.LaneID != done {
		t.Fatalf("task in lane %d, want done %d", got.LaneID, done)
	}

	// The prompt is optional with a skill; a skill needs a task and must exist.
	if _, err := a.StartSession(ctx, StartOpts{TaskID: task.ID, SkillID: &sk.ID}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.StartSession(ctx, StartOpts{SkillID: &sk.ID, Prompt: "x"}); err == nil {
		t.Fatal("started a skill without a task")
	}
	missing := sk.ID + 100
	if _, err := a.StartSession(ctx, StartOpts{TaskID: task.ID, SkillID: &missing}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("missing skill: %v", err)
	}
	a.Wait()
}

// A gemini session runs every turn, its handoff included, on gemini; its
// summary is written from the transcript since gemini cannot fork.
func TestGeminiSession(t *testing.T) {
	a, r := newApp(t)
	ctx := context.Background()
	research, _ := a.Store.CreateTask(ctx, store.Task{Title: "Research", Workdir: "/repo"})
	a.Store.CreateTask(ctx, store.Task{Title: "Build", ContextFrom: []int64{research.ID}})

	sess, err := a.StartSession(ctx, StartOpts{TaskID: research.ID, Prompt: "look", Agent: agent.Gemini, Model: "flash"})
	if err != nil {
		t.Fatal(err)
	}
	a.Wait()
	if got, _ := a.Store.Session(ctx, sess.ID); got.Agent != agent.Gemini || got.ClaudeSessionID != "claude-1" || got.Summary == "" {
		t.Fatalf("session = %+v", got)
	}
	reqs := r.requests()
	if len(reqs) != 2 || reqs[0].Agent != agent.Gemini || reqs[0].Model != "flash" {
		t.Fatalf("turn = %+v", reqs)
	}
	if sum := reqs[1]; sum.Fork || sum.ResumeID != "" || sum.Agent != agent.Gemini || sum.Model != GeminiSummaryModel {
		t.Fatalf("summary should come from the transcript, on gemini: %+v", sum)
	}

	if _, err := a.SendMessage(ctx, sess.ID, "more"); err != nil {
		t.Fatal(err)
	}
	a.Wait()
	if err := a.Handoff(ctx, sess.ID, ""); err != nil {
		t.Fatal(err)
	}
	a.Wait()
	for _, req := range r.requests() {
		if req.Agent != agent.Gemini {
			t.Fatalf("request not on gemini: %+v", req)
		}
	}
	got, _ := a.Store.Session(ctx, sess.ID)
	if child, _ := a.Store.Session(ctx, *got.ChildID); child.Agent != agent.Gemini || child.Model != "flash" {
		t.Fatalf("handed-off session = %+v", child)
	}

	if _, err := a.StartSession(ctx, StartOpts{TaskID: research.ID, Prompt: "x", Agent: "codex"}); err == nil {
		t.Fatal("unknown agent should fail")
	}
	if c, _ := a.StartSession(ctx, StartOpts{TaskID: research.ID, Prompt: "x"}); c.Agent != agent.Claude {
		t.Fatalf("default agent = %q", c.Agent)
	}
	a.Wait()
}

// With gemini as the default agent nothing runs on claude: sessions that name
// no agent (as rules and schedules start them), their summaries and memories.
func TestGeminiAsDefaultAgent(t *testing.T) {
	a, r := newApp(t)
	a.DefaultAgent = agent.Gemini
	ctx := context.Background()
	research, _ := a.Store.CreateTask(ctx, store.Task{Title: "Research", Workdir: "/repo"})
	a.Store.CreateTask(ctx, store.Task{Title: "Build", ContextFrom: []int64{research.ID}})
	m, _ := a.Store.CreateMemory(ctx, store.Memory{Name: "Notes", Body: "old"})
	a.Store.SetTaskMemories(ctx, research.ID, []int64{m.ID})

	sess, err := a.StartSession(ctx, StartOpts{TaskID: research.ID, Prompt: "look"})
	if err != nil {
		t.Fatal(err)
	}
	a.Wait()
	if got, _ := a.Store.Session(ctx, sess.ID); got.Agent != agent.Gemini {
		t.Fatalf("session = %+v", got)
	}
	if err := a.UpdateMemories(ctx, sess.ID); err != nil {
		t.Fatal(err)
	}
	a.Wait()
	reqs := r.requests()
	if len(reqs) < 3 {
		t.Fatalf("want the turn, a summary and a memory update: %+v", reqs)
	}
	for _, req := range reqs {
		if req.Agent != agent.Gemini || req.Model == SummaryModel {
			t.Fatalf("request not on gemini: %+v", req)
		}
	}
	if c, _ := a.StartSession(ctx, StartOpts{TaskID: research.ID, Prompt: "x", Agent: agent.Claude}); c.Agent != agent.Claude {
		t.Fatalf("an explicit agent wins: %q", c.Agent)
	}
	a.Wait()
}

// Every turn carries the settings' session prompt, rendered for the session's
// project and task, so an agent working in another repo knows its app.
func TestSessionPrompt(t *testing.T) {
	a, r := newApp(t)
	ctx := context.Background()
	p, _ := a.Store.CreateProject(ctx, store.Project{Name: "Fantastica", Workdir: "/fantastica"})
	task, _ := a.Store.CreateTask(ctx, store.Task{Title: "Ship it", ProjectID: p.ID})
	set, _ := a.Store.Settings(ctx)
	if !strings.Contains(set.SessionPrompt, "{{.Project.ID}}") {
		t.Fatalf("default session prompt = %q", set.SessionPrompt)
	}
	set.SessionPrompt = "Project {{.Project.Name}} ({{.Project.ID}}){{if .Task.ID}}, task #{{.Task.ID}} {{.Task.Title}}{{end}}, session #{{.Session.ID}} in {{.Session.Workdir}}"
	a.Store.SaveSettings(ctx, set)

	sess, err := a.StartSession(ctx, StartOpts{TaskID: task.ID, Prompt: "go"})
	if err != nil {
		t.Fatal(err)
	}
	a.Wait()
	a.Store.UpdateProject(ctx, store.Project{ID: p.ID, Name: "Fantastica 2", Workdir: "/fantastica"})
	a.SendMessage(ctx, sess.ID, "more")
	a.Wait()
	standalone, _ := a.StartSession(ctx, StartOpts{Prompt: "hi", Workdir: "/x"})
	a.Wait()
	set.SessionPrompt = "  "
	a.Store.SaveSettings(ctx, set)
	a.StartSession(ctx, StartOpts{Prompt: "quiet"})
	a.Wait()

	reqs := r.requests()
	// A session on a task is told to keep its activity thread.
	activity := "\n\n" + fmt.Sprintf(ActivityNote, task.ID, sess.ID)
	want := []string{
		fmt.Sprintf("Project Fantastica (%d), task #%d Ship it, session #%d in /fantastica", p.ID, task.ID, sess.ID) + activity,
		// Rendered each turn, so a rename shows on the next one.
		fmt.Sprintf("Project Fantastica 2 (%d), task #%d Ship it, session #%d in /fantastica", p.ID, task.ID, sess.ID) + activity,
		// No project: the first one, where tasks go by default.
		fmt.Sprintf("Project Assistant (1), session #%d in /x", standalone.ID),
		"",
	}
	if len(reqs) != len(want) {
		t.Fatalf("requests = %+v", reqs)
	}
	for i, w := range want {
		if reqs[i].SystemPrompt != w {
			t.Errorf("turn %d system prompt = %q, want %q", i, reqs[i].SystemPrompt, w)
		}
	}
	// The transcript shows what the user sent, not the system prompt.
	if msgs, _ := a.Store.Messages(ctx, sess.ID, 0); msgs[0].Content != "go" {
		t.Fatalf("first message = %q", msgs[0].Content)
	}
}

func TestMoveRuleMovesTaskAfterTurn(t *testing.T) {
	ctx := context.Background()
	setup := func(t *testing.T) (*App, *fakeRunner, map[string]int64, store.Task) {
		a, r := newApp(t)
		lanes := lanesByName(t, a)
		wip, review := lanes["In progress"], lanes["Review"]
		task, _ := a.Store.CreateTask(ctx, store.Task{Title: "T", LaneID: lanes["Todo"]})
		a.Store.SaveRule(ctx, store.Rule{Name: "Then review", ToLaneID: wip, DoneLaneID: &review, Enabled: true})
		return a, r, lanes, task
	}

	t.Run("turn after the move", func(t *testing.T) {
		a, r, lanes, task := setup(t)
		if started, err := a.MoveTask(ctx, task.ID, lanes["In progress"], 0); err != nil || len(started) != 0 {
			t.Fatalf("a move rule starts nothing: %+v, %v", started, err)
		}
		if len(r.requests()) != 0 {
			t.Fatal("a move rule ran an agent")
		}
		a.StartSession(ctx, StartOpts{TaskID: task.ID, Prompt: "p"})
		a.Wait()
		if got, _ := a.Store.Task(ctx, task.ID); got.LaneID != lanes["Review"] {
			t.Fatalf("task in lane %d, want review", got.LaneID)
		}
		n, _ := a.Store.Notifications(ctx, true)
		if len(n) != 1 || !strings.Contains(n[0].Message, "moved to Review") {
			t.Fatalf("notifications = %+v", n)
		}
	})

	t.Run("not before it", func(t *testing.T) {
		a, _, lanes, task := setup(t)
		a.StartSession(ctx, StartOpts{TaskID: task.ID, Prompt: "p"})
		a.Wait()
		if got, _ := a.Store.Task(ctx, task.ID); got.LaneID != lanes["Todo"] {
			t.Fatal("task moved without entering the rule's lane")
		}
	})

	t.Run("failed turn", func(t *testing.T) {
		a, r, lanes, task := setup(t)
		a.MoveTask(ctx, task.ID, lanes["In progress"], 0)
		r.err = errors.New("boom")
		a.StartSession(ctx, StartOpts{TaskID: task.ID, Prompt: "p"})
		a.Wait()
		if got, _ := a.Store.Task(ctx, task.ID); got.LaneID != lanes["In progress"] {
			t.Fatal("failed turn must not move the task")
		}
	})

	t.Run("moved by hand meanwhile", func(t *testing.T) {
		a, r, lanes, task := setup(t)
		a.MoveTask(ctx, task.ID, lanes["In progress"], 0)
		r.block = make(chan struct{})
		a.StartSession(ctx, StartOpts{TaskID: task.ID, Prompt: "p"})
		a.MoveTask(ctx, task.ID, lanes["Done"], 0)
		close(r.block)
		a.Wait()
		if got, _ := a.Store.Task(ctx, task.ID); got.LaneID != lanes["Done"] {
			t.Fatal("a manual move must win")
		}
	})

	t.Run("session's done lane wins", func(t *testing.T) {
		a, _, lanes, task := setup(t)
		a.MoveTask(ctx, task.ID, lanes["In progress"], 0)
		done := lanes["Done"]
		a.StartSession(ctx, StartOpts{TaskID: task.ID, Prompt: "p", DoneLaneID: &done})
		a.Wait()
		if got, _ := a.Store.Task(ctx, task.ID); got.LaneID != done {
			t.Fatalf("task in lane %d, want done", got.LaneID)
		}
	})
}
