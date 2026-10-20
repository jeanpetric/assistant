package app

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/jeanpetric/assistant/internal/agent"
	"github.com/jeanpetric/assistant/internal/store"
)

func TestTaskBriefCarriesMemories(t *testing.T) {
	a, _ := newApp(t)
	ctx := context.Background()
	api, _ := a.Store.CreateMemory(ctx, store.Memory{Name: "API", Description: "The REST API", Body: "- Pages hold 200 items"})
	empty, _ := a.Store.CreateMemory(ctx, store.Memory{Name: "Empty"})
	src, _ := a.Store.CreateTask(ctx, store.Task{Title: "Earlier", Memories: []int64{}})
	task, _ := a.Store.CreateTask(ctx, store.Task{Title: "Now", Memories: []int64{api.ID, empty.ID}, ContextFrom: []int64{src.ID}})

	brief := a.TaskBrief(ctx, task)
	for _, want := range []string{MemoryIntro, "## Memory #" + itoa(api.ID) + ": API\n\nThe REST API\n\n- Pages hold 200 items",
		"## Memory #" + itoa(empty.ID) + ": Empty\n\n(Empty so far.)", SharedIntro} {
		if !strings.Contains(brief, want) {
			t.Fatalf("brief lacks %q:\n%s", want, brief)
		}
	}
	if strings.Index(brief, MemoryIntro) > strings.Index(brief, SharedIntro) {
		t.Fatal("memories come before the earlier tasks' results")
	}
	plain, _ := a.Store.CreateTask(ctx, store.Task{Title: "Plain", Memories: []int64{}})
	if strings.Contains(a.TaskBrief(ctx, plain), MemoryIntro) {
		t.Fatal("a task without memories gets none")
	}
	// A session that takes over from another starts with the memories too.
	if p := a.handoffPrompt(ctx, store.Session{TaskID: task.ID}, "brief", ""); !strings.Contains(p, "- Pages hold 200 items") {
		t.Fatalf("handoff prompt lacks the memory:\n%s", p)
	}
}

func TestMemoryUpdatedAfterTurn(t *testing.T) {
	a, r := newApp(t)
	ctx := context.Background()
	m, _ := a.Store.CreateMemory(ctx, store.Memory{Name: "API", Body: "- old fact"})
	other, _ := a.Store.CreateMemory(ctx, store.Memory{Name: "Other", Body: "- unrelated"})
	task, _ := a.Store.CreateTask(ctx, store.Task{Title: "Work", Workdir: "/repo", Memories: []int64{m.ID, other.ID}})
	r.result = func(req agent.Request) string {
		switch {
		case !req.Fork:
			return "done"
		case strings.Contains(req.Prompt, `memory "API"`):
			return "```markdown\n- old fact\n- new fact\n```"
		default:
			return NoChanges
		}
	}

	sess, _ := a.StartSession(ctx, StartOpts{TaskID: task.ID, Prompt: "dig", Mode: "bypassPermissions", Model: "sonnet"})
	a.Wait()
	var forks []agent.Request
	for _, req := range r.requests() {
		if req.Fork {
			forks = append(forks, req)
		}
	}
	if len(forks) != 2 {
		t.Fatalf("one fork per memory: %+v", r.requests())
	}
	f := forks[0]
	if f.Prompt != MemoryPrompt(m) || f.ResumeID != "claude-1" || f.Mode != "plan" || f.Model != "sonnet" || f.Workdir != "/repo" {
		t.Fatalf("memory request = %+v", f)
	}
	got, _ := a.Store.Memory(ctx, m.ID)
	if got.Body != "- old fact\n- new fact" {
		t.Fatalf("memory = %q", got.Body)
	}
	revs, _ := a.Store.MemoryRevisions(ctx, m.ID)
	if len(revs) != 2 || revs[0].Source != store.SourceAgent || *revs[0].SessionID != sess.ID || *revs[0].TaskID != task.ID {
		t.Fatalf("revisions = %+v", revs)
	}
	if revs, _ := a.Store.MemoryRevisions(ctx, other.ID); len(revs) != 1 {
		t.Fatalf("NO_CHANGES keeps the memory: %+v", revs)
	}
	s, _ := a.Store.Session(ctx, sess.ID)
	if s.CostUSD != 1.5 {
		t.Fatalf("cost should include both updates: %v", s.CostUSD)
	}
	if msgs, _ := a.Store.Messages(ctx, sess.ID, 0); len(msgs) != 3 {
		t.Fatalf("the update stays out of the transcript: %+v", msgs)
	}
	notes, _ := a.Store.Notifications(ctx, false)
	if len(notes) != 1 || notes[0].Message != `Memory "API" updated from session "dig"` || *notes[0].TaskID != task.ID {
		t.Fatalf("notifications = %+v", notes)
	}
	if a.Memorizing(sess.ID) {
		t.Fatal("done")
	}

	// A task without memories forks nothing.
	before := len(r.requests())
	plain, _ := a.Store.CreateTask(ctx, store.Task{Title: "Plain", Memories: []int64{}})
	a.StartSession(ctx, StartOpts{TaskID: plain.ID, Prompt: "go"})
	a.StartSession(ctx, StartOpts{Prompt: "standalone"})
	a.Wait()
	if n := len(r.requests()) - before; n != 2 {
		t.Fatalf("requests = %d", n)
	}
}

