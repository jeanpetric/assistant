package app

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jeanpetric/assistant/internal/agent"
	"github.com/jeanpetric/assistant/internal/store"
)

// fakeRunner records requests and replies with a canned transcript.
type fakeRunner struct {
	mu    sync.Mutex
	reqs  []agent.Request
	err   error
	block chan struct{} // if set, Run waits for close or ctx cancel
}

func (f *fakeRunner) Run(ctx context.Context, req agent.Request, emit func(agent.Event)) error {
	f.mu.Lock()
	f.reqs = append(f.reqs, req)
	f.mu.Unlock()
	emit(agent.Event{Role: "system", Kind: "init", ClaudeSessionID: "claude-1"})
	if f.block != nil {
		select {
		case <-f.block:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	emit(agent.Event{Role: "assistant", Kind: "text", Content: "echo: " + req.Prompt})
	emit(agent.Event{Role: "system", Kind: "result", Content: "ok", CostUSD: 0.5})
	return f.err
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
	if got.Mode != "bypassPermissions" || got.Workdir != "/repo" || got.Title != "look around" || got.Model != "" {
		t.Fatalf("defaults not applied: %+v", got)
	}

	if err := a.SendMessage(ctx, sess.ID, "now fix it"); err != nil {
		t.Fatal(err)
	}
	a.Wait()
	reqs := r.requests()
	if len(reqs) != 2 || reqs[1].ResumeID != "claude-1" || reqs[1].Prompt != "now fix it" {
		t.Fatalf("follow-up should resume: %+v", reqs)
	}
	if key := SessionKey(sess.ID); reqs[0].Key != key || reqs[1].Key != key {
		t.Fatalf("turns should share the session key %s: %+v", key, reqs)
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
	if err := a.SendMessage(ctx, sess.ID, "again"); !errors.Is(err, ErrBusy) {
		t.Fatalf("expected ErrBusy, got %v", err)
	}
	a.Cancel(sess.ID)
	a.Wait()
	got, _ = a.Store.Session(ctx, sess.ID)
	if got.Status != store.StatusCancelled {
		t.Fatalf("expected cancelled, got %+v", got)
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
	a.Wait()
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

	if _, err := a.SaveSchedule(ctx, store.Schedule{TaskID: task.ID, Cron: "bad", Action: "agent"}); err == nil {
		t.Fatal("expected cron error")
	}
	if _, err := a.SaveSchedule(ctx, store.Schedule{TaskID: task.ID, Cron: "* * * * *", Action: "dance"}); !errors.Is(err, ErrBadAction) {
		t.Fatal("expected bad action")
	}
	agentSched, err := a.SaveSchedule(ctx, store.Schedule{TaskID: task.ID, Cron: "0 9 * * *", Action: "agent",
		Prompt: "Summarise: {{.Context}}", Enabled: true})
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
	notes, _ := a.Store.Notifications(ctx, true)
	if len(notes) != 1 || notes[0].Message != "Reminder: Daily digest" {
		t.Fatalf("notes = %+v", notes)
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

func TestTaskContext(t *testing.T) {
	got := TaskContext(store.Task{ID: 7, Title: "T", Description: "desc",
		Fields: map[string]string{"slack": "s", "productive": "p"}})
	want := "Task #7: T\n\ndesc\n\nLinks:\n- productive: p\n- slack: s"
	if got != want {
		t.Fatalf("got %q", got)
	}
}

func TestSessionModel(t *testing.T) {
	a, r := newApp(t)
	a.DefaultModel = "sonnet"
	ctx := context.Background()
	task, _ := a.Store.CreateTask(ctx, store.Task{Title: "Pick a model"})

	def, _ := a.StartSession(ctx, StartOpts{TaskID: task.ID, Prompt: "a"})
	chosen, _ := a.StartSession(ctx, StartOpts{TaskID: task.ID, Prompt: "b", Model: "haiku"})
	a.Wait()
	if def.Model != "sonnet" || chosen.Model != "haiku" {
		t.Fatalf("models: %q %q", def.Model, chosen.Model)
	}
	if err := a.SendMessage(ctx, chosen.ID, "c"); err != nil {
		t.Fatal(err)
	}
	a.Wait()
	for _, req := range r.requests() {
		if want := map[string]string{"a": "sonnet", "b": "haiku", "c": "haiku"}[req.Prompt]; req.Model != want {
			t.Errorf("prompt %q ran with model %q, want %q", req.Prompt, req.Model, want)
		}
	}
	if a.AttachCommand(def.ID) != "" {
		t.Error("a runner without AttachCommand has nothing to attach")
	}
}
