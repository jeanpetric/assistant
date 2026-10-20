package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

func TestMemoriesAndRevisions(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	m, err := s.CreateMemory(ctx, Memory{Name: "Productive API", Description: "How the API behaves", Body: "  - Auth: token header\n"})
	if err != nil {
		t.Fatal(err)
	}
	if m.Body != "- Auth: token header" || m.Revision == 0 {
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

	m.Name, m.Description = "API", "d"
	if err := s.UpdateMemory(ctx, m); err != nil {
		t.Fatal(err)
	}
	m.Name = "Empty"
	if err := s.UpdateMemory(ctx, m); !errors.Is(err, ErrMemoryExists) {
		t.Fatalf("rename onto another: %v", err)
	}
	if xs, _ := s.Memories(ctx); len(xs) != 2 || !slices.ContainsFunc(xs, func(x Memory) bool { return x.Name == "API" && x.Description == "d" }) {
		t.Fatalf("memories = %+v", xs)
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
	own, _ := s.CreateMemory(ctx, Memory{Name: "Home"})
	topic, _ := s.CreateMemory(ctx, Memory{Name: "Topic"})
	s.CreateMemory(ctx, Memory{Name: "Elsewhere"})

	// No memories given and building on nothing: none, whatever the project.
	a, err := s.CreateTask(ctx, Task{Title: "a", ProjectID: home})
	if err != nil || len(a.Memories) != 0 {
		t.Fatalf("a = %+v, %v", a.Memories, err)
	}
	if err := s.SetTaskMemories(ctx, a.ID, []int64{topic.ID, own.ID, topic.ID}); err != nil {
		t.Fatal(err)
	}
	// A follow-up in another project: the source's.
	b, _ := s.CreateTask(ctx, Task{Title: "b", ProjectID: other.ID, ContextFrom: []int64{a.ID}})
	if !slices.Equal(b.Memories, []int64{own.ID, topic.ID}) {
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

func TestMemoryProjectMigrationKeepsData(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	const n = 16 // migrations before memories lost project_id
	for _, m := range migrations[:n] {
		if _, err := db.Exec(m); err != nil {
			t.Fatal(err)
		}
	}
	for _, q := range []string{
		`PRAGMA user_version = 16`,
		`INSERT INTO tasks (id, title, lane_id, position, created_at, updated_at) VALUES (1, 'old', 1, 0, 0, 0)`,
		`INSERT INTO sessions (id, task_id, title, status, mode, created_at, updated_at) VALUES (7, 1, 'work', 'idle', 'plan', 0, 9000)`,
		`INSERT INTO memories (id, name, description, body, revision, project_id, created_at, updated_at) VALUES
			(3, 'API', 'The REST API', '- v2', 6, 1, 100, 200), (4, 'Empty', '', '', 0, NULL, 50, 50)`,
		`INSERT INTO memory_revisions (id, memory_id, body, source, session_id, task_id, created_at) VALUES
			(5, 3, '- v1', 'user', NULL, NULL, 100), (6, 3, '- v2', 'agent', 7, 1, 200)`,
		`INSERT INTO task_memories (task_id, memory_id) VALUES (1, 3), (1, 4)`,
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
	m, err := s.Memory(ctx, 3)
	if err != nil || m.Name != "API" || m.Description != "The REST API" || m.Body != "- v2" || m.Revision != 6 || m.Tasks != 1 ||
		m.CreatedAt.UnixMilli() != 100 || m.LastUsed.UnixMilli() != 9000 {
		t.Fatalf("memory = %+v, %v", m, err)
	}
	if revs, _ := s.MemoryRevisions(ctx, 3); len(revs) != 2 || revs[0].ID != 6 || *revs[0].SessionID != 7 || *revs[0].TaskID != 1 {
		t.Fatalf("revisions = %+v", revs)
	}
	if rev, prev, _ := s.MemoryRevision(ctx, 6); rev.Body != "- v2" || prev != "- v1" {
		t.Fatalf("revision = %+v, %q", rev, prev)
	}
	if task, _ := s.Task(ctx, 1); !slices.Equal(task.Memories, []int64{3, 4}) {
		t.Fatalf("links = %+v", task.Memories)
	}
	// The links still cascade and the name is still unique.
	if _, err := s.CreateMemory(ctx, Memory{Name: "api"}); !errors.Is(err, ErrMemoryExists) {
		t.Fatalf("duplicate: %v", err)
	}
	if err := s.DeleteMemory(ctx, 3); err != nil {
		t.Fatal(err)
	}
	if task, _ := s.Task(ctx, 1); !slices.Equal(task.Memories, []int64{4}) {
		t.Fatalf("after delete = %+v", task.Memories)
	}
	if revs, _ := s.MemoryRevisions(ctx, 3); len(revs) != 0 {
		t.Fatalf("revisions left = %+v", revs)
	}
}

func TestMemoriesLastUsedAndSearch(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	now := time.UnixMilli(1000)
	s.SetClock(func() time.Time { return now })
	tick := func() { now = now.Add(time.Minute) }

	api, _ := s.CreateMemory(ctx, Memory{Name: "Productive API", Description: "How the REST endpoints behave", Body: "- Auth: token header"})
	tick()
	cache, _ := s.CreateMemory(ctx, Memory{Name: "Caching", Description: "Redis setup", Body: "The API responses are cached for a minute."})
	tick()
	hr, _ := s.CreateMemory(ctx, Memory{Name: "Memorija", Body: "Bilješke o memoriji asistenta."})
	tick()

	// Newest first while nothing is used.
	order := func(xs []Memory) []int64 {
		var ids []int64
		for _, x := range xs {
			ids = append(ids, x.ID)
		}
		return ids
	}
	if xs, _ := s.Memories(ctx); !slices.Equal(order(xs), []int64{hr.ID, cache.ID, api.ID}) {
		t.Fatalf("by creation = %v", order(xs))
	}
	// Activity in a session of a linked task counts as use.
	task, _ := s.CreateTask(ctx, Task{Title: "t", Memories: []int64{api.ID}})
	tick()
	sess, err := s.CreateSession(ctx, Session{TaskID: task.ID, Title: "s", Mode: "plan"})
	if err != nil {
		t.Fatal(err)
	}
	if xs, _ := s.Memories(ctx); !slices.Equal(order(xs), []int64{api.ID, hr.ID, cache.ID}) || !xs[0].LastUsed.Equal(sess.UpdatedAt) {
		t.Fatalf("after a session = %v %+v", order(xs), xs[0])
	}
	// So does a new revision.
	tick()
	s.AppendMemoryNote(ctx, MemoryRevision{MemoryID: cache.ID, Body: "TTL 60s", Source: SourceNote})
	if xs, _ := s.Memories(ctx); !slices.Equal(order(xs), []int64{cache.ID, api.ID, hr.ID}) {
		t.Fatalf("after a revision = %v", order(xs))
	}

	search := func(q string) []int64 {
		t.Helper()
		xs, err := s.SearchMemories(ctx, q)
		if err != nil {
			t.Fatal(err)
		}
		return order(xs)
	}
	// The name weighs more than the body; ties go to the most recently used.
	if got := search("api"); !slices.Equal(got, []int64{api.ID, cache.ID}) {
		t.Fatalf("api = %v", got)
	}
	if got := search("Redis tokens"); !slices.Equal(got, []int64{cache.ID, api.ID}) {
		t.Fatalf("redis tokens = %v", got)
	}
	// Inflected forms match; stop words and short words alone find nothing.
	if got := search("Memoriju asistentu"); !slices.Equal(got, []int64{hr.ID}) {
		t.Fatalf("hr = %v", got)
	}
	if got := search("the and za je"); len(got) != 0 {
		t.Fatalf("stop words = %v", got)
	}
	// "cached" finds "Caching" in a name (3), "endpoints" a description (2).
	if xs, _ := s.SearchMemories(ctx, "cached endpoints"); len(xs) != 2 || xs[0].Score != 3 || xs[1].Score != 2 {
		t.Fatalf("scores = %+v", xs)
	}
}
