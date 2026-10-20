package server

import (
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/jeanpetric/assistant/internal/app"
	"github.com/jeanpetric/assistant/internal/store"
)

func TestMemoriesAPI(t *testing.T) {
	c := newClient(t)
	var m store.Memory
	c.do("POST", "/api/memories", map[string]any{"name": " API ", "description": "The REST API", "body": "- v1"}, 201, &m)
	if m.Name != "API" || m.Body != "- v1" || m.Revision == 0 {
		t.Fatalf("created = %+v", m)
	}
	c.do("POST", "/api/memories", map[string]any{"name": "api"}, 400, nil)
	c.do("POST", "/api/memories", map[string]any{"name": " "}, 400, nil)
	var other store.Memory
	c.do("POST", "/api/memories", map[string]any{"name": "Other"}, 201, &other)
	if other.Revision != 0 {
		t.Fatalf("other = %+v", other)
	}

	// A new task gets the memories it names; none without.
	var task, bare store.Task
	c.do("POST", "/api/tasks", map[string]any{"title": "Work", "memories": []int64{m.ID}}, 201, &task)
	if !slices.Equal(task.Memories, []int64{m.ID}) {
		t.Fatalf("task memories = %v", task.Memories)
	}
	c.do("POST", "/api/tasks", map[string]any{"title": "Bare"}, 201, &bare)
	if len(bare.Memories) != 0 {
		t.Fatalf("bare memories = %v", bare.Memories)
	}
	c.do("POST", "/api/tasks", map[string]any{"title": "Bad", "memories": []int64{999}}, 400, nil)
	c.do("PATCH", "/api/tasks/"+itoa(task.ID), map[string]any{"memories": []int64{m.ID, other.ID}}, 200, &task)
	if !slices.Equal(task.Memories, []int64{m.ID, other.ID}) {
		t.Fatalf("patched = %v", task.Memories)
	}
	c.do("PATCH", "/api/tasks/"+itoa(task.ID), map[string]any{"memories": []int64{999}}, 400, nil)
	var page struct {
		Memories []memoryRef `json:"memories"`
	}
	c.do("GET", "/api/tasks/"+itoa(task.ID), nil, 200, &page)
	if len(page.Memories) != 2 || page.Memories[0].Name != "API" {
		t.Fatalf("task page memories = %+v", page.Memories)
	}

	// Edits by hand: a stale base is refused.
	base := m.Revision
	c.do("PATCH", "/api/memories/"+itoa(m.ID), map[string]any{"body": "- v2", "base_revision": base}, 200, &m)
	c.do("PATCH", "/api/memories/"+itoa(m.ID), map[string]any{"body": "- lost", "base_revision": base}, 409, nil)
	c.do("PATCH", "/api/memories/"+itoa(m.ID), map[string]any{"description": "Productive API"}, 200, &m)
	if m.Body != "- v2" || m.Description != "Productive API" || m.Name != "API" {
		t.Fatalf("patched = %+v", m)
	}
	c.do("PATCH", "/api/memories/"+itoa(m.ID), map[string]any{"name": "other"}, 400, nil)
	c.do("POST", "/api/memories/"+itoa(m.ID)+"/notes", map[string]any{"text": "Rate limit 100/min"}, 200, &m)
	c.do("POST", "/api/memories/"+itoa(m.ID)+"/notes", map[string]any{"text": " "}, 400, nil)
	if m.Body != "- v2\n\nRate limit 100/min" {
		t.Fatalf("noted = %q", m.Body)
	}

	// A task turn updates its memories from a fork of the session.
	var sess store.Session
	c.do("POST", "/api/tasks/"+itoa(task.ID)+"/sessions", map[string]any{"prompt": "dig", "include_context": true}, 201, &sess)
	c.app.Wait()
	var full struct {
		Memory    store.Memory           `json:"memory"`
		Revisions []store.MemoryRevision `json:"revisions"`
		Tasks     []taskRef              `json:"tasks"`
	}
	c.do("GET", "/api/memories/"+itoa(m.ID), nil, 200, &full)
	if full.Memory.Body != "SUMMARY of c-1" || len(full.Revisions) != 4 || full.Revisions[0].Source != "agent" ||
		len(full.Tasks) != 1 || full.Tasks[0].ID != task.ID {
		t.Fatalf("memory = %+v", full)
	}
	var msgs struct {
		Messages []store.Message `json:"messages"`
	}
	c.do("GET", "/api/sessions/"+itoa(sess.ID), nil, 200, &msgs)
	if !strings.Contains(msgs.Messages[0].Content, app.MemoryIntro) || !strings.Contains(msgs.Messages[0].Content, "Rate limit 100/min") {
		t.Fatalf("the brief carries the memory: %s", msgs.Messages[0].Content)
	}
	c.do("POST", "/api/sessions/"+itoa(sess.ID)+"/memory", nil, 202, nil)
	c.app.Wait()
	var bareSess store.Session
	c.do("POST", "/api/tasks/"+itoa(bare.ID)+"/sessions", map[string]any{"prompt": "go"}, 201, &bareSess)
	c.app.Wait()
	c.do("POST", "/api/sessions/"+itoa(bareSess.ID)+"/memory", nil, 400, nil)
	c.do("POST", "/api/sessions/999/memory", nil, 404, nil)

	// Revisions: a diff to the one before, and a restore.
	var rev struct {
		Revision store.MemoryRevision `json:"revision"`
		Diff     []app.DiffLine       `json:"diff"`
	}
	c.do("GET", "/api/memory-revisions/"+itoa(full.Revisions[0].ID), nil, 200, &rev)
	want := []app.DiffLine{{Op: "-", Text: "- v2"}, {Op: "-", Text: ""}, {Op: "-", Text: "Rate limit 100/min"}, {Op: "+", Text: "SUMMARY of c-1"}}
	if rev.Revision.Body != "SUMMARY of c-1" || !slices.Equal(rev.Diff, want) {
		t.Fatalf("revision = %+v", rev)
	}
	c.do("POST", "/api/memory-revisions/"+itoa(full.Revisions[1].ID)+"/restore", nil, 200, &m)
	if m.Body != "- v2\n\nRate limit 100/min" {
		t.Fatalf("restored = %q", m.Body)
	}
	c.do("GET", "/api/memory-revisions/999", nil, 404, nil)

	var list []store.Memory
	c.do("GET", "/api/memories", nil, 200, &list)
	if len(list) != 2 || list[0].Name != "API" || list[0].Body != "" || list[0].Tasks != 1 || list[0].LastUsed.IsZero() {
		t.Fatalf("list = %+v", list)
	}
	// A search finds by words, the body's too, best first; bodies stay out.
	c.do("GET", "/api/memories?q="+url.QueryEscape("rate limits of the REST api"), nil, 200, &list)
	if len(list) != 1 || list[0].ID != m.ID || list[0].Score == 0 || list[0].Body != "" {
		t.Fatalf("search = %+v", list)
	}
	c.do("GET", "/api/memories?q=", nil, 200, &list)
	if len(list) != 0 {
		t.Fatalf("empty search = %+v", list)
	}
	c.do("DELETE", "/api/memories/"+itoa(other.ID), nil, 204, nil)
	c.do("DELETE", "/api/memories/"+itoa(other.ID), nil, 404, nil)
	c.do("GET", "/api/tasks/"+itoa(task.ID), nil, 200, &page)
	if len(page.Memories) != 1 {
		t.Fatalf("deleted memory unlinked: %+v", page.Memories)
	}
}

