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
	if len(notes) != 0 {
		t.Fatalf("a successful update notifies nothing: %+v", notes)
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
	a.Cancel(ctx, sess.ID)
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

// A memory is a topic: task A on it updates it, and task B on the same topic,
// months later, finds it by its words, reads it and adds to it.
func TestMemoryTopicAcrossTasks(t *testing.T) {
	a, r := newApp(t)
	ctx := context.Background()
	facts := 0
	r.result = func(req agent.Request) string {
		if !req.Fork {
			return "done"
		}
		facts++
		body := req.Prompt[strings.Index(req.Prompt, "<memory>\n")+len("<memory>\n") : strings.Index(req.Prompt, "\n</memory>")]
		return strings.TrimSpace(body + "\n- fact " + itoa(int64(facts)))
	}
	m, _ := a.Store.CreateMemory(ctx, store.Memory{Name: "Invoice export", Description: "Exporting invoices to the accounting system"})
	other, _ := a.Store.CreateMemory(ctx, store.Memory{Name: "Caching", Body: "Redis keys"})
	taskA, _ := a.Store.CreateTask(ctx, store.Task{Title: "Export invoices as CSV", Memories: []int64{m.ID}})
	a.StartSession(ctx, StartOpts{TaskID: taskA.ID, Prompt: "build it"})
	a.Wait()
	if got, _ := a.Store.Memory(ctx, m.ID); got.Body != "- fact 1" {
		t.Fatalf("after A = %q", got.Body)
	}

	// Task B, in another project, is suggested the memory by its words.
	p, _ := a.Store.CreateProject(ctx, store.Project{Name: "Elsewhere"})
	title, desc := "Invoices exported twice", "The accounting export sends some invoices again"
	found, _ := a.Store.SearchMemories(ctx, title+" "+desc)
	if len(found) != 1 || found[0].ID != m.ID {
		t.Fatalf("suggested = %+v (other is %d)", found, other.ID)
	}
	taskB, _ := a.Store.CreateTask(ctx, store.Task{Title: title, Description: desc, ProjectID: p.ID, Memories: []int64{found[0].ID}})
	if brief := a.TaskBrief(ctx, taskB); !strings.Contains(brief, "## Memory #"+itoa(m.ID)+": Invoice export") || !strings.Contains(brief, "- fact 1") {
		t.Fatalf("B's brief:\n%s", brief)
	}
	sessB, _ := a.StartSession(ctx, StartOpts{TaskID: taskB.ID, Prompt: "fix it"})
	a.Wait()
	if got, _ := a.Store.Memory(ctx, m.ID); got.Body != "- fact 1\n- fact 2" || got.Tasks != 2 {
		t.Fatalf("after B = %+v", got)
	}
	revs, _ := a.Store.MemoryRevisions(ctx, m.ID)
	if len(revs) != 2 || *revs[0].TaskID != taskB.ID || *revs[0].SessionID != sessB.ID || *revs[1].TaskID != taskA.ID {
		t.Fatalf("revisions = %+v", revs)
	}
}

func TestFillMemoryFromTaskSessions(t *testing.T) {
	a, r := newApp(t)
	ctx := context.Background()
	task, _ := a.Store.CreateTask(ctx, store.Task{Title: "Work", Memories: []int64{}})
	first, _ := a.StartSession(ctx, StartOpts{TaskID: task.ID, Prompt: "first"})
	a.Wait()
	second, _ := a.StartSession(ctx, StartOpts{TaskID: task.ID, Prompt: "second"})
	a.Wait()
	before := len(r.requests())

	var seen []string
	r.result = func(req agent.Request) string {
		body := req.Prompt[strings.Index(req.Prompt, "<memory>\n")+len("<memory>\n") : strings.Index(req.Prompt, "\n</memory>")]
		seen = append(seen, body)
		return strings.TrimSpace(body + "\n- from a session")
	}
	m, _ := a.Store.CreateMemory(ctx, store.Memory{Name: "Topic"})
	a.Store.SetTaskMemories(ctx, task.ID, []int64{m.ID})
	a.FillMemory(task.ID, m.ID)
	a.Wait()
	if a.Filling(m.ID) {
		t.Fatal("done")
	}
	// Oldest session first, each building on the last one's version.
	if !reflect.DeepEqual(seen, []string{"", "- from a session"}) || len(r.requests())-before != 2 {
		t.Fatalf("seen = %q", seen)
	}
	revs, _ := a.Store.MemoryRevisions(ctx, m.ID)
	if len(revs) != 2 || *revs[1].SessionID != first.ID || *revs[0].SessionID != second.ID {
		t.Fatalf("revisions = %+v", revs)
	}
	notes, _ := a.Store.Notifications(ctx, false)
	if len(notes) == 0 || notes[0].Message != `Memory "Topic" written from the task's sessions` {
		t.Fatalf("notifications = %+v", notes)
	}
}
