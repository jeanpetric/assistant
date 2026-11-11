package app

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/jeanpetric/assistant/internal/agent"
	"github.com/jeanpetric/assistant/internal/store"
)

const reviewDiff = `diff --git a/app/pay.rb b/app/pay.rb
index 1..2 100644
--- a/app/pay.rb
+++ b/app/pay.rb
@@ -1,4 +1,5 @@ class Pay
 class Pay
-  def old_total
+  def total
+    round(sum)
   end
 end
diff --git a/config/x.yml b/config/x.yml
new file mode 100644
--- /dev/null
+++ b/config/x.yml
@@ -0,0 +1 @@
+a: 1
diff --git a/lib/b.rb b/lib/b.rb
--- a/lib/b.rb
+++ b/lib/b.rb
@@ -3,1 +3,1 @@
-x
+y
diff --git a/lib/a.rb b/lib/c.rb
similarity index 90%
rename from lib/a.rb
rename to lib/c.rb
diff --git a/spec/pay_spec.rb b/spec/pay_spec.rb
--- a/spec/pay_spec.rb
+++ b/spec/pay_spec.rb
@@ -1 +1,2 @@
 it
+it2
`

const reviewNotes = `{
  "title": "o/r#1: Pay", "url": "https://github.com/o/r/pull/1", "head": "abc123",
  "summary": "Mijenja zbroj.",
  "files": [
    {"path": "lib/b.rb", "rank": 2, "why": "poziva"},
    {"path": "app/pay.rb", "rank": 1, "why": "srž"},
    {"path": "spec/pay_spec.rb", "unimportant": true, "why": "test"}
  ],
  "annotations": {"app/pay.rb": [{"line": 2, "kind": "method", "text": "Računa zbroj."}, {"line": 2, "side": "old", "text": "Maknuto."}]}
}`

func TestHiddenBy(t *testing.T) {
	for p, want := range map[string]string{
		"config/x.yml": "*.yml", "README.md": "*.md", "spec/a/b.rb": "spec/**", "engines/e/spec/x.rb": "spec/**",
		"test/x.go": "test/**", "a/user_spec.rb": "*_spec.rb", "x_test.go": "*_test.go", "docs/a.plain": "*.plain",
		"app/x.rb": "", "specs/x.rb": "", "lib/testing.go": "", "a.json5": "",
	} {
		if got := HiddenBy(p); got != want {
			t.Errorf("HiddenBy(%q) = %q, want %q", p, got, want)
		}
	}
}

func TestParseDiff(t *testing.T) {
	files := ParseDiff(reviewDiff)
	if len(files) != 5 {
		t.Fatalf("files = %+v", files)
	}
	pay := files[0]
	if pay.Path != "app/pay.rb" || pay.Adds != 2 || pay.Dels != 1 || len(pay.Hunks) != 1 || pay.Hunks[0].Header != "@@ -1,4 +1,5 @@ class Pay" {
		t.Fatalf("pay = %+v", pay)
	}
	want := []PRLine{{" ", 1, 1, "class Pay"}, {"-", 2, 0, "  def old_total"}, {"+", 0, 2, "  def total"}, {"+", 0, 3, "    round(sum)"},
		{" ", 3, 4, "  end"}, {" ", 4, 5, "end"}}
	if !slices.Equal(pay.Hunks[0].Lines, want) {
		t.Fatalf("lines = %+v", pay.Hunks[0].Lines)
	}
	if files[1].Status != "added" || files[3].Status != "renamed" || files[3].OldPath != "lib/a.rb" || files[3].Path != "lib/c.rb" || len(files[3].Hunks) != 0 {
		t.Fatalf("statuses = %+v", files)
	}
	if !pay.has("old", 2) || !pay.has("new", 2) || pay.has("old", 1) || !pay.has("new", 1) || pay.has("new", 9) {
		t.Fatal("has")
	}
}

