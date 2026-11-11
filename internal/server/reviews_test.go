package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jeanpetric/assistant/internal/store"
)

const testDiff = `diff --git a/app/pay.rb b/app/pay.rb
--- a/app/pay.rb
+++ b/app/pay.rb
@@ -1,2 +1,2 @@
-def a
+def b
 end
diff --git a/README.md b/README.md
--- a/README.md
+++ b/README.md
@@ -1 +1 @@
-x
+y
`

type reviewPage struct {
	Review struct {
		ID        int64  `json:"id"`
		SessionID int64  `json:"session_id"`
		Status    string `json:"status"`
		Title     string `json:"title"`
		HeadSHA   string `json:"head_sha"`
	} `json:"review"`
	Summary string `json:"summary"`
	Files   []struct {
		Path  string `json:"path"`
		Group string `json:"group"`
		Hunks []any  `json:"hunks"`
	} `json:"files"`
	Questions []store.ReviewQuestion `json:"questions"`
	Session   struct {
		ID int64 `json:"id"`
	} `json:"session"`
}

func TestReviewAPI(t *testing.T) {
	c := newClient(t)
	var plain, task store.Task
	c.do("POST", "/api/tasks", map[string]any{"title": "no PR"}, 201, &plain)
	c.do("POST", fmt.Sprintf("/api/tasks/%d/reviews", plain.ID), nil, 400, nil)
	c.do("POST", "/api/tasks/999/reviews", nil, 404, nil)
	c.do("GET", "/api/tasks/999/reviews", nil, 404, nil)
	c.do("GET", "/api/reviews/999", nil, 404, nil)
	c.do("POST", "/api/tasks", map[string]any{"title": "PR", "fields": map[string]string{"pull_request": "https://github.com/o/r/pull/1"}}, 201, &task)

	var started struct {
		ReviewID  int64 `json:"review_id"`
		SessionID int64 `json:"session_id"`
	}
	c.do("POST", fmt.Sprintf("/api/tasks/%d/reviews", task.ID), nil, 201, &started)
	if started.ReviewID == 0 || started.SessionID == 0 {
		t.Fatalf("started = %+v", started)
	}
	c.app.Wait()
	// The session ended without saving: failed, and no questions then.
	var page reviewPage
	c.do("GET", fmt.Sprintf("/api/reviews/%d", started.ReviewID), nil, 200, &page)
	if page.Review.Status != "failed" || page.Session.ID != started.SessionID || len(page.Files) != 0 {
		t.Fatalf("page = %+v", page)
	}
	ask := func(want int, q map[string]any) {
		c.do("POST", fmt.Sprintf("/api/reviews/%d/questions", started.ReviewID), q, want, nil)
	}
	ask(409, map[string]any{"path": "app/pay.rb", "question": "?"})

	// The agent saves through MCP and hears of every mistake.
	diff := filepath.Join(t.TempDir(), "pr.diff")
	os.WriteFile(diff, []byte(testDiff), 0o600)
	notes := map[string]any{"title": "o/r#1: Pay", "url": "https://github.com/o/r/pull/1", "head": "abc", "summary": "Sažetak.",
		"files":       []map[string]any{{"path": "app/pay.rb", "rank": 1, "why": "srž"}, {"path": "gone.rb", "rank": 2}},
		"annotations": map[string]any{"app/pay.rb": []map[string]any{{"line": 1, "text": "b"}, {"line": 5, "text": "x"}}}}
	text, isErr := c.tool("save_review", map[string]any{"review_id": started.ReviewID, "notes": notes, "diff_path": diff})
	if isErr || !strings.Contains(text, "gone.rb") || !strings.Contains(text, "new line 5") {
		t.Fatalf("save_review = %s", text)
	}
	if text, isErr = c.tool("save_review", map[string]any{"review_id": started.ReviewID, "notes": "{}", "diff_path": diff}); !isErr || !strings.HasPrefix(text, "400") {
		t.Fatalf("string notes = %s", text)
	}
	c.do("GET", fmt.Sprintf("/api/reviews/%d", started.ReviewID), nil, 200, &page)
	if page.Review.Status != "ready" || page.Review.Title != "o/r#1: Pay" || page.Summary != "Sažetak." || len(page.Files) != 2 ||
		page.Files[0].Group != "ranked" || page.Files[1].Group != "hidden" || len(page.Files[0].Hunks) != 1 {
		t.Fatalf("page = %+v", page)
	}

	ask(400, map[string]any{"path": "app/pay.rb", "side": "new", "line": 9, "question": "?"})
	ask(400, map[string]any{"path": "app/pay.rb", "question": ""})
	ask(201, map[string]any{"path": "app/pay.rb", "side": "new", "line": 1, "question": "Zašto?"})
	c.app.Wait()
	c.do("GET", fmt.Sprintf("/api/reviews/%d", started.ReviewID), nil, 200, &page)
	if len(page.Questions) != 1 || page.Questions[0].Status != "answered" || page.Questions[0].ShortAnswer != "ok" || page.Questions[0].Line != 1 {
		t.Fatalf("questions = %+v", page.Questions)
	}

	// get_task and the task's list name the reviews, newest first.
	var second struct {
		ReviewID int64 `json:"review_id"`
	}
	c.do("POST", fmt.Sprintf("/api/tasks/%d/reviews", task.ID), nil, 201, &second)
	c.app.Wait()
	var list []struct {
		ID     int64  `json:"id"`
		Status string `json:"status"`
	}
	c.do("GET", fmt.Sprintf("/api/tasks/%d/reviews", task.ID), nil, 200, &list)
	if len(list) != 2 || list[0].ID != second.ReviewID || list[1].Status != "ready" {
		t.Fatalf("list = %+v", list)
	}
	text, _ = c.tool("get_task", map[string]any{"task_id": task.ID})
	var got struct {
		Reviews []struct {
			ID      int64  `json:"id"`
			HeadSHA string `json:"head_sha"`
			Status  string `json:"status"`
		} `json:"reviews"`
	}
	json.Unmarshal([]byte(text), &got)
	if len(got.Reviews) != 2 || got.Reviews[1].HeadSHA != "abc" || got.Reviews[1].Status != "ready" {
		t.Fatalf("get_task reviews = %s", text)
	}

	// get_review leaves the diff out; ask_review asks through MCP.
	text, _ = c.tool("get_review", map[string]any{"review_id": started.ReviewID})
	if strings.Contains(text, "hunks") || !strings.Contains(text, "Sažetak.") || !strings.Contains(text, `"group":"ranked"`) || !strings.Contains(text, "Zašto?") {
		t.Fatalf("get_review = %s", text)
	}
	if text, isErr = c.tool("ask_review", map[string]any{"review_id": started.ReviewID, "path": "README.md", "question": "Treba li?"}); isErr {
		t.Fatalf("ask_review = %s", text)
	}
	c.app.Wait()
	if text, isErr = c.tool("generate_review", map[string]any{"task_id": plain.ID}); !isErr || !strings.Contains(text, "pull_request") {
		t.Fatalf("generate_review without PR = %s", text)
	}
	c.app.Wait()

	c.do("DELETE", fmt.Sprintf("/api/reviews/%d", started.ReviewID), nil, 204, nil)
	c.do("GET", fmt.Sprintf("/api/reviews/%d", started.ReviewID), nil, 404, nil)
	c.do("DELETE", fmt.Sprintf("/api/reviews/%d", started.ReviewID), nil, 404, nil)
}

