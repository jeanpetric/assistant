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
	mu     sync.Mutex
	reqs   []agent.Request
	err    error
	block  chan struct{}     // if set, Run waits for close or ctx cancel
	limits *agent.RateLimits // if set, emitted as a rate_limit event
}

func (f *fakeRunner) Run(ctx context.Context, req agent.Request, emit func(agent.Event)) error {
	f.mu.Lock()
	f.reqs = append(f.reqs, req)
	f.mu.Unlock()
	emit(agent.Event{Role: "system", Kind: "init", ClaudeSessionID: "claude-1"})
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
	if got.Mode != "bypassPermissions" || got.Workdir != "/repo" || got.Title != "look around" {
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
	if err := a.SendMessage(ctx, sess.ID, "more"); err != nil {
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

func TestStandaloneSession(t *testing.T) {
	a, r := newApp(t)
	ctx := context.Background()
	sess, err := a.StartSession(ctx, StartOpts{Prompt: "hello", Model: "sonnet", Workdir: "/elsewhere"})
	if err != nil {
		t.Fatal(err)
	}
	a.Wait()
	if err := a.SendMessage(ctx, sess.ID, "more"); err != nil {
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