func writeDiff(t *testing.T, text string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "pr.diff")
	if err := os.WriteFile(p, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func prTask(t *testing.T, a *App) store.Task {
	t.Helper()
	task, err := a.Store.CreateTask(context.Background(), store.Task{Title: "PR", Fields: map[string]string{"pull_request": "https://github.com/o/r/pull/1"}})
	if err != nil {
		t.Fatal(err)
	}
	return task
}

func TestSaveReviewWarnsAndRefuses(t *testing.T) {
	a, _ := newApp(t)
	ctx := context.Background()
	task := prTask(t, a)
	r, err := a.Store.CreateReview(ctx, store.Review{TaskID: task.ID})
	if err != nil {
		t.Fatal(err)
	}
	diff := writeDiff(t, reviewDiff)
	bad := `{"files": [{"path": "nope.rb", "rank": 1}], "annotations": {"app/pay.rb": [{"line": 3, "side": "old", "text": "x"}, {"line": 3, "text": "ok"}], "gone.rb": []}}`
	warns, err := a.SaveReview(ctx, r.ID, json.RawMessage(bad), diff)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"notes name a file not in the diff: nope.rb", "app/pay.rb: note on old line 3 is not in the diff",
		"annotations name a file not in the diff: gone.rb"}
	if !slices.Equal(warns, want) {
		t.Fatalf("warnings = %q", warns)
	}
	// Saved all the same; a fixed save replaces it.
	if got, _ := a.Store.Review(ctx, r.ID); got.Notes == "" || got.Diff != reviewDiff {
		t.Fatalf("not saved: %+v", got)
	}
	if warns, err = a.SaveReview(ctx, r.ID, json.RawMessage(reviewNotes), diff); err != nil || len(warns) != 0 {
		t.Fatalf("good notes: %q, %v", warns, err)
	}
	got, _ := a.Store.Review(ctx, r.ID)
	if got.Title != "o/r#1: Pay" || got.HeadSHA != "abc123" || got.PRURL != "https://github.com/o/r/pull/1" || !strings.Contains(got.Notes, "Računa zbroj.") {
		t.Fatalf("review = %+v", got)
	}

	for name, c := range map[string]struct {
		notes, path string
	}{
		"string notes":  {`"notes"`, diff},
		"array notes":   {`[]`, diff},
		"relative path": {reviewNotes, "pr.diff"},
		"missing diff":  {reviewNotes, filepath.Join(t.TempDir(), "none.diff")},
		"not a diff":    {reviewNotes, writeDiff(t, "hello")},
		"too large":     {reviewNotes, writeDiff(t, reviewDiff+strings.Repeat("+x\n", MaxReviewDiff/3+1))},
	} {
		if _, err := a.SaveReview(ctx, r.ID, json.RawMessage(c.notes), c.path); !errors.Is(err, ErrBadReview) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := a.SaveReview(ctx, 999, json.RawMessage(reviewNotes), diff); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("unknown review: %v", err)
	}
}

func TestReviewFilesOrder(t *testing.T) {
	var n ReviewNotes
	if err := json.Unmarshal([]byte(reviewNotes), &n); err != nil {
		t.Fatal(err)
	}
	files := ReviewFiles(reviewDiff, n)
	var got []string
	for _, f := range files {
		got = append(got, f.Group+":"+f.Path)
	}
	want := []string{"ranked:app/pay.rb", "ranked:lib/b.rb", "unimportant:spec/pay_spec.rb", "hidden:config/x.yml", "other:lib/c.rb"}
	if !slices.Equal(got, want) {
		t.Fatalf("order = %q", got)
	}
	if files[0].Why != "srž" || len(files[0].Annotations) != 2 || files[0].Annotations[0].Side != "new" || files[3].HiddenBy != "*.yml" {
		t.Fatalf("files = %+v", files)
	}
}

