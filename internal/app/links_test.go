package app

import (
	"context"
	"testing"

	"github.com/jeanpetric/assistant/internal/agent"
	"github.com/jeanpetric/assistant/internal/store"
)

func TestLinkFields(t *testing.T) {
	a, _ := newApp(t)
	ctx := context.Background()
	a.Store.UpsertFieldDef(ctx, store.FieldDef{Key: "note", Label: "Note", Type: "text"})
	desc := "Fix https://app.productive.io/1-org/tasks/42, see https://github.com/o/r/pull/7. " +
		"Older: https://github.com/o/r/pull/3 and https://github.com/o/r/issues/9, " +
		"thread https://acme.slack.com/archives/C1/p2."
	got := a.LinkFields(ctx, desc, nil)
	want := map[string]string{
		"productive":   "https://app.productive.io/1-org/tasks/42",
		"pull_request": "https://github.com/o/r/pull/7",
		"slack":        "https://acme.slack.com/archives/C1/p2",
	}
	if len(got) != len(want) {
		t.Fatalf("got %v", got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("%s = %q, want %q (all: %v)", k, got[k], v, got)
		}
	}

	// Fields the task has, or that were cleared on purpose, stay as they are.
	got = a.LinkFields(ctx, desc, map[string]string{"pull_request": "https://github.com/o/r/pull/1", "slack": ""})
	if _, ok := got["pull_request"]; ok {
		t.Fatalf("overwrote a set field: %v", got)
	}
	if _, ok := got["slack"]; ok {
		t.Fatalf("refilled a cleared field: %v", got)
	}

	// Only defined url fields are filled.
	a.Store.DeleteFieldDef(ctx, "slack")
	a.Store.UpsertFieldDef(ctx, store.FieldDef{Key: "productive", Label: "Productive", Type: "text"})
	got = a.LinkFields(ctx, desc, nil)
	if got["slack"] != "" || got["productive"] != "" {
		t.Fatalf("filled an undefined or text field: %v", got)
	}
	if got := a.LinkFields(ctx, "no links here", nil); len(got) != 0 {
		t.Fatalf("got %v", got)
	}
}

func TestNewSessionTaskGetsLinkFields(t *testing.T) {
	a, _ := newApp(t)
	ctx := context.Background()
	sess, err := a.StartSessionInNewTask(ctx, StartOpts{Prompt: "Review it\nhttps://github.com/o/r/pull/5"})
	if err != nil {
		t.Fatal(err)
	}
	a.Wait()
	task, _ := a.Store.Task(ctx, sess.TaskID)
	if task.Fields["pull_request"] != "https://github.com/o/r/pull/5" {
		t.Fatalf("fields = %v", task.Fields)
	}
}

func TestTurnRecordsOpenedPR(t *testing.T) {
	a, r := newApp(t)
	ctx := context.Background()
	task, _ := a.Store.CreateTask(ctx, store.Task{Title: "Ship it"})
	r.events = []agent.Event{
		// A PR the agent only reads is not the task's.
		{Role: "assistant", Kind: "tool_use", Content: `Bash {"command":"gh pr view 1"}`},
		{Role: "tool", Kind: "tool_result", Content: "https://github.com/o/r/pull/1"},
		{Role: "assistant", Kind: "tool_use", Content: `Bash {"command":"git push && gh pr create --title x"}`},
		{Role: "tool", Kind: "tool_result", Content: "Creating pull request\nhttps://github.com/o/r/pull/12\n"},
	}
	if _, err := a.StartSession(ctx, StartOpts{TaskID: task.ID, Prompt: "go"}); err != nil {
		t.Fatal(err)
	}
	a.Wait()
	task, _ = a.Store.Task(ctx, task.ID)
	if task.Fields["pull_request"] != "https://github.com/o/r/pull/12" {
		t.Fatalf("fields = %v", task.Fields)
	}

	// A PR the task already has stays.
	r.events = []agent.Event{
		{Role: "assistant", Kind: "tool_use", Content: `mcp__github__create_pull_request {"title":"y"}`},
		{Role: "tool", Kind: "tool_result", Content: `{"html_url":"https://github.com/o/r/pull/13"}`},
	}
	sessions, _ := a.Store.Sessions(ctx, task.ID, "")
	if _, err := a.SendMessage(ctx, sessions[0].ID, "another"); err != nil {
		t.Fatal(err)
	}
	a.Wait()
	task, _ = a.Store.Task(ctx, task.ID)
	if task.Fields["pull_request"] != "https://github.com/o/r/pull/12" {
		t.Fatalf("overwrote the PR: %v", task.Fields)
	}
}

func TestOpenedPRs(t *testing.T) {
	var o openedPRs
	o.see(agent.Event{Kind: "tool_use", Content: `mcp__github__create_pull_request {}`})
	o.see(agent.Event{Kind: "tool_result", Content: `{"html_url":"https://github.com/o/r/pull/13"}`})
	o.see(agent.Event{Kind: "tool_result", Content: "https://github.com/o/r/pull/14"})
	if len(o.urls) != 1 || o.urls[0] != "https://github.com/o/r/pull/13" {
		t.Fatalf("urls = %v", o.urls)
	}
}
