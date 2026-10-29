package store

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// A task's thread lists its messages oldest first, each from its session (by
// title) or the user; the latest open question is the one an answer goes to.
func TestTaskMessages(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	task, _ := s.CreateTask(ctx, Task{Title: "t"})
	other, _ := s.CreateTask(ctx, Task{Title: "other"})
	sess, _ := s.CreateSession(ctx, Session{TaskID: task.ID, Title: "Spec review", Mode: "plan"})

	if _, err := s.OpenQuestion(ctx, task.ID); err != ErrNotFound {
		t.Fatalf("open question on an empty thread: %v", err)
	}
	add := func(x TaskMessage) TaskMessage {
		t.Helper()
		x, err := s.AddTaskMessage(ctx, x)
		if err != nil {
			t.Fatal(err)
		}
		return x
	}
	note := add(TaskMessage{TaskID: task.ID, SessionID: &sess.ID, Body: "Spec rejected."})
	q1 := add(TaskMessage{TaskID: task.ID, SessionID: &sess.ID, Body: "Show it?", Question: true})
	q2 := add(TaskMessage{TaskID: task.ID, SessionID: &sess.ID, Body: "Or refuse?", Question: true})
	mine := add(TaskMessage{TaskID: task.ID, Body: "hi"})
	add(TaskMessage{TaskID: other.ID, Body: "elsewhere"})
	if note.SessionTitle != "Spec review" || note.Question || mine.SessionID != nil || mine.SessionTitle != "" || q1.AnsweredAt != nil {
		t.Fatalf("note = %+v, mine = %+v, q1 = %+v", note, mine, q1)
	}

	xs, err := s.TaskMessages(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	var bodies []string
	for _, x := range xs {
		bodies = append(bodies, x.Body)
	}
	if !slices.Equal(bodies, []string{"Spec rejected.", "Show it?", "Or refuse?", "hi"}) {
		t.Fatalf("thread = %v", bodies)
	}
	if q, err := s.OpenQuestion(ctx, task.ID); err != nil || q.ID != q2.ID {
		t.Fatalf("open question = %+v, %v", q, err)
	}
	if open, _ := s.HasOpenQuestion(ctx, sess.ID); !open {
		t.Fatal("session's questions not open")
	}
	s.SetQuestionAnswered(ctx, q2.ID, true)
	if q, err := s.OpenQuestion(ctx, task.ID); err != nil || q.ID != q1.ID {
		t.Fatalf("open question = %+v, %v", q, err)
	}
	if x, _ := s.TaskMessage(ctx, q2.ID); x.AnsweredAt == nil {
		t.Fatal("answered question has no answered_at")
	}
	s.SetQuestionAnswered(ctx, q1.ID, true)
	if open, _ := s.HasOpenQuestion(ctx, sess.ID); open {
		t.Fatal("answered questions still open")
	}
	s.SetQuestionAnswered(ctx, q1.ID, false)
	if q, err := s.OpenQuestion(ctx, task.ID); err != nil || q.ID != q1.ID || q.AnsweredAt != nil {
		t.Fatalf("reopened question = %+v, %v", q, err)
	}
	if err := s.SetQuestionAnswered(ctx, note.ID, true); err != ErrNotFound {
		t.Fatalf("answering a plain message: %v", err)
	}

	// A deleted session leaves its messages without a link; a deleted task
	// takes its thread along.
	exec(t, s, fmt.Sprintf(`DELETE FROM sessions WHERE id = %d`, sess.ID))
	if x, err := s.TaskMessage(ctx, note.ID); err != nil || x.SessionID != nil || x.SessionTitle != "" || x.Body != "Spec rejected." {
		t.Fatalf("message of a deleted session = %+v, %v", x, err)
	}
	if err := s.DeleteTask(ctx, task.ID); err != nil {
		t.Fatal(err)
	}
	var n int
	s.db.QueryRow(`SELECT COUNT(*) FROM task_messages`).Scan(&n)
	if n != 1 {
		t.Fatalf("%d messages left, want the other task's", n)
	}
}

// The thread is synced like the task's other rows.
func TestTaskMessagesSync(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	task, _ := s.CreateTask(ctx, Task{Title: "t"})
	s.SetSyncEnabled(ctx, true)
	q, _ := s.AddTaskMessage(ctx, TaskMessage{TaskID: task.ID, Body: "?", Question: true})
	s.SetQuestionAnswered(ctx, q.ID, true)
	var got []string
	for _, x := range outbox(t, s) {
		if strings.Contains(x, "task_messages") {
			got = append(got, x)
		}
	}
	pk := fmt.Sprintf("[%d]", q.ID)
	if want := []string{"insert task_messages " + pk, "update task_messages " + pk}; !slices.Equal(got, want) {
		t.Fatalf("outbox = %v, want %v", got, want)
	}
}

// Upgrading starts every task with an empty thread, its data kept.
func TestTaskMessagesMigrationKeepsData(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	n := migrationWith(t, "CREATE TABLE task_messages")
	for _, m := range migrations[:n] {
		if _, err := db.Exec(m); err != nil {
			t.Fatal(err)
		}
	}
	for _, q := range []string{
		fmt.Sprintf(`PRAGMA user_version = %d`, n),
		`INSERT INTO tasks (id, title, lane_id, position, project_id, created_at, updated_at) VALUES (5, 'Ship it', 1, 0, 1, 0, 0)`,
		`INSERT INTO sessions (id, task_id, title, status, mode, created_at, updated_at, needs_input) VALUES (7, 5, 'Guard', 'idle', 'plan', 0, 0, 'asked a question')`,
		`INSERT INTO messages (id, session_id, role, kind, content, created_at) VALUES (3, 7, 'assistant', 'text', 'Which one?', 0)`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(q, err)
		}
	}
	db.Close()

	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	if task, err := s.Task(ctx, 5); err != nil || task.Title != "Ship it" {
		t.Fatalf("task = %+v, %v", task, err)
	}
	if sess, err := s.Session(ctx, 7); err != nil || sess.Title != "Guard" || sess.NeedsInput != "asked a question" {
		t.Fatalf("session = %+v, %v", sess, err)
	}
	if msgs, err := s.Messages(ctx, 7, 0); err != nil || len(msgs) != 1 || msgs[0].Content != "Which one?" {
		t.Fatalf("messages = %+v, %v", msgs, err)
	}
	if xs, err := s.TaskMessages(ctx, 5); err != nil || len(xs) != 0 {
		t.Fatalf("thread = %+v, %v", xs, err)
	}
	sid := int64(7)
	if x, err := s.AddTaskMessage(ctx, TaskMessage{TaskID: 5, SessionID: &sid, Body: "Done."}); err != nil || x.SessionTitle != "Guard" {
		t.Fatalf("message = %+v, %v", x, err)
	}
}
