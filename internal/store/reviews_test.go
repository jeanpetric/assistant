package store

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"slices"
	"testing"
)

// The reviews migration only adds its tables: a populated database keeps
// every row, and the new tables take reviews of its tasks.
func TestReviewsMigration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	n := migrationWith(t, "CREATE TABLE reviews")
	for _, m := range migrations[:n] {
		if _, err := db.Exec(m); err != nil {
			t.Fatal(err)
		}
	}
	for _, q := range []string{
		fmt.Sprintf(`PRAGMA user_version = %d`, n),
		`INSERT INTO tasks (id, title, lane_id, position, project_id, created_at, updated_at) VALUES (5, 'PR task', 1, 0, 1, 0, 0)`,
		`INSERT INTO task_fields (task_id, key, value) VALUES (5, 'pull_request', 'https://github.com/o/r/pull/1')`,
		`INSERT INTO sessions (id, task_id, title, mode, workdir, status, created_at, updated_at) VALUES (3, 5, 's', 'auto', '/tmp', 'idle', 0, 0)`,
		`INSERT INTO messages (id, session_id, role, kind, content, created_at) VALUES (1, 3, 'assistant', 'text', 'hi', 0)`,
		`INSERT INTO artifacts (id, task_id, session_id, content, created_at) VALUES (1, 5, 3, 'art', 0)`,
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
	task, err := s.Task(ctx, 5)
	if err != nil || task.Title != "PR task" || task.Fields["pull_request"] == "" {
		t.Fatalf("task = %+v, %v", task, err)
	}
	if msgs, _ := s.Messages(ctx, 3, 0); len(msgs) != 1 {
		t.Fatalf("messages = %+v", msgs)
	}
	if arts, _ := s.Artifacts(ctx, 5); len(arts) != 1 {
		t.Fatalf("artifacts = %+v", arts)
	}
	sid := int64(3)
	r, err := s.CreateReview(ctx, Review{TaskID: 5, SessionID: &sid})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddReviewQuestion(ctx, ReviewQuestion{ReviewID: r.ID, Path: "a.go", Side: "new", Line: 1, Question: "?"}); err != nil {
		t.Fatal(err)
	}
}

func TestReviewsCRUD(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	task, err := s.CreateTask(ctx, Task{Title: "t"})
	if err != nil {
		t.Fatal(err)
	}
	sess, err := s.CreateSession(ctx, Session{TaskID: task.ID, Title: "PR review"})
	if err != nil {
		t.Fatal(err)
	}
	r1, err := s.CreateReview(ctx, Review{TaskID: task.ID})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetReviewSession(ctx, r1.ID, sess.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveReviewNotes(ctx, r1.ID, "https://github.com/o/r/pull/1", "o/r#1: T", "abc", `{"summary":"s"}`, "diff --git a/x b/x"); err != nil {
		t.Fatal(err)
	}
	r2, err := s.CreateReview(ctx, Review{TaskID: task.ID})
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.Review(ctx, r1.ID)
	if err != nil || got.Title != "o/r#1: T" || got.HeadSHA != "abc" || got.Notes != `{"summary":"s"}` || got.Diff == "" || *got.SessionID != sess.ID {
		t.Fatalf("review = %+v, %v", got, err)
	}
	list, err := s.Reviews(ctx, task.ID)
	if err != nil || len(list) != 2 || list[0].ID != r2.ID || list[1].Diff != "" {
		t.Fatalf("reviews = %+v, %v", list, err)
	}
	if _, err := s.Review(ctx, 999); err != ErrNotFound {
		t.Fatalf("missing review: %v", err)
	}
	if err := s.SaveReviewNotes(ctx, 999, "", "", "", "", ""); err != ErrNotFound {
		t.Fatalf("save missing: %v", err)
	}

	// Questions are answered oldest first, by the session that makes the review.
	q1, _ := s.AddReviewQuestion(ctx, ReviewQuestion{ReviewID: r1.ID, Path: "x", Side: "new", Line: 3, Question: "a?"})
	q2, _ := s.AddReviewQuestion(ctx, ReviewQuestion{ReviewID: r1.ID, Path: "x", Question: "b?"})
	if id, err := s.AnswerReviewQuestion(ctx, sess.ID, QuestionAnswered, "short", "full", ""); err != nil || id != q1.ID {
		t.Fatalf("answer = %d, %v", id, err)
	}
	if id, _ := s.AnswerReviewQuestion(ctx, sess.ID, QuestionFailed, "", "", "boom"); id != q2.ID {
		t.Fatalf("second answer went to %d", id)
	}
	if id, _ := s.AnswerReviewQuestion(ctx, sess.ID, QuestionAnswered, "x", "x", ""); id != 0 {
		t.Fatalf("answer without a pending question went to %d", id)
	}
	qs, err := s.ReviewQuestions(ctx, r1.ID)
	if err != nil || len(qs) != 2 {
		t.Fatalf("questions = %+v, %v", qs, err)
	}
	if qs[0].Status != QuestionAnswered || qs[0].ShortAnswer != "short" || qs[0].FullAnswer != "full" || qs[0].AnsweredAt == nil {
		t.Fatalf("q1 = %+v", qs[0])
	}
	if qs[1].Status != QuestionFailed || qs[1].Error != "boom" || qs[1].Line != 0 || qs[1].Side != "" {
		t.Fatalf("q2 = %+v", qs[1])
	}

	// A deleted session leaves the review; a deleted review takes its
	// questions; a deleted task its reviews.
	exec(t, s, fmt.Sprintf(`DELETE FROM sessions WHERE id = %d`, sess.ID))
	if got, _ := s.Review(ctx, r1.ID); got.SessionID != nil {
		t.Fatalf("session after delete = %v", *got.SessionID)
	}
	if err := s.DeleteReview(ctx, r1.ID); err != nil {
		t.Fatal(err)
	}
	if qs, _ := s.ReviewQuestions(ctx, r1.ID); len(qs) != 0 {
		t.Fatalf("questions after delete = %+v", qs)
	}
	if err := s.DeleteTask(ctx, task.ID); err != nil {
		t.Fatal(err)
	}
	if list, _ := s.Reviews(ctx, task.ID); len(list) != 0 {
		t.Fatalf("reviews after task delete = %+v", list)
	}
}

// Reviews and their questions are shared with the other nodes.
func TestReviewsSync(t *testing.T) {
	s := openSynced(t)
	ctx := context.Background()
	task, err := s.CreateTask(ctx, Task{Title: "t"})
	if err != nil {
		t.Fatal(err)
	}
	r, err := s.CreateReview(ctx, Review{TaskID: task.ID})
	if err != nil {
		t.Fatal(err)
	}
	q, err := s.AddReviewQuestion(ctx, ReviewQuestion{ReviewID: r.ID, Path: "x", Question: "?"})
	if err != nil {
		t.Fatal(err)
	}
	got := outbox(t, s)
	for _, want := range []string{fmt.Sprintf("insert reviews [%d]", r.ID), fmt.Sprintf("insert review_questions [%d]", q.ID)} {
		if !slices.Contains(got, want) {
			t.Fatalf("outbox %v lacks %q", got, want)
		}
	}
	if err := s.DeleteTask(ctx, task.ID); err != nil {
		t.Fatal(err)
	}
	if _, deleted := syncRow(t, s, "review_questions", fmt.Sprintf("[%d]", q.ID)); !deleted {
		t.Fatal("question not deleted with its task")
	}
}