func TestMemoryEditedDuringUpdateIsAskedAgain(t *testing.T) {
	a, r := newApp(t)
	ctx := context.Background()
	m, _ := a.Store.CreateMemory(ctx, store.Memory{Name: "API", Body: "v1"})
	task, _ := a.Store.CreateTask(ctx, store.Task{Title: "Work", Memories: []int64{m.ID}})
	edited := false
	r.result = func(req agent.Request) string {
		if !req.Fork {
			return "done"
		}
		if !edited { // the user edits the memory while the agent writes
			edited = true
			a.Store.SetMemoryBody(ctx, store.MemoryRevision{MemoryID: m.ID, Body: "v2 by hand", Source: store.SourceUser}, nil)
			return "v1 + agent"
		}
		if !strings.Contains(req.Prompt, "v2 by hand") {
			t.Errorf("asked again with the old body: %s", req.Prompt)
		}
		return "v2 by hand + agent"
	}
	a.StartSession(ctx, StartOpts{TaskID: task.ID, Prompt: "dig"})
	a.Wait()
	if got, _ := a.Store.Memory(ctx, m.ID); got.Body != "v2 by hand + agent" {
		t.Fatalf("memory = %q", got.Body)
	}
}

func TestUpdateMemoriesByHand(t *testing.T) {
	a, r := newApp(t)
	ctx := context.Background()
	if err := a.UpdateMemories(ctx, 999); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("missing session: %v", err)
	}
	standalone, _ := a.StartSession(ctx, StartOpts{Prompt: "hi"})
	plain, _ := a.Store.CreateTask(ctx, store.Task{Title: "Plain", Memories: []int64{}})
	noMem, _ := a.StartSession(ctx, StartOpts{TaskID: plain.ID, Prompt: "hi"})
	a.Wait()
	for _, id := range []int64{standalone.ID, noMem.ID} {
		if err := a.UpdateMemories(ctx, id); !errors.Is(err, ErrNoMemory) {
			t.Fatalf("session %d: %v", id, err)
		}
	}

	m, _ := a.Store.CreateMemory(ctx, store.Memory{Name: "API"})
	task, _ := a.Store.CreateTask(ctx, store.Task{Title: "Work", Memories: []int64{}})
	sess, _ := a.StartSession(ctx, StartOpts{TaskID: task.ID, Prompt: "dig"})
	a.Wait()
	// Linked after the turn: only a manual update covers it.
	a.Store.SetTaskMemories(ctx, task.ID, []int64{m.ID})
	r.block = make(chan struct{})
	a.SendMessage(ctx, sess.ID, "more")
	if err := a.UpdateMemories(ctx, sess.ID); !errors.Is(err, ErrBusy) {
		t.Fatalf("running: %v", err)
	}
	a.Cancel(sess.ID)
	a.Wait()
	r.block = nil
	r.result = func(req agent.Request) string { return "- learned" }
	if err := a.UpdateMemories(ctx, sess.ID); err != nil {
		t.Fatal(err)
	}
	a.Wait()
	if got, _ := a.Store.Memory(ctx, m.ID); got.Body != "- learned" {
		t.Fatalf("memory = %q", got.Body)
	}
}