func TestCreateMemoryForTask(t *testing.T) {
	c := newClient(t)
	var existing, task store.Task
	var m store.Memory
	c.do("POST", "/api/memories", map[string]any{"name": "Old"}, 201, &m)
	c.do("POST", "/api/tasks", map[string]any{"title": "Work", "memories": []int64{m.ID}}, 201, &task)
	var sess store.Session
	c.do("POST", "/api/tasks/"+itoa(task.ID)+"/sessions", map[string]any{"prompt": "dig"}, 201, &sess)
	c.app.Wait()
	c.do("POST", "/api/tasks", map[string]any{"title": "Other"}, 201, &existing)

	// Linked to the task next to its other memory, and written from its session.
	var created store.Memory
	c.do("POST", "/api/memories", map[string]any{"name": "Topic", "task_id": task.ID, "fill": true}, 201, &created)
	if created.Tasks != 1 {
		t.Fatalf("created = %+v", created)
	}
	c.app.Wait()
	var full struct {
		Memory    store.Memory           `json:"memory"`
		Revisions []store.MemoryRevision `json:"revisions"`
		Filling   bool                   `json:"filling"`
	}
	c.do("GET", "/api/memories/"+itoa(created.ID), nil, 200, &full)
	if full.Memory.Body != "SUMMARY of c-1" || len(full.Revisions) != 1 || *full.Revisions[0].SessionID != sess.ID || full.Filling {
		t.Fatalf("filled = %+v", full)
	}
	c.do("GET", "/api/tasks/"+itoa(task.ID), nil, 200, &struct{ Task *store.Task }{&task})
	if !slices.Equal(task.Memories, []int64{m.ID, created.ID}) {
		t.Fatalf("task memories = %v", task.Memories)
	}
	// Without fill it starts empty; an unknown task creates nothing.
	c.do("POST", "/api/memories", map[string]any{"name": "Empty", "task_id": existing.ID}, 201, &created)
	c.app.Wait()
	if c.do("GET", "/api/memories/"+itoa(created.ID), nil, 200, &full); full.Memory.Body != "" || full.Memory.Tasks != 1 {
		t.Fatalf("empty = %+v", full)
	}
	c.do("POST", "/api/memories", map[string]any{"name": "Nowhere", "task_id": 999}, 400, nil)
	var list []store.Memory
	c.do("GET", "/api/memories", nil, 200, &list)
	if len(list) != 3 {
		t.Fatalf("list = %+v", list)
	}
}