// A question for a review whose session runs on another node is refused.
func TestReviewForeignSession(t *testing.T) {
	c := newClient(t)
	ctx := t.Context()
	var task store.Task
	c.do("POST", "/api/tasks", map[string]any{"title": "PR", "fields": map[string]string{"pull_request": "https://github.com/o/r/pull/1"}}, 201, &task)
	var started struct {
		ReviewID  int64 `json:"review_id"`
		SessionID int64 `json:"session_id"`
	}
	c.do("POST", fmt.Sprintf("/api/tasks/%d/reviews", task.ID), nil, 201, &started)
	c.app.Wait()
	diff := filepath.Join(t.TempDir(), "pr.diff")
	os.WriteFile(diff, []byte(testDiff), 0o600)
	c.do("PUT", fmt.Sprintf("/api/reviews/%d/notes", started.ReviewID), map[string]any{"notes": map[string]any{"summary": "s"}, "diff_path": diff}, 200, nil)
	sess, _ := c.app.Store.Session(ctx, started.SessionID)
	foreign(t, c, sess)
	c.do("POST", fmt.Sprintf("/api/reviews/%d/questions", started.ReviewID), map[string]any{"path": "app/pay.rb", "question": "?"}, 409, nil)
}

// Submitting a review: the verdict and message go to gh pr review on the
// review's PR (API and MCP); bad input is 400, gh's refusal 502.
func TestSubmitReviewAPI(t *testing.T) {
	c := newClient(t)
	ctx := t.Context()
	task, _ := c.app.Store.CreateTask(ctx, store.Task{Title: "PR"})
	rev, _ := c.app.Store.CreateReview(ctx, store.Review{TaskID: task.ID, PRURL: "https://github.com/o/r/pull/3/changes"})
	var calls []string
	var ghErr error
	c.app.GH = func(_ context.Context, args ...string) ([]byte, error) {
		calls = append(calls, strings.Join(args, " "))
		return nil, ghErr
	}
	submit := fmt.Sprintf("/api/reviews/%d/submit", rev.ID)
	var out struct {
		Submitted bool   `json:"submitted"`
		Event     string `json:"event"`
	}
	c.do("POST", submit, map[string]any{"event": "request_changes", "body": "Fali test."}, 200, &out)
	if !out.Submitted || out.Event != "request_changes" || len(calls) != 1 || calls[0] != "pr review 3 -R o/r --request-changes --body Fali test." {
		t.Fatalf("out = %+v, calls = %q", out, calls)
	}
	c.do("POST", submit, map[string]any{"event": "comment"}, 400, nil)
	c.do("POST", submit, map[string]any{"event": "merge", "body": "x"}, 400, nil)
	c.do("POST", "/api/reviews/999/submit", map[string]any{"event": "approve"}, 404, nil)
	if text, isErr := c.tool("submit_review", map[string]any{"review_id": rev.ID, "event": "approve"}); isErr || len(calls) != 2 || calls[1] != "pr review 3 -R o/r --approve" {
		t.Fatalf("submit_review = %s, calls = %q", text, calls)
	}
	ghErr = errors.New("Can not approve your own pull request")
	var e struct {
		Error string `json:"error"`
	}
	c.do("POST", submit, map[string]any{"event": "approve"}, 502, &e)
	if !strings.Contains(e.Error, "own pull request") {
		t.Fatalf("error = %q", e.Error)
	}
}

