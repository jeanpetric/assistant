package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"slices"
	"testing"
)

func TestMemoriesAndRevisions(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	m, err := s.CreateMemory(ctx, Memory{Name: "Productive API", Description: "How the API behaves", Body: "  - Auth: token header\n"})
	if err != nil {
		t.Fatal(err)
	}
	if m.Body != "- Auth: token header" || m.Revision == 0 || m.ProjectID != nil {
		t.Fatalf("created = %+v", m)
	}
	if _, err := s.CreateMemory(ctx, Memory{Name: "productive api"}); !errors.Is(err, ErrMemoryExists) {
		t.Fatalf("duplicate name: %v", err)
	}
	empty, err := s.CreateMemory(ctx, Memory{Name: "Empty"})
	if err != nil || empty.Revision != 0 || empty.Body != "" {
		t.Fatalf("empty = %+v, %v", empty, err)
	}

	// A change based on the current revision lands; one based on an older fails.
	base := m.Revision
	rev, changed, err := s.SetMemoryBody(ctx, MemoryRevision{MemoryID: m.ID, Body: "- Auth: token header\n- Pages: 200 max", Source: SourceAgent}, &base)
	if err != nil || !changed || rev.ID == base || rev.Source != SourceAgent {
		t.Fatalf("set = %+v, %v, %v", rev, changed, err)
	}
	if _, _, err := s.SetMemoryBody(ctx, MemoryRevision{MemoryID: m.ID, Body: "lost", Source: SourceAgent}, &base); !errors.Is(err, ErrMemoryChanged) {
		t.Fatalf("stale base: %v", err)
	}
	// The same body again adds no revision.
	if _, changed, err := s.SetMemoryBody(ctx, MemoryRevision{MemoryID: m.ID, Body: "- Auth: token header\n- Pages: 200 max\n", Source: SourceUser}, nil); err != nil || changed {
		t.Fatalf("unchanged: %v, %v", changed, err)
	}
	if _, _, err := s.SetMemoryBody(ctx, MemoryRevision{MemoryID: 999, Body: "x"}, nil); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing memory: %v", err)
	}
	note, err := s.AppendMemoryNote(ctx, MemoryRevision{MemoryID: m.ID, Body: "Rate limit: 100/min", Source: SourceNote})
	if err != nil {
		t.Fatal(err)
	}
	if m, _ = s.Memory(ctx, m.ID); m.Body != "- Auth: token header\n- Pages: 200 max\n\nRate limit: 100/min" || m.Revision != note.ID {
		t.Fatalf("after note = %+v", m)
	}

	revs, err := s.MemoryRevisions(ctx, m.ID)
	if err != nil || len(revs) != 3 || revs[0].ID != note.ID || revs[0].Body != "" || revs[2].Source != SourceUser {
		t.Fatalf("revisions = %+v, %v", revs, err)
	}
	got, prev, err := s.MemoryRevision(ctx, rev.ID)
	if err != nil || got.Body != "- Auth: token header\n- Pages: 200 max" || prev != "- Auth: token header" {
		t.Fatalf("revision = %+v, prev %q, %v", got, prev, err)
	}
	if _, prev, _ := s.MemoryRevision(ctx, revs[2].ID); prev != "" {
		t.Fatalf("the first revision has no previous body: %q", prev)
	}

	p, _ := s.CreateProject(ctx, Project{Name: "Work"})
	m.Name, m.Description, m.ProjectID = "API", "d", &p.ID
	if err := s.UpdateMemory(ctx, m); err != nil {
		t.Fatal(err)
	}
	m.Name = "Empty"
	if err := s.UpdateMemory(ctx, m); !errors.Is(err, ErrMemoryExists) {
		t.Fatalf("rename onto another: %v", err)
	}
	if xs, _ := s.Memories(ctx); len(xs) != 2 || xs[0].Name != "API" || *xs[0].ProjectID != p.ID {
		t.Fatalf("memories = %+v", xs)
	}
	// Deleting the project only unlinks it.
	if err := s.DeleteProject(ctx, p.ID); err != nil {
		t.Fatal(err)
	}
	if m, _ = s.Memory(ctx, m.ID); m.ProjectID != nil {
		t.Fatalf("project gone: %+v", m)
	}
	if err := s.DeleteMemory(ctx, m.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Memory(ctx, m.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted: %v", err)
	}
	if revs, _ := s.MemoryRevisions(ctx, m.ID); len(revs) != 0 {
		t.Fatalf("revisions go with the memory: %+v", revs)
	}
}