func TestMCPMemories(t *testing.T) {
	c := newClient(t)
	text, isErr := c.tool("create_memory", map[string]any{"name": "API", "description": "The REST API"})
	if isErr || !strings.Contains(text, `"name":"API"`) {
		t.Fatalf("create: %s", text)
	}
	if text, isErr = c.tool("remember", map[string]any{"memory_id": 1, "text": "Pages hold 200"}); isErr || !strings.Contains(text, `"body":"Pages hold 200"`) {
		t.Fatalf("remember: %s", text)
	}
	if text, isErr = c.tool("update_memory", map[string]any{"memory_id": 1, "body": "x", "base_revision": 999}); !isErr || !strings.HasPrefix(text, "409") {
		t.Fatalf("stale update: %s", text)
	}
	if text, isErr = c.tool("create_task", map[string]any{"title": "Work", "memories": []int{1}}); isErr || !strings.Contains(text, `"memories":[1]`) {
		t.Fatalf("create_task: %s", text)
	}
	if text, isErr = c.tool("get_task", map[string]any{"task_id": 1}); isErr || !strings.Contains(text, `"memories":[{"id":1,"name":"API"}]`) {
		t.Fatalf("get_task: %s", text)
	}
	if text, isErr = c.tool("list_memories", map[string]any{}); isErr || strings.Contains(text, "Pages") || !strings.Contains(text, `"tasks":1`) {
		t.Fatalf("list: %s", text)
	}
	if text, isErr = c.tool("search_memories", map[string]any{"q": "the rest api"}); isErr || !strings.Contains(text, `"name":"API"`) || !strings.Contains(text, `"score":`) {
		t.Fatalf("search: %s", text)
	}
	if text, isErr = c.tool("search_memories", map[string]any{"q": "redis"}); isErr || text != "[]" {
		t.Fatalf("search nothing: %s", text)
	}
	if text, isErr = c.tool("create_memory", map[string]any{"name": "Topic", "task_id": 1}); isErr || !strings.Contains(text, `"tasks":1`) {
		t.Fatalf("create for a task: %s", text)
	}
	if text, isErr = c.tool("get_memory", map[string]any{"memory_id": 1}); isErr || !strings.Contains(text, `"source":"note"`) {
		t.Fatalf("get: %s", text)
	}
	if text, isErr = c.tool("update_memories_from_session", map[string]any{"session_id": 9}); !isErr || !strings.HasPrefix(text, "404") {
		t.Fatalf("no session: %s", text)
	}
	if _, isErr = c.tool("delete_memory", map[string]any{"memory_id": 1}); isErr {
		t.Fatal("delete")
	}
}