func TestMemoryFallbackAndFailure(t *testing.T) {
	a, r := newApp(t)
	ctx := context.Background()
	m, _ := a.Store.CreateMemory(ctx, store.Memory{Name: "API", Body: "- old"})
	task, _ := a.Store.CreateTask(ctx, store.Task{Title: "Work", Memories: []int64{m.ID}})
	r.fail = func(req agent.Request) error {
		if req.Fork {
			return errors.New("fork broke")
		}
		return nil
	}
	r.result = func(req agent.Request) string {
		if strings.HasPrefix(req.Prompt, "Below are the memory") {
			return "- old\n- from transcript"
		}
		return "done"
	}
	a.StartSession(ctx, StartOpts{TaskID: task.ID, Prompt: "dig"})
	a.Wait()
	reqs := r.requests()
	last := reqs[len(reqs)-1]
	if last.Model != SummaryModel || last.ResumeID != "" || !strings.Contains(last.Prompt, "<transcript>") || !strings.Contains(last.Prompt, "[User]\ndig") {
		t.Fatalf("fallback request = %+v", last)
	}
	if got, _ := a.Store.Memory(ctx, m.ID); got.Body != "- old\n- from transcript" {
		t.Fatalf("memory = %q", got.Body)
	}

	// Both ways failing leaves the memory as it was and notifies.
	r.fail = func(req agent.Request) error {
		if req.Fork || req.Model == SummaryModel {
			return errors.New("down")
		}
		return nil
	}
	sess, _ := a.StartSession(ctx, StartOpts{TaskID: task.ID, Prompt: "again"})
	a.Wait()
	if got, _ := a.Store.Memory(ctx, m.ID); got.Body != "- old\n- from transcript" {
		t.Fatalf("memory = %q", got.Body)
	}
	notes, _ := a.Store.Notifications(ctx, false)
	if len(notes) == 0 || !strings.HasPrefix(notes[0].Message, `Could not update memory "API" from session "again"`) {
		t.Fatalf("notifications = %+v", notes)
	}
	if a.Memorizing(sess.ID) {
		t.Fatal("done")
	}
}

func TestCleanMemory(t *testing.T) {
	for in, want := range map[string]string{
		"  - a\n- b \n":              "- a\n- b",
		"```markdown\n# A\n- b\n```": "# A\n- b",
		"```\nx\n```":                "x",
		"NO_CHANGES":                 NoChanges,
		"`NO_CHANGES`.":              NoChanges,
		"**NO_CHANGES**":             NoChanges,
		"- NO_CHANGES was a flag":    "- NO_CHANGES was a flag",
	} {
		if got := cleanMemory(in); got != want {
			t.Errorf("cleanMemory(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestLineDiff(t *testing.T) {
	got := LineDiff("a\nb\nc", "a\nc\nd")
	want := []DiffLine{{" ", "a"}, {"-", "b"}, {" ", "c"}, {"+", "d"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("diff = %+v", got)
	}
	if got := LineDiff("", "x"); !reflect.DeepEqual(got, []DiffLine{{"+", "x"}}) {
		t.Fatalf("from empty = %+v", got)
	}
	if got := LineDiff("x", "x"); !reflect.DeepEqual(got, []DiffLine{{" ", "x"}}) {
		t.Fatalf("same = %+v", got)
	}
	long := strings.Repeat("l\n", 2100)
	if got := LineDiff(long+"a", long+"b"); len(got) != 2*2101 || got[0].Op != "-" {
		t.Fatalf("beyond the bound the whole text changes: %d lines", len(got))
	}
}
