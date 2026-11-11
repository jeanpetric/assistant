package app

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/jeanpetric/assistant/internal/store"
)

const specDiff = `diff --git a/plain/internal/rd/amounts.plain b/plain/internal/rd/amounts.plain
--- a/plain/internal/rd/amounts.plain
+++ b/plain/internal/rd/amounts.plain
@@ -8,3 +8,3 @@
 ***functional specs***
` + " " + `
-- A :Period: stores cents.
+- A :Period: stores whole cents.
diff --git a/plain/internal/gone.plain b/plain/internal/gone.plain
deleted file mode 100644
--- a/plain/internal/gone.plain
+++ /dev/null
@@ -1 +0,0 @@
-- x
diff --git a/app/x.rb b/app/x.rb
--- a/app/x.rb
+++ b/app/x.rb
@@ -1 +1 @@
-a
+b
`

const amountsSpec = `---
description: Amounts.
import:
  - quotes/quote
  - rd/period
---

***definitions***
- :Base: is what percentages apply to.

***functional specs***

- A :Period: stores whole cents.
`

func TestSpecRefs(t *testing.T) {
	for _, c := range []struct {
		text string
		want []string
	}{
		{amountsSpec, []string{"quotes/quote", "rd/period"}},
		{"---\ndescription: x\nimport: [\"a/b\", c]\nrequires:\n  - 'd'\nexported_concepts:\n  - :X:\n---\n- import:\n  - nope\n", []string{"a/b", "c", "d"}},
		{"---\nrequires: base\n---\n", []string{"base"}},
		{"no frontmatter\nimport:\n  - x\n", nil},
	} {
		if got := SpecRefs(c.text); !slices.Equal(got, c.want) {
			t.Errorf("SpecRefs(%q) = %q, want %q", c.text, got, c.want)
		}
	}
}

func TestResolveSpec(t *testing.T) {
	tree := map[string]bool{"plain/internal/quotes/quote.plain": true, "plain/internal/rd/period.plain": true, "plain/internal/rd/rd/period.plain": false}
	for ref, want := range map[string]string{"quotes/quote": "plain/internal/quotes/quote.plain", "rd/period.plain": "plain/internal/rd/period.plain", "nope/x": ""} {
		if got := resolveSpec("plain/internal/rd/amounts.plain", ref, tree); got != want {
			t.Errorf("resolveSpec(%q) = %q, want %q", ref, got, want)
		}
	}
}

