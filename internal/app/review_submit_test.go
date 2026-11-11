package app

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/jeanpetric/assistant/internal/store"
)

func TestParsePRURL(t *testing.T) {
	for _, c := range []struct{ in, owner, repo, n string }{
		{"https://github.com/productiveio/api/pull/25333/changes", "productiveio", "api", "25333"},
		{"https://github.com/o/r/pull/1", "o", "r", "1"},
		{" https://github.com/o/r/pull/7/files ", "o", "r", "7"},
		{"https://github.com/o/r/pull/7#discussion", "o", "r", "7"},
	} {
		o, r, n, ok := ParsePRURL(c.in)
		if !ok || o != c.owner || r != c.repo || n != c.n {
			t.Errorf("ParsePRURL(%q) = %s %s %s %v", c.in, o, r, n, ok)
		}
	}
	for _, bad := range []string{"", "https://github.com/o/r/issues/1", "https://gitlab.com/o/r/pull/1", "https://github.com/o/r/pull/x"} {
		if _, _, _, ok := ParsePRURL(bad); ok {
			t.Errorf("ParsePRURL(%q) ok", bad)
		}
	}
}

// SubmitReview runs gh pr review on the review's PR with the verdict's flag
// and the message; bad input never reaches gh, gh's error comes back.
func TestSubmitReview(t *testing.T) {
	a, _ := newApp(t)
	ctx := t.Context()
	task, err := a.Store.CreateTask(ctx, store.Task{Title: "PR"})
	if err != nil {
		t.Fatal(err)
	}
	r, err := a.Store.CreateReview(ctx, store.Review{TaskID: task.ID, PRURL: "https://github.com/o/r/pull/12/changes"})
	if err != nil {
		t.Fatal(err)
	}
	var calls [][]string
	ghErr := error(nil)
	a.GH = func(_ context.Context, args ...string) ([]byte, error) {
		calls = append(calls, args)
		return nil, ghErr
	}
	if err := a.SubmitReview(ctx, r.ID, "approve", ""); err != nil {
		t.Fatal(err)
	}
	if err := a.SubmitReview(ctx, r.ID, "request_changes", "  Popravi `x`.\n"); err != nil {
		t.Fatal(err)
	}
	if err := a.SubmitReview(ctx, r.ID, "comment", "Bravo"); err != nil {
		t.Fatal(err)
	}
	want := [][]string{
		{"pr", "review", "12", "-R", "o/r", "--approve"},
		{"pr", "review", "12", "-R", "o/r", "--request-changes", "--body", "Popravi `x`."},
		{"pr", "review", "12", "-R", "o/r", "--comment", "--body", "Bravo"},
	}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("gh calls = %q", calls)
	}
	for _, c := range []struct{ event, body string }{{"merge", "x"}, {"comment", " "}, {"request_changes", ""}} {
		if err := a.SubmitReview(ctx, r.ID, c.event, c.body); !errors.Is(err, ErrBadReview) {
			t.Errorf("%s %q: %v", c.event, c.body, err)
		}
	}
	if err := a.SubmitReview(ctx, 999, "approve", ""); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("unknown review: %v", err)
	}
	if len(calls) != 3 {
		t.Fatalf("bad input reached gh: %q", calls[3:])
	}
	ghErr = errors.New("Can not approve your own pull request")
	if err := a.SubmitReview(ctx, r.ID, "approve", ""); !errors.Is(err, ErrGitHub) || !strings.Contains(err.Error(), "own pull request") {
		t.Fatalf("gh error = %v", err)
	}
}