func TestQuestionPrompt(t *testing.T) {
	var n ReviewNotes
	json.Unmarshal([]byte(reviewNotes), &n)
	r := store.Review{PRURL: "https://github.com/o/r/pull/1", Title: "o/r#1: Pay", HeadSHA: "abc123", Diff: reviewDiff}
	got, err := QuestionPrompt(r, n, store.ReviewQuestion{Path: "app/pay.rb", Side: "new", Line: 2, Question: "Zašto round?"})
	if err != nil {
		t.Fatal(err)
	}
	want := `Pitanje iz reviewa PR-a https://github.com/o/r/pull/1 ("o/r#1: Pay", head abc123).
File: app/pay.rb, linija 2 (new strana)
Isječak diffa:
` + "```diff" + `
@@ -1,4 +1,5 @@ class Pay
 class Pay
-  def old_total
+  def total
+    round(sum)
   end
 end
` + "```" + `
Postojeća bilješka: Računa zbroj.

Pitanje: Zašto round?

Odgovori na hrvatskom. Na kraju odgovora dodaj <kratko>2-4 rečenice jednostavnim jezikom: odgovor na pitanje</kratko>.
Kod PR-a čitaj s ` + "`gh`/`git`" + ` na head commitu; ništa ne mijenjaj ni ne komentiraj na GitHubu.`
	if got != want {
		t.Fatalf("prompt:\n%s\n---want:\n%s", got, want)
	}
	// A whole file: its diff, no line, no note.
	got, err = QuestionPrompt(r, n, store.ReviewQuestion{Path: "lib/b.rb", Question: "Što?"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "File: lib/b.rb\nIsječak diffa:\n```diff\n@@ -3,1 +3,1 @@\n-x\n+y\n```\n\nPitanje: Što?") || strings.Contains(got, "bilješka") {
		t.Fatalf("file prompt:\n%s", got)
	}
	// Only lines shown in the diff, on their side.
	if _, err := QuestionPrompt(r, n, store.ReviewQuestion{Path: "app/pay.rb", Side: "old", Line: 3, Question: "?"}); !errors.Is(err, ErrBadReview) {
		t.Fatalf("old line 3: %v", err)
	}
	if _, err := QuestionPrompt(r, n, store.ReviewQuestion{Path: "nope", Question: "?"}); !errors.Is(err, ErrBadReview) {
		t.Fatalf("unknown path: %v", err)
	}
	// A long hunk is cut to 20 lines around the line.
	long := "diff --git a/x b/x\n--- a/x\n+++ b/x\n@@ -1,60 +1,60 @@\n" + strings.Repeat(" c\n", 60)
	got, _ = QuestionPrompt(store.Review{Diff: long}, ReviewNotes{}, store.ReviewQuestion{Path: "x", Side: "new", Line: 30, Question: "?"})
	if n := strings.Count(got, "\n c"); n != 41 || strings.Contains(got, "@@") {
		t.Fatalf("snippet has %d lines:\n%s", n, got)
	}
}

func TestSplitAnswer(t *testing.T) {
	for _, c := range []struct{ reply, short, full string }{
		{"Dugo objašnjenje.\n\n<kratko>Kratko i jasno.</kratko>", "Kratko i jasno.", "Dugo objašnjenje."},
		{"<kratko>prvo</kratko> tekst <kratko>\n zadnje \n</kratko>", "zadnje", "tekst"},
		{"Bez bloka,\n  samo tekst.", "Bez bloka, samo tekst.", "Bez bloka,\n  samo tekst."},
		{strings.Repeat("ž", 400), strings.Repeat("ž", 300) + "…", strings.Repeat("ž", 400)},
		{"Tekst <kratko> </kratko>", "Tekst", "Tekst"},
	} {
		short, full := SplitAnswer(c.reply)
		if short != c.short || full != c.full {
			t.Errorf("SplitAnswer(%q) = %q, %q", c.reply, short, full)
		}
	}
}

// A review's session makes it and then answers its questions, one turn
// each, oldest question first; a new review gets a new session.
func TestReviewSessionFlow(t *testing.T) {
	a, r := newApp(t)
	ctx := context.Background()
	if _, _, err := a.GenerateReview(ctx, 999); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unknown task: %v", err)
	}
	plain, _ := a.Store.CreateTask(ctx, store.Task{Title: "no PR"})
	if _, _, err := a.GenerateReview(ctx, plain.ID); !errors.Is(err, ErrNoPR) {
		t.Fatalf("task without PR: %v", err)
	}
	// Skills do not run on a review's turns.
	if _, err := a.Store.SaveSkill(ctx, store.Skill{Name: "Guard", Body: "check", Automations: []store.SkillAutomation{{Enabled: true}}}); err != nil {
		t.Fatal(err)
	}
	task := prTask(t, a)
	block := make(chan struct{})
	r.block = block
	r.result = func(req agent.Request) string {
		if strings.HasPrefix(req.Prompt, QuestionPrefix) {
			q := req.Prompt[strings.Index(req.Prompt, "Pitanje: ")+len("Pitanje: "):]
			q = q[:strings.Index(q, "\n")]
			return "Detaljno o " + q + ".\n<kratko>Odgovor na " + q + "</kratko>"
		}
		return "Review spremljen."
	}
	rev, sess, err := a.GenerateReview(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if sess.Title != "PR review" || sess.Trigger != ReviewTrigger || sess.TaskID != task.ID || *rev.SessionID != sess.ID {
		t.Fatalf("session = %+v, review = %+v", sess, rev)
	}
	saved, _ := a.Store.Review(ctx, rev.ID)
	if st := a.ReviewStatusOf(ctx, saved); st != ReviewGenerating {
		t.Fatalf("status while running = %s", st)
	}
	if _, err := a.AskReview(ctx, rev.ID, store.ReviewQuestion{Path: "app/pay.rb", Question: "?"}); !errors.Is(err, ErrReviewNotReady) {
		t.Fatalf("ask while generating: %v", err)
	}
	// The agent saves the notes; a question asked before its turn ends waits
	// for the next turn and is not answered by this one.
	if _, err := a.SaveReview(ctx, rev.ID, json.RawMessage(reviewNotes), writeDiff(t, reviewDiff)); err != nil {
		t.Fatal(err)
	}
	q1, err := a.AskReview(ctx, rev.ID, store.ReviewQuestion{Path: "app/pay.rb", Side: "new", Line: 2, Question: "prvo"})
	if err != nil {
		t.Fatal(err)
	}
	q2, err := a.AskReview(ctx, rev.ID, store.ReviewQuestion{Path: "lib/b.rb", Question: "drugo"})
	if err != nil {
		t.Fatal(err)
	}
	close(block)
	a.Wait()
	qs, _ := a.Store.ReviewQuestions(ctx, rev.ID)
	if len(qs) != 2 || qs[0].ID != q1.ID || qs[1].ID != q2.ID {
		t.Fatalf("questions = %+v", qs)
	}
	for i, want := range []string{"prvo", "drugo"} {
		if qs[i].Status != store.QuestionAnswered || qs[i].ShortAnswer != "Odgovor na "+want || qs[i].FullAnswer != "Detaljno o "+want+"." {
			t.Fatalf("question %d = %+v", i, qs[i])
		}
	}
	reqs := r.requests()
	if len(reqs) != 3 || !strings.Contains(reqs[0].Prompt, "review_id "+itoa(rev.ID)) || !strings.Contains(reqs[0].Prompt, "https://github.com/o/r/pull/1") ||
		!strings.HasPrefix(reqs[1].Prompt, QuestionPrefix) || !strings.Contains(reqs[1].Prompt, "linija 2 (new strana)") || reqs[2].ResumeID != "claude-1" {
		t.Fatalf("requests = %+v", reqs)
	}
	saved, _ = a.Store.Review(ctx, rev.ID)
	if st := a.ReviewStatusOf(ctx, saved); st != ReviewReady {
		t.Fatalf("status = %s", st)
	}
	if arts, _ := a.Store.Artifacts(ctx, task.ID); len(arts) != 0 {
		t.Fatalf("a skill ran on the review's session: %+v", arts)
	}

	// A failed turn fails its question.
	r.mu.Lock()
	r.failOn = QuestionPrefix
	r.mu.Unlock()
	q3, err := a.AskReview(ctx, rev.ID, store.ReviewQuestion{Path: "app/pay.rb", Side: "old", Line: 2, Question: "treće"})
	if err != nil {
		t.Fatal(err)
	}
	a.Wait()
	qs, _ = a.Store.ReviewQuestions(ctx, rev.ID)
	if qs[2].ID != q3.ID || qs[2].Status != store.QuestionFailed || !strings.Contains(qs[2].Error, "failed") {
		t.Fatalf("failed question = %+v", qs[2])
	}
	r.mu.Lock()
	r.failOn = ""
	r.mu.Unlock()

	// Bad questions.
	for _, q := range []store.ReviewQuestion{
		{Path: "app/pay.rb", Question: " "},
		{Path: "", Question: "?"},
		{Path: "app/pay.rb", Side: "new", Question: "?"},
		{Path: "app/pay.rb", Line: 2, Question: "?"},
		{Path: "app/pay.rb", Side: "left", Line: 2, Question: "?"},
		{Path: "app/pay.rb", Side: "new", Line: 99, Question: "?"},
		{Path: "nope.rb", Question: "?"},
	} {
		if _, err := a.AskReview(ctx, rev.ID, q); !errors.Is(err, ErrBadReview) {
			t.Errorf("AskReview(%+v) = %v", q, err)
		}
	}
	if qs, _ := a.Store.ReviewQuestions(ctx, rev.ID); len(qs) != 3 {
		t.Fatalf("bad questions were kept: %+v", qs)
	}

	// A new review has a new session; one whose session ends without notes failed.
	rev2, sess2, err := a.GenerateReview(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	a.Wait()
	if sess2.ID == sess.ID {
		t.Fatal("new review reused the session")
	}
	saved, _ = a.Store.Review(ctx, rev2.ID)
	if st := a.ReviewStatusOf(ctx, saved); st != ReviewFailed {
		t.Fatalf("status without notes = %s", st)
	}
	if _, err := a.AskReview(ctx, rev2.ID, store.ReviewQuestion{Path: "app/pay.rb", Question: "?"}); !errors.Is(err, ErrReviewNotReady) {
		t.Fatalf("ask on failed review: %v", err)
	}
}

// A question to another node's session is refused, and none is kept.
func TestAskReviewForeignSession(t *testing.T) {
	a, _ := newApp(t)
	ctx := context.Background()
	task := prTask(t, a)
	sess, err := a.Store.CreateSession(ctx, store.Session{TaskID: task.ID, Title: "PR review", Trigger: ReviewTrigger})
	if err != nil {
		t.Fatal(err)
	}
	makeForeign(t, a.Store, sess)
	rev, _ := a.Store.CreateReview(ctx, store.Review{TaskID: task.ID, SessionID: &sess.ID})
	if _, err := a.SaveReview(ctx, rev.ID, json.RawMessage(reviewNotes), writeDiff(t, reviewDiff)); err != nil {
		t.Fatal(err)
	}
	if _, err := a.AskReview(ctx, rev.ID, store.ReviewQuestion{Path: "app/pay.rb", Question: "?"}); !errors.Is(err, ErrForeignSession) {
		t.Fatalf("foreign: %v", err)
	}
	if qs, _ := a.Store.ReviewQuestions(ctx, rev.ID); len(qs) != 0 {
		t.Fatalf("questions = %+v", qs)
	}
}
