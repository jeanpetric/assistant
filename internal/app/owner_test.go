package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jeanpetric/assistant/internal/store"
)

// makeForeign hands the session to node 2, as a change synced from there would.
func makeForeign(t *testing.T, s *store.Store, sess store.Session) {
	t.Helper()
	row, _ := json.Marshal(map[string]any{"id": sess.ID, "status": sess.Status, "mode": sess.Mode,
		"created_at": sess.CreatedAt.UnixMilli(), "updated_at": sess.UpdatedAt.UnixMilli(), "node_id": 2, "node_name": "office-mac"})
	err := s.ApplyChanges(context.Background(), []store.Change{{Table: "sessions", Key: json.RawMessage(fmt.Sprintf("[%d]", sess.ID)),
		ChangedAtMs: time.Now().Add(time.Hour).UnixMilli(), NodeID: 2, Row: row}})
	if err != nil {
		t.Fatal(err)
	}
}

// A session another node runs goes on only there: every action that would
// run its agent is refused here, naming that node, and changes nothing.
func TestForeignSessionActionsRefused(t *testing.T) {
	a, r := newApp(t)
	ctx := context.Background()
	task, _ := a.Store.CreateTask(ctx, store.Task{Title: "Remote"})
	sess, err := a.StartSession(ctx, StartOpts{TaskID: task.ID, Prompt: "go"})
	if err != nil {
		t.Fatal(err)
	}
	a.Wait()
	sk, _ := a.Store.SaveSkill(ctx, store.Skill{Name: "Guard", Body: "Guard it."})
	sess, _ = a.Store.Session(ctx, sess.ID)
	makeForeign(t, a.Store, sess)
	q, _ := a.Store.QueueMessage(ctx, sess.ID, "later")
	before, _ := a.Store.Session(ctx, sess.ID)
	if before.Owned {
		t.Fatal("still owned")
	}
	n := len(r.requests())

	for name, err := range map[string]error{
		"resume":  a.Resume(ctx, sess.ID),
		"cancel":  a.Cancel(ctx, sess.ID),
		"handoff": a.Handoff(ctx, sess.ID, ""),
		"skill":   a.RunSkill(ctx, sess.ID, sk.ID),
		"queued":  a.DeleteQueuedMessage(ctx, q.ID),
		"message": func() error { _, err := a.SendMessage(ctx, sess.ID, "more"); return err }(),
	} {
		if !errors.Is(err, ErrForeignSession) || err.Error() != "session runs on office-mac; open it there" {
			t.Errorf("%s: %v", name, err)
		}
	}
	a.Wait()
	if len(r.requests()) != n {
		t.Fatalf("agent ran: %+v", r.requests()[n:])
	}
	after, _ := a.Store.Session(ctx, sess.ID)
	if after.Status != before.Status || len(after.Queued) != 1 || after.Queued[0].ID != q.ID {
		t.Fatalf("session = %+v", after)
	}
}

// The user's answer to a question another node's session asked is refused:
// the question stays open and the thread as it was.
func TestAnswerToForeignSessionRefused(t *testing.T) {
	a, r := newApp(t)
	ctx := context.Background()
	task, _ := a.Store.CreateTask(ctx, store.Task{Title: "Remote"})
	sess, _ := a.StartSession(ctx, StartOpts{TaskID: task.ID, Prompt: "go"})
	a.Wait()
	if _, err := a.PostTaskMessage(ctx, store.TaskMessage{TaskID: task.ID, SessionID: &sess.ID, Body: "Which one?", Question: true}, 0); err != nil {
		t.Fatal(err)
	}
	sess, _ = a.Store.Session(ctx, sess.ID)
	makeForeign(t, a.Store, sess)
	n := len(r.requests())

	_, err := a.PostTaskMessage(ctx, store.TaskMessage{TaskID: task.ID, Body: "The first"}, 0)
	if !errors.Is(err, ErrForeignSession) {
		t.Fatalf("answer: %v", err)
	}
	a.Wait()
	thread, _ := a.Store.TaskMessages(ctx, task.ID)
	if len(thread) != 1 || thread[0].AnsweredAt != nil || len(r.requests()) != n {
		t.Fatalf("thread = %+v, requests %d", thread, len(r.requests())-n)
	}
	if got, _ := a.Store.Session(ctx, sess.ID); got.NeedsInput == "" {
		t.Fatal("needs input cleared")
	}
}

// A skill rule whose task's latest session runs on another node starts a new
// session here, as if that one were busy.
func TestRuleSkillWithForeignLatestStartsNewSession(t *testing.T) {
	a, r := newApp(t)
	ctx := context.Background()
	lanes := lanesByName(t, a)
	wip, review := lanes["In progress"], lanes["Review"]
	task, _ := a.Store.CreateTask(ctx, store.Task{Title: "Remote", LaneID: wip})
	sk, _ := a.Store.SaveSkill(ctx, store.Skill{Name: "Guard", Body: "Guard it."})
	a.Store.SaveRule(ctx, store.Rule{Name: "Guard it", ToLaneID: review, SkillID: &sk.ID, Enabled: true})
	first, _ := a.StartSession(ctx, StartOpts{TaskID: task.ID, Prompt: "code"})
	a.Wait()
	first, _ = a.Store.Session(ctx, first.ID)
	makeForeign(t, a.Store, first)

	started, err := a.MoveTask(ctx, task.ID, review, 0)
	if err != nil || len(started) != 1 || started[0].ID == first.ID || !started[0].Owned {
		t.Fatalf("started = %+v, %v", started, err)
	}
	a.Wait()
	reqs := r.requests()
	if last := reqs[len(reqs)-1]; last.ResumeID != "" || last.Prompt == "" {
		t.Fatalf("skill request = %+v", last)
	}
}

// Another node's session is summed up from its stored transcript: its agent
// conversation lives there, so there is nothing here to fork.
func TestForeignSessionSummaryFromTranscript(t *testing.T) {
	a, r := newApp(t)
	ctx := context.Background()
	task, _ := a.Store.CreateTask(ctx, store.Task{Title: "Remote"})
	sess, _ := a.StartSession(ctx, StartOpts{TaskID: task.ID, Prompt: "go"})
	a.Wait()
	sess, _ = a.Store.Session(ctx, sess.ID)
	makeForeign(t, a.Store, sess)
	n := len(r.requests())
	if err := a.Summarize(ctx, sess.ID); err != nil {
		t.Fatal(err)
	}
	a.Wait()
	reqs := r.requests()[n:]
	if len(reqs) != 1 || reqs[0].ResumeID != "" || reqs[0].Fork {
		t.Fatalf("summary requests = %+v", reqs)
	}
	if got, _ := a.Store.Session(ctx, sess.ID); got.Summary == "" {
		t.Fatal("no summary")
	}
}