// A review of a PR with .plain specs gives the page their head content (specs) and the concepts'
// Croatian notes; get_review keeps the concepts but not the specs.
func TestReviewSpecsAPI(t *testing.T) {
	c := newClient(t)
	var task store.Task
	c.do("POST", "/api/tasks", map[string]any{"title": "PR", "fields": map[string]string{"pull_request": "https://github.com/o/r/pull/1"}}, 201, &task)
	r, err := c.app.Store.CreateReview(t.Context(), store.Review{TaskID: task.ID, PRURL: "https://github.com/o/r/pull/1"})
	if err != nil {
		t.Fatal(err)
	}
	c.app.GH = func(_ context.Context, args ...string) ([]byte, error) {
		if strings.Contains(args[len(args)-1], "/git/trees/") {
			return []byte(`{"tree": [{"path": "plain/a.plain", "type": "blob"}]}`), nil
		}
		return []byte("***definitions***\n\n- :Period: is a slice.\n"), nil
	}
	diff := filepath.Join(t.TempDir(), "pr.diff")
	os.WriteFile(diff, []byte("diff --git a/plain/a.plain b/plain/a.plain\n--- a/plain/a.plain\n+++ b/plain/a.plain\n@@ -3 +3 @@\n-- :Period: is.\n+- :Period: is a slice.\n"), 0o600)
	notes := map[string]any{"title": "t", "url": "https://github.com/o/r/pull/1", "head": "abc", "summary": "s", "files": []any{},
		"concepts": map[string]string{"Period": "Razdoblje."}}
	if text, isErr := c.tool("save_review", map[string]any{"review_id": r.ID, "notes": notes, "diff_path": diff}); isErr || strings.Contains(text, "not in the diff") {
		t.Fatalf("save_review = %s", text)
	}
	var page struct {
		Specs    map[string]string `json:"specs"`
		Concepts map[string]string `json:"concepts"`
	}
	c.do("GET", fmt.Sprintf("/api/reviews/%d", r.ID), nil, 200, &page)
	if page.Specs["plain/a.plain"] != "***definitions***\n\n- :Period: is a slice.\n" || page.Concepts["Period"] != "Razdoblje." {
		t.Fatalf("page = %+v", page)
	}
	text, _ := c.tool("get_review", map[string]any{"review_id": r.ID})
	if strings.Contains(text, `"specs"`) || !strings.Contains(text, "Razdoblje.") {
		t.Fatalf("get_review = %s", text)
	}
}