// SaveReview fetches the head content of the PR's .plain files (not a
// deleted one) and of the modules they import, transitively and once each;
// a module gh cannot give is left out and the review is saved all the same.
func TestSaveReviewFetchesSpecs(t *testing.T) {
	a, _ := newApp(t)
	ctx := context.Background()
	r, err := a.Store.CreateReview(ctx, store.Review{TaskID: prTask(t, a).ID, PRURL: "https://github.com/o/r/pull/1"})
	if err != nil {
		t.Fatal(err)
	}
	repo := map[string]string{
		"plain/internal/rd/amounts.plain":   amountsSpec,
		"plain/internal/quotes/quote.plain": "---\ndescription: q\n---\n***definitions***\n- :Quote: is a deal.\n",
		"plain/internal/rd/period.plain":    "---\nimport:\n  - quotes/quote\n  - rd/missing\n---\n***definitions***\n- :Period: is a slice of a :Quote:.\n",
		"plain/internal/rd/missing.plain":   "",
	}
	var calls []string
	a.GH = func(_ context.Context, args ...string) ([]byte, error) {
		calls = append(calls, strings.Join(args, " "))
		last := args[len(args)-1]
		if last == "repos/o/r/git/trees/abc123?recursive=1" {
			out := `{"tree": [{"path": "app/x.rb", "type": "blob"}, {"path": "plain", "type": "tree"}`
			for _, p := range slices.Sorted(maps.Keys(repo)) {
				out += `, {"path": "` + p + `", "type": "blob"}`
			}
			return []byte(out + `]}`), nil
		}
		p, ok := strings.CutPrefix(last, "repos/o/r/contents/")
		p, ok2 := strings.CutSuffix(p, "?ref=abc123")
		if !ok || !ok2 || args[1] != "-H" || args[2] != "Accept: application/vnd.github.raw" {
			t.Fatalf("gh %q", args)
		}
		if p == "plain/internal/rd/missing.plain" {
			return nil, errors.New("HTTP 404: Not Found")
		}
		return []byte(repo[p]), nil
	}
	notes := `{"title": "t", "url": "https://github.com/o/r/pull/1/changes", "head": "abc123", "summary": "s", "files": [],
		"concepts": {"Period": "Razdoblje prihoda."}, "specs": {"x": "the agent's is ignored"}}`
	warns, err := a.SaveReview(ctx, r.ID, json.RawMessage(notes), writeDiff(t, specDiff))
	if err != nil || len(warns) != 0 {
		t.Fatalf("save: %q %v", warns, err)
	}
	got, _ := a.Store.Review(ctx, r.ID)
	var n ReviewNotes
	if err := json.Unmarshal([]byte(got.Notes), &n); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"plain/internal/rd/amounts.plain":   repo["plain/internal/rd/amounts.plain"],
		"plain/internal/quotes/quote.plain": repo["plain/internal/quotes/quote.plain"],
		"plain/internal/rd/period.plain":    repo["plain/internal/rd/period.plain"],
	}
	if !reflect.DeepEqual(n.Specs, want) {
		t.Fatalf("specs = %q", slices.Sorted(maps.Keys(n.Specs)))
	}
	if n.Concepts["Period"] != "Razdoblje prihoda." {
		t.Fatalf("concepts = %v", n.Concepts)
	}
	// The tree once, each module once (quote is imported twice), the deleted file never.
	if len(calls) != 5 || strings.Contains(strings.Join(calls, "\n"), "gone.plain") {
		t.Fatalf("gh calls:\n%s", strings.Join(calls, "\n"))
	}

	// Without a head commit, or when gh lists nothing, only what can be had.
	a.GH = func(context.Context, ...string) ([]byte, error) { return nil, errors.New("gh: not logged in") }
	if warns, err := a.SaveReview(ctx, r.ID, json.RawMessage(notes), writeDiff(t, specDiff)); err != nil || len(warns) != 0 {
		t.Fatalf("save without gh: %q %v", warns, err)
	}
	got, _ = a.Store.Review(ctx, r.ID)
	if strings.Contains(got.Notes, `"specs"`) {
		t.Fatalf("specs without gh: %s", got.Notes)
	}
}

// A question on a spec line the diff does not show gets the module around
// it from the head content; without that content it is refused as before.
func TestQuestionPromptSpecLine(t *testing.T) {
	r := store.Review{PRURL: "https://github.com/o/r/pull/1", Diff: specDiff}
	n := ReviewNotes{Specs: map[string]string{"plain/internal/rd/amounts.plain": amountsSpec}}
	q := store.ReviewQuestion{Path: "plain/internal/rd/amounts.plain", Side: "new", Line: 3, Question: "Što je quote?"}
	got, err := QuestionPrompt(r, n, q)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "nepromijenjeni dio specifikacije") || !strings.Contains(got, "> 3: import:") || !strings.Contains(got, "\n  4:   - quotes/quote") {
		t.Fatalf("prompt:\n%s", got)
	}
	// A diff line still gets the diff.
	q.Line = 10
	if got, _ := QuestionPrompt(r, n, q); !strings.Contains(got, "+- A :Period: stores whole cents.") {
		t.Fatalf("diff line prompt:\n%s", got)
	}
	for _, c := range []struct {
		n    ReviewNotes
		side string
		line int
	}{{ReviewNotes{}, "new", 3}, {n, "old", 3}, {n, "new", 99}} {
		if _, err := QuestionPrompt(r, c.n, store.ReviewQuestion{Path: q.Path, Side: c.side, Line: c.line, Question: "?"}); !errors.Is(err, ErrBadReview) {
			t.Errorf("%s %d: %v", c.side, c.line, err)
		}
	}
}

func TestReviewPromptAsksForConcepts(t *testing.T) {
	p, err := ReviewPrompt(1, 2, "https://github.com/o/r/pull/1")
	if err != nil || !strings.Contains(p, `"concepts": {"Period": "…"}`) {
		t.Fatalf("prompt lacks concepts: %v\n%s", err, p)
	}
}
