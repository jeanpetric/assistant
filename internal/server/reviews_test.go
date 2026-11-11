package server

import (
	"encoding/json"
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
