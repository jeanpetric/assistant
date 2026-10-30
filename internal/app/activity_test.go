package app

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/jeanpetric/assistant/internal/agent"
	"github.com/jeanpetric/assistant/internal/store"
)

// An agent's question on the thread flags its session and notifies the user
// once; the task stays put until the answer, which continues the session.
func TestQuestionAndAnswer(t *testing.T) {
	a, r := newApp(t)
	ctx := context.Background()
	lanes := lanesByName(t, a)
	task, _ := a.Store.CreateTask(ctx, store.Task{Title: "Spec", LaneID: lanes["Todo"]})
	done := lanes["Done"]
	var asked store.TaskMessage
	r.fail = func(req agent.Request) error {
		if req.Prompt == "refine" {
			sid := sessionOf(t, a, task.ID)
			a.PostTaskMessage(ctx, store.TaskMessage{TaskID: task.ID, SessionID: &sid, Body: "Filled in acceptance."}, PostOpts{})
			x, err := a.PostTaskMessage(ctx, store.TaskMessage{TaskID: task.ID, SessionID: &sid, Body: "Show the lane or refuse the move?", Question: true}, PostOpts{})
			if err != nil {
				t.Error(err)
			}
			asked = x.TaskMessage
		}
		return nil
	}
	r.result = func(req agent.Request) string {
		if req.Prompt == "refine" {
			return "Which one?" // asked in the reply too: still one notification
		}
		return "ok"
	}
	sess, err := a.StartSession(ctx, StartOpts{TaskID: task.ID, Title: "Spec refine", Prompt: "refine", DoneLaneID: &done})
	if err != nil {
		t.Fatal(err)
	}
	a.Wait()

	sess, _ = a.Store.Session(ctx, sess.ID)
	if sess.NeedsInput != "asked a question on the task" {
		t.Fatalf("needs_input = %q", sess.NeedsInput)
	}
	ns, _ := a.Store.Notifications(ctx, false)
	if len(ns) != 1 || ns[0].Message != `Question from "Spec refine": Show the lane or refuse the move?` || ns[0].SessionID == nil || *ns[0].SessionID != sess.ID {
		t.Fatalf("notifications = %+v", ns)
	}
	if got, _ := a.Store.Task(ctx, task.ID); got.LaneID != lanes["Todo"] {
		t.Fatalf("task moved to lane %d while its question waits", got.LaneID)
	}

	x, err := a.PostTaskMessage(ctx, store.TaskMessage{TaskID: task.ID, Body: "  show it "}, PostOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if x.AnsweredID == nil || *x.AnsweredID != asked.ID || x.Queued || x.Body != "show it" || x.SessionID != nil {
		t.Fatalf("answer = %+v", x)
	}
	a.Wait()
	reqs := r.requests()
	if want := fmt.Sprintf(AnswerPrompt, "Show the lane or refuse the move?", "show it"); len(reqs) != 2 || reqs[1].Prompt != want || reqs[1].ResumeID != "claude-1" {
		t.Fatalf("requests = %+v", reqs)
	}
	sess, _ = a.Store.Session(ctx, sess.ID)
	q, _ := a.Store.TaskMessage(ctx, asked.ID)
	if sess.NeedsInput != "" || q.AnsweredAt == nil {
		t.Fatalf("needs_input = %q, question = %+v", sess.NeedsInput, q)
	}
	// The turn after the answer finishes the session's work.
	if got, _ := a.Store.Task(ctx, task.ID); got.LaneID != done {
		t.Fatalf("task in lane %d after the answer", got.LaneID)
	}
	if xs, _ := a.Store.TaskMessages(ctx, task.ID); len(xs) != 3 || xs[0].SessionTitle != "Spec refine" || xs[2].Body != "show it" {
		t.Fatalf("thread = %+v", xs)
	}
}

// sessionOf is the task's only session.
func sessionOf(t *testing.T, a *App, taskID int64) int64 {
	t.Helper()
	xs, err := a.Store.Sessions(context.Background(), taskID, "")
	if err != nil || len(xs) != 1 {
		t.Fatalf("sessions = %+v, %v", xs, err)
	}
	return xs[0].ID
}

// An answer to a session that is working waits in its queue.
func TestAnswerQueuedWhileRunning(t *testing.T) {
	a, r := newApp(t)
	ctx := context.Background()
	r.block = make(chan struct{})
	task, _ := a.Store.CreateTask(ctx, store.Task{Title: "t"})
	sess, _ := a.StartSession(ctx, StartOpts{TaskID: task.ID, Prompt: "go"})
	q, err := a.PostTaskMessage(ctx, store.TaskMessage{TaskID: task.ID, SessionID: &sess.ID, Body: "A or B?", Question: true}, PostOpts{})
	if err != nil {
		t.Fatal(err)
	}
	x, err := a.PostTaskMessage(ctx, store.TaskMessage{TaskID: task.ID, Body: "B"}, PostOpts{AnswerTo: q.ID})
	if err != nil || !x.Queued || *x.AnsweredID != q.ID {
		t.Fatalf("answer = %+v, %v", x, err)
	}
	got, _ := a.Store.Session(ctx, sess.ID)
	if got.NeedsInput != "" || len(got.Queued) != 1 {
		t.Fatalf("session = %+v", got)
	}
	close(r.block)
	a.Wait()
	if reqs := r.requests(); len(reqs) != 2 || reqs[1].Prompt != fmt.Sprintf(AnswerPrompt, "A or B?", "B") {
		t.Fatalf("requests = %+v", reqs)
	}
}

// The user's message with no open question only joins the thread; one that
// cannot be delivered leaves everything as it was.
func TestTaskMessageWithoutQuestion(t *testing.T) {
	a, r := newApp(t)
	ctx := context.Background()
	task, _ := a.Store.CreateTask(ctx, store.Task{Title: "t"})
	sess, _ := a.StartSession(ctx, StartOpts{TaskID: task.ID, Prompt: "go"})
	a.Wait()
	note, _ := a.PostTaskMessage(ctx, store.TaskMessage{TaskID: task.ID, SessionID: &sess.ID, Body: "Done."}, PostOpts{})
	x, err := a.PostTaskMessage(ctx, store.TaskMessage{TaskID: task.ID, Body: "thanks", Question: true}, PostOpts{})
	if err != nil || x.AnsweredID != nil || x.Question {
		t.Fatalf("message = %+v, %v", x, err)
	}
	a.Wait()
	if n := len(r.requests()); n != 1 {
		t.Fatalf("%d turns, want the first only", n)
	}
	if s, _ := a.Store.Session(ctx, sess.ID); s.NeedsInput != "" {
		t.Fatalf("plain message flagged the session: %q", s.NeedsInput)
	}

	for _, c := range []struct {
		m        store.TaskMessage
		answerTo int64
		want     error
	}{
		{store.TaskMessage{TaskID: task.ID, Body: "  "}, 0, ErrBadMessage},
		{store.TaskMessage{TaskID: task.ID + 99, Body: "x"}, 0, store.ErrNotFound},
		{store.TaskMessage{TaskID: task.ID, SessionID: ptr(sess.ID + 99), Body: "x"}, 0, ErrBadMessage},
		{store.TaskMessage{TaskID: task.ID, Body: "x"}, note.ID, ErrBadMessage},
		{store.TaskMessage{TaskID: task.ID, SessionID: &sess.ID, Body: "x"}, note.ID, ErrBadMessage},
	} {
		if _, err := a.PostTaskMessage(ctx, c.m, PostOpts{AnswerTo: c.answerTo}); !errors.Is(err, c.want) {
			t.Errorf("%+v answering %d: %v, want %v", c.m, c.answerTo, err, c.want)
		}
	}

	// A session with no agent session to resume cannot take an answer.
	stuck, _ := a.Store.CreateSession(ctx, store.Session{TaskID: task.ID, Title: "stuck", Mode: "plan", Status: store.StatusError})
	q, _ := a.PostTaskMessage(ctx, store.TaskMessage{TaskID: task.ID, SessionID: &stuck.ID, Body: "Which?", Question: true}, PostOpts{})
	if _, err := a.PostTaskMessage(ctx, store.TaskMessage{TaskID: task.ID, Body: "this"}, PostOpts{}); !errors.Is(err, ErrNoResume) {
		t.Fatalf("answer to a stuck session: %v", err)
	}
	if got, _ := a.Store.TaskMessage(ctx, q.ID); got.AnsweredAt != nil {
		t.Fatal("undelivered answer answered the question")
	}
	if s, _ := a.Store.Session(ctx, stuck.ID); s.NeedsInput != "asked a question on the task" {
		t.Fatalf("needs_input = %q", s.NeedsInput)
	}
	if xs, _ := a.Store.TaskMessages(ctx, task.ID); len(xs) != 3 || strings.Contains(xs[2].Body, "this") {
		t.Fatalf("thread = %+v", xs)
	}
}

// Replying in the session itself answers its question on the thread, so the
// user's next thread message does not go to it.
func TestSessionReplyAnswersQuestion(t *testing.T) {
	a, r := newApp(t)
	ctx := context.Background()
	task, _ := a.Store.CreateTask(ctx, store.Task{Title: "t"})
	sess, _ := a.StartSession(ctx, StartOpts{TaskID: task.ID, Prompt: "go"})
	a.Wait()
	q, _ := a.PostTaskMessage(ctx, store.TaskMessage{TaskID: task.ID, SessionID: &sess.ID, Body: "A or B?", Question: true}, PostOpts{})
	if _, err := a.SendMessage(ctx, sess.ID, "B"); err != nil {
		t.Fatal(err)
	}
	a.Wait()
	if got, _ := a.Store.TaskMessage(ctx, q.ID); got.AnsweredAt == nil {
		t.Fatal("question open after a reply in the session")
	}
	x, err := a.PostTaskMessage(ctx, store.TaskMessage{TaskID: task.ID, Body: "fyi"}, PostOpts{})
	a.Wait()
	if err != nil || x.AnsweredID != nil || len(r.requests()) != 2 {
		t.Fatalf("message = %+v, %v, %d turns", x, err, len(r.requests()))
	}
}
