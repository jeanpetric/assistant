package app

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jeanpetric/assistant/internal/agent"
	"github.com/jeanpetric/assistant/internal/store"
)

// orchestrated starts an orchestrator session on its own task and lets its
// first turn finish, and makes the task a sub-session works on.
func orchestrated(t *testing.T, a *App) (orch store.Session, sub store.Task) {
	t.Helper()
	ctx := context.Background()
	ot, _ := a.Store.CreateTask(ctx, store.Task{Title: "Orchestrate"})
	orch, err := a.StartSession(ctx, StartOpts{TaskID: ot.ID, Prompt: "orchestrate", Title: "Orchestrator"})
	if err != nil {
		t.Fatal(err)
	}
	a.Wait()
	sub, _ = a.Store.CreateTask(ctx, store.Task{Title: "Fix login"})
	return orch, sub
}

// reports lists the prompts the orchestrator got after its first turn.
func reports(r *fakeRunner) []string {
	var out []string
	for _, q := range r.requests() {
		if strings.HasPrefix(q.Prompt, "Session #") {
			out = append(out, q.Prompt)
		}
	}
	return out
}

func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for end := time.Now().Add(5 * time.Second); !ok(); {
		if time.Now().After(end) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// A finished turn is reported as the orchestrator's next turn, with the
// session's final reply. The report answers none of the orchestrator's own
// questions: it is not the user's answer.
func TestReportFinished(t *testing.T) {
	a, r := newApp(t)
	ctx := context.Background()
	orch, task := orchestrated(t, a)
	if _, err := a.PostTaskMessage(ctx, store.TaskMessage{TaskID: orch.TaskID, SessionID: &orch.ID, Body: "Ship it?", Question: true}, 0); err != nil {
		t.Fatal(err)
	}
	r.result = func(req agent.Request) string {
		if req.Prompt == "fix it" {
			return "Fixed in PR #12."
		}
		return "ok"
	}
	sub, err := a.StartSession(ctx, StartOpts{TaskID: task.ID, Prompt: "fix it", Title: "Login fix", ReportTo: &orch.ID})
	if err != nil {
		t.Fatal(err)
	}
	if sub.ReportTo == nil || *sub.ReportTo != orch.ID {
		t.Fatalf("session = %+v", sub)
	}
	a.Wait()
	reqs := r.requests()
	want := fmt.Sprintf("Session #%d \"Login fix\" (task #%d \"Fix login\") finished:\n\nFixed in PR #12.", sub.ID, task.ID)
	if len(reqs) != 3 || reqs[2].Prompt != want || reqs[2].ResumeID != "claude-1" {
		t.Fatalf("requests = %+v", reqs)
	}
	if msgs, _ := a.Store.Messages(ctx, orch.ID, 0); msgs[len(msgs)-1].Content != "ok" {
		t.Fatalf("the report did not run in the orchestrator: %+v", msgs)
	}
	if open, _ := a.Store.HasOpenQuestion(ctx, orch.ID); !open {
		t.Fatal("the report answered the orchestrator's question")
	}
	// A session without report_to reports nothing; a standalone one's head
	// names no task.
	a.StartSession(ctx, StartOpts{TaskID: task.ID, Prompt: "quiet"})
	solo, _ := a.StartSession(ctx, StartOpts{Prompt: "chat", Title: "Chat", ReportTo: &orch.ID})
	a.Wait()
	if got := reports(r); len(got) != 2 || got[1] != fmt.Sprintf("Session #%d \"Chat\" finished:\n\nok", solo.ID) {
		t.Fatalf("reports = %q", got)
	}
}

// While the orchestrator works, the report waits in its queue for its turn.
func TestReportQueuedWhileOrchestratorBusy(t *testing.T) {
	a, r := newApp(t)
	ctx := context.Background()
	orch, task := orchestrated(t, a)
	busy := make(chan struct{})
	r.result = func(req agent.Request) string {
		if req.Prompt == "keep going" {
			<-busy
		}
		return "ok"
	}
	if _, err := a.SendMessage(ctx, orch.ID, "keep going"); err != nil {
		t.Fatal(err)
	}
	sub, _ := a.StartSession(ctx, StartOpts{TaskID: task.ID, Prompt: "fix it", Title: "Login fix", ReportTo: &orch.ID})
	eventually(t, "the report to be queued", func() bool {
		o, _ := a.Store.Session(ctx, orch.ID)
		return len(o.Queued) == 1
	})
	o, _ := a.Store.Session(ctx, orch.ID)
	if o.Status != store.StatusRunning || !strings.HasPrefix(o.Queued[0].Content, fmt.Sprintf("Session #%d ", sub.ID)) {
		t.Fatalf("orchestrator = %+v", o)
	}
	close(busy)
	a.Wait()
	if got := reports(r); len(got) != 1 || r.requests()[len(r.requests())-1].Prompt != got[0] {
		t.Fatalf("reports = %q", got)
	}
	if o, _ = a.Store.Session(ctx, orch.ID); len(o.Queued) != 0 {
		t.Fatalf("queue = %+v", o.Queued)
	}
}

// A failed turn is reported with its error.
func TestReportFailed(t *testing.T) {
	a, r := newApp(t)
	ctx := context.Background()
	orch, task := orchestrated(t, a)
	r.failOn = "boom"
	sub, _ := a.StartSession(ctx, StartOpts{TaskID: task.ID, Prompt: "boom", Title: "Login fix", ReportTo: &orch.ID})
	a.Wait()
	want := fmt.Sprintf("Session #%d \"Login fix\" (task #%d \"Fix login\") failed:\n\nfailed: boom", sub.ID, task.ID)
	if got := reports(r); len(got) != 1 || got[0] != want {
		t.Fatalf("reports = %q", got)
	}
}

// A session that asked on its task's thread reports the question; the
// orchestrator's answer through send_message answers it.
func TestReportAsks(t *testing.T) {
	a, r := newApp(t)
	ctx := context.Background()
	orch, task := orchestrated(t, a)
	r.result = func(req agent.Request) string {
		if req.Prompt == "fix it" {
			ss, _ := a.Store.Sessions(ctx, task.ID, "")
			if _, err := a.PostTaskMessage(ctx, store.TaskMessage{TaskID: task.ID, SessionID: &ss[0].ID, Body: "Which DB?", Question: true}, 0); err != nil {
				t.Error(err)
			}
			return "I asked on the thread."
		}
		return "ok"
	}
	sub, _ := a.StartSession(ctx, StartOpts{TaskID: task.ID, Prompt: "fix it", Title: "Login fix", ReportTo: &orch.ID})
	a.Wait()
	want := fmt.Sprintf("Session #%d \"Login fix\" (task #%d \"Fix login\") asks:\n\nWhich DB?\n\nAnswer it with send_message to session %d.",
		sub.ID, task.ID, sub.ID)
	if got := reports(r); len(got) != 1 || got[0] != want {
		t.Fatalf("reports = %q", got)
	}
	if _, err := a.SendMessage(ctx, sub.ID, "Postgres"); err != nil {
		t.Fatal(err)
	}
	a.Wait()
	if open, _ := a.Store.HasOpenQuestion(ctx, sub.ID); open {
		t.Fatal("the answer left the question open")
	}
	if got := reports(r); len(got) != 2 || !strings.Contains(got[1], "finished:\n\nok") {
		t.Fatalf("reports = %q", got)
	}
}

// A stopped turn is reported briefly; one cut short by a shutdown is not.
func TestReportCancelledNotInterrupted(t *testing.T) {
	a, r := newApp(t)
	ctx := context.Background()
	orch, task := orchestrated(t, a)
	r.block = make(chan struct{})
	sub, _ := a.StartSession(ctx, StartOpts{TaskID: task.ID, Prompt: "fix it", Title: "Login fix", ReportTo: &orch.ID})
	if err := a.Cancel(ctx, sub.ID); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the report", func() bool { return len(reports(r)) == 1 })
	close(r.block)
	a.Wait()
	if got := reports(r); got[0] != fmt.Sprintf("Session #%d \"Login fix\" (task #%d \"Fix login\") was cancelled.", sub.ID, task.ID) {
		t.Fatalf("reports = %q", got)
	}

	r.block = make(chan struct{})
	a.StartSession(ctx, StartOpts{TaskID: task.ID, Prompt: "again", ReportTo: &orch.ID})
	a.Shutdown()
	if got := reports(r); len(got) != 1 {
		t.Fatalf("an interrupted turn reported: %q", got)
	}
	if ns, _ := a.Store.Notifications(ctx, false); len(ns) != 0 {
		t.Fatalf("notifications = %+v", ns)
	}
}

// Skills that follow a turn run first; the report comes once, after the last.
func TestReportOnceAfterSkills(t *testing.T) {
	a, r := newApp(t)
	ctx := context.Background()
	orch, task := orchestrated(t, a)
	a.Store.SaveSkill(ctx, store.Skill{Name: "Notes", Body: "Note it.", Automations: []store.SkillAutomation{{Enabled: true}}})
	r.result = func(req agent.Request) string {
		if strings.HasPrefix(req.Prompt, "Apply the skill") {
			return "Notes written."
		}
		return "ok"
	}
	sub, _ := a.StartSession(ctx, StartOpts{TaskID: task.ID, Prompt: "fix it", Title: "Login fix", ReportTo: &orch.ID})
	a.Wait()
	reqs := r.requests()
	// orchestrator, the turn, the skill, the report; the report's own skill turn
	// in the orchestrator (no report_to) reports nothing.
	if len(reqs) < 4 || !strings.HasPrefix(reqs[2].Prompt, `Apply the skill "Notes"`) {
		t.Fatalf("requests = %+v", reqs)
	}
	want := fmt.Sprintf("Session #%d \"Login fix\" (task #%d \"Fix login\") finished:\n\nNotes written.", sub.ID, task.ID)
	if got := reports(r); len(got) != 1 || got[0] != want || reqs[3].Prompt != want {
		t.Fatalf("reports = %q", got)
	}
}

// A queued message runs before the report, which comes after its turn.
func TestReportAfterQueuedMessage(t *testing.T) {
	a, r := newApp(t)
	ctx := context.Background()
	orch, task := orchestrated(t, a)
	r.block = make(chan struct{})
	sub, _ := a.StartSession(ctx, StartOpts{TaskID: task.ID, Prompt: "fix it", ReportTo: &orch.ID})
	a.SendMessage(ctx, sub.ID, "and test it")
	close(r.block)
	a.Wait()
	reqs := r.requests()
	if got := reports(r); len(got) != 1 || len(reqs) != 4 || reqs[2].Prompt != "and test it" || reqs[3].Prompt != got[0] {
		t.Fatalf("requests = %+v", reqs)
	}
}

// A report to a session that was deleted or runs on another node becomes a
// notification on the reporting session.
func TestReportUndeliverableNotifies(t *testing.T) {
	a, r := newApp(t)
	ctx := context.Background()
	orch, task := orchestrated(t, a)
	makeForeign(t, a.Store, orch)
	sub, _ := a.StartSession(ctx, StartOpts{TaskID: task.ID, Prompt: "fix it", Title: "Login fix", ReportTo: &orch.ID})
	a.Wait()
	if got := reports(r); len(got) != 0 {
		t.Fatalf("reports = %q", got)
	}
	ns, _ := a.Store.Notifications(ctx, false)
	if len(ns) != 1 || ns[0].SessionID == nil || *ns[0].SessionID != sub.ID ||
		!strings.Contains(ns[0].Message, fmt.Sprintf("could not report to session #%d (session runs on office-mac", orch.ID)) ||
		!strings.Contains(ns[0].Message, "finished:\n\nok") {
		t.Fatalf("notifications = %+v", ns)
	}

	if err := a.Store.DeleteTask(ctx, orch.TaskID); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Store.Session(ctx, orch.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("orchestrator not deleted: %v", err)
	}
	if _, err := a.SendMessage(ctx, sub.ID, "more"); err != nil {
		t.Fatal(err)
	}
	a.Wait()
	ns, _ = a.Store.Notifications(ctx, false)
	if len(ns) != 2 || !slicesContainsMsg(ns, fmt.Sprintf("could not report to session #%d (it was deleted)", orch.ID)) {
		t.Fatalf("notifications = %+v", ns)
	}
}

func slicesContainsMsg(ns []store.Notification, part string) bool {
	for _, n := range ns {
		if strings.Contains(n.Message, part) {
			return true
		}
	}
	return false
}

// A session handed off reports nothing itself: the one it hands off to
// reports to the same session.
func TestReportAfterHandoff(t *testing.T) {
	a, r := newApp(t)
	ctx := context.Background()
	orch, task := orchestrated(t, a)
	sub, _ := a.StartSession(ctx, StartOpts{TaskID: task.ID, Prompt: "fix it", Title: "Login fix", ReportTo: &orch.ID})
	a.Wait()
	if err := a.Handoff(ctx, sub.ID, ""); err != nil {
		t.Fatal(err)
	}
	a.Wait()
	old, _ := a.Store.Session(ctx, sub.ID)
	child, _ := a.Store.Session(ctx, *old.ChildID)
	if child.ReportTo == nil || *child.ReportTo != orch.ID {
		t.Fatalf("child = %+v", child)
	}
	got := reports(r)
	if len(got) != 2 || !strings.HasPrefix(got[1], fmt.Sprintf("Session #%d ", child.ID)) {
		t.Fatalf("reports = %q", got)
	}
}

func TestStartSessionRejectsUnknownReportTo(t *testing.T) {
	a, r := newApp(t)
	ctx := context.Background()
	if _, err := a.StartSession(ctx, StartOpts{Prompt: "p", ReportTo: ptr(999)}); !errors.Is(err, ErrBadReportTo) {
		t.Fatalf("err = %v", err)
	}
	if ss, _ := a.Store.Sessions(ctx, 0, ""); len(ss) != 0 || len(r.requests()) != 0 {
		t.Fatalf("sessions = %+v", ss)
	}
}