func TestTaskMemories(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	projects, _ := s.Projects(ctx)
	home := projects[0].ID
	other, _ := s.CreateProject(ctx, Project{Name: "Other"})
	own, _ := s.CreateMemory(ctx, Memory{Name: "Home", ProjectID: &home})
	topic, _ := s.CreateMemory(ctx, Memory{Name: "Topic"})
	elsewhere, _ := s.CreateMemory(ctx, Memory{Name: "Elsewhere", ProjectID: &other.ID})

	// No memories given: the project's.
	a, err := s.CreateTask(ctx, Task{Title: "a", ProjectID: home})
	if err != nil || !slices.Equal(a.Memories, []int64{own.ID}) {
		t.Fatalf("a = %+v, %v", a.Memories, err)
	}
	if err := s.SetTaskMemories(ctx, a.ID, []int64{topic.ID, own.ID, topic.ID}); err != nil {
		t.Fatal(err)
	}
	// A follow-up in another project: that project's and the source's.
	b, _ := s.CreateTask(ctx, Task{Title: "b", ProjectID: other.ID, ContextFrom: []int64{a.ID}})
	if !slices.Equal(b.Memories, []int64{own.ID, topic.ID, elsewhere.ID}) {
		t.Fatalf("b = %+v", b.Memories)
	}
	// An explicit list, even empty, is kept as is.
	c, _ := s.CreateTask(ctx, Task{Title: "c", ProjectID: home, Memories: []int64{}})
	if len(c.Memories) != 0 {
		t.Fatalf("c = %+v", c.Memories)
	}
	if _, err := s.CreateTask(ctx, Task{Title: "d", Memories: []int64{999}}); !errors.Is(err, ErrBadMemory) {
		t.Fatalf("unknown memory: %v", err)
	}
	if err := s.SetTaskMemories(ctx, a.ID, []int64{999}); !errors.Is(err, ErrBadMemory) {
		t.Fatalf("unknown memory: %v", err)
	}

	tasks, _ := s.Tasks(ctx, 0)
	for _, x := range tasks {
		if x.ID == a.ID && !slices.Equal(x.Memories, []int64{own.ID, topic.ID}) {
			t.Fatalf("listed a = %+v", x.Memories)
		}
	}
	if ids, _ := s.MemoryTasks(ctx, topic.ID); !slices.Equal(ids, []int64{b.ID, a.ID}) {
		t.Fatalf("topic tasks = %v", ids)
	}
	if m, _ := s.Memory(ctx, topic.ID); m.Tasks != 2 {
		t.Fatalf("count = %d", m.Tasks)
	}
	// Deleting a memory unlinks it; deleting a task keeps the memory.
	s.DeleteMemory(ctx, topic.ID)
	if got, _ := s.Task(ctx, a.ID); !slices.Equal(got.Memories, []int64{own.ID}) {
		t.Fatalf("after delete = %+v", got.Memories)
	}
	s.DeleteTask(ctx, a.ID)
	if m, err := s.Memory(ctx, own.ID); err != nil || m.Tasks != 1 {
		t.Fatalf("own = %+v, %v", m, err)
	}
}

func TestMemoriesMigrationKeepsData(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	const n = 14 // migrations before memories
	for _, m := range migrations[:n] {
		if _, err := db.Exec(m); err != nil {
			t.Fatal(err)
		}
	}
	for _, q := range []string{
		`PRAGMA user_version = 14`,
		`INSERT INTO tasks (id, title, lane_id, position, created_at, updated_at) VALUES (1, 'old', 1, 0, 0, 0), (2, 'next', 1, 1, 0, 0)`,
		`INSERT INTO task_context (task_id, source_id, position) VALUES (2, 1, 0)`,
		`INSERT INTO sessions (id, task_id, title, status, mode, claude_session_id, cost_usd, summary, created_at, updated_at) VALUES (7, 1, 'work', 'idle', 'plan', 'c-7', 1.5, 'sum', 0, 5)`,
		`INSERT INTO messages (session_id, role, kind, content, created_at) VALUES (7, 'user', 'text', 'hi', 0)`,
		`UPDATE skills SET model = 'fable' WHERE id = 1`,
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
	old, err := s.Task(ctx, 1)
	if err != nil || old.Title != "old" || len(old.Memories) != 0 {
		t.Fatalf("old = %+v, %v", old, err)
	}
	if next, _ := s.Task(ctx, 2); !slices.Equal(next.ContextFrom, []int64{1}) {
		t.Fatalf("next = %+v", next)
	}
	if x, _ := s.Session(ctx, 7); x.Summary != "sum" || x.CostUSD != 1.5 || x.ClaudeSessionID != "c-7" {
		t.Fatalf("session = %+v", x)
	}
	if msgs, _ := s.Messages(ctx, 7, 0); len(msgs) != 1 {
		t.Fatalf("messages = %+v", msgs)
	}
	if sk, _ := s.Skill(ctx, 1); sk.Model != "fable" {
		t.Fatalf("skill = %+v", sk)
	}
	m, err := s.CreateMemory(ctx, Memory{Name: "New", Body: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetTaskMemories(ctx, 1, []int64{m.ID}); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Task(ctx, 1); !slices.Equal(got.Memories, []int64{m.ID}) {
		t.Fatalf("linked = %+v", got.Memories)
	}
}
