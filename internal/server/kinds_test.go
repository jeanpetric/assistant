package server

import (
	"slices"
	"strings"
	"testing"

	"github.com/jeanpetric/assistant/internal/store"
)

func kindNames(xs []store.Kind) []string {
	out := []string{}
	for _, x := range xs {
		out = append(out, x.Name)
	}
	return out
}

func TestKindsAPI(t *testing.T) {
	c := newClient(t)
	base := []string{"task", "coding", "research", "discussion", "reminder"}
	var kinds []store.Kind
	c.do("GET", "/api/kinds", nil, 200, &kinds)
	if !slices.Equal(kindNames(kinds), base) || !kinds[0].BuiltIn || kinds[1].BuiltIn || !kinds[4].BuiltIn {
		t.Fatalf("kinds = %+v", kinds)
	}

	for _, bad := range []string{"", "  ", "Bug", "1st", "a b", "x/y", strings.Repeat("a", 32)} {
		c.do("POST", "/api/kinds", map[string]any{"name": bad}, 400, nil)
	}
	c.do("POST", "/api/kinds", map[string]any{"name": " bug "}, 200, &kinds)
	if !slices.Equal(kindNames(kinds), append(slices.Clone(base), "bug")) {
		t.Fatalf("after add = %v", kindNames(kinds))
	}
	c.do("POST", "/api/kinds", map[string]any{"name": "bug"}, 409, nil)
	c.do("POST", "/api/kinds", map[string]any{"name": "a_b-1"}, 200, nil)

	var meta struct {
		Kinds []string `json:"kinds"`
	}
	c.do("GET", "/api/meta", nil, 200, &meta)
	if !slices.Contains(meta.Kinds, "bug") {
		t.Fatalf("meta kinds = %v", meta.Kinds)
	}
	var b struct {
		Kinds []string `json:"kinds"`
		Lanes []store.Lane
	}
	c.do("GET", "/api/board", nil, 200, &b)
	if !slices.Equal(b.Kinds, meta.Kinds) {
		t.Fatalf("board kinds = %v", b.Kinds)
	}

	// Tasks, rules and automations take known kinds only.
	var task store.Task
	c.do("POST", "/api/tasks", map[string]any{"title": "x", "kind": "nope"}, 400, nil)
	c.do("POST", "/api/tasks", map[string]any{"title": "plain"}, 201, &task)
	if task.Kind != "task" {
		t.Fatalf("default kind %q", task.Kind)
	}
	c.do("PATCH", "/api/tasks/"+itoa(task.ID), map[string]any{"kind": "nope"}, 400, nil)
	c.do("PATCH", "/api/tasks/"+itoa(task.ID), map[string]any{"kind": ""}, 400, nil)
	c.do("PATCH", "/api/tasks/"+itoa(task.ID), map[string]any{"kind": "bug"}, 200, &task)
	var other store.Task
	c.do("POST", "/api/tasks", map[string]any{"title": "other", "kind": "bug"}, 201, &other)
	c.do("POST", "/api/rules", map[string]any{"name": "r", "to_lane_id": b.Lanes[1].ID, "prompt": "x", "kind": "nope"}, 400, nil)
	c.do("POST", "/api/skills", map[string]any{"name": "s", "body": "x", "automations": []map[string]any{{"kind": "nope"}}}, 400, nil)

	// Built in, unknown.
	c.do("DELETE", "/api/kinds/task", nil, 409, nil)
	c.do("DELETE", "/api/kinds/reminder", nil, 409, nil)
	c.do("DELETE", "/api/kinds/nope", nil, 404, nil)

	// Used by tasks only: they become kind task.
	c.do("DELETE", "/api/kinds/bug", nil, 200, &kinds)
	if slices.Contains(kindNames(kinds), "bug") {
		t.Fatalf("bug still there: %v", kindNames(kinds))
	}
	for _, id := range []int64{task.ID, other.ID} {
		var d struct{ Task store.Task }
		c.do("GET", "/api/tasks/"+itoa(id), nil, 200, &d)
		if d.Task.Kind != "task" {
			t.Fatalf("task %d kind %q", id, d.Task.Kind)
		}
	}

	// Used by a rule or an automation: refused with the counts.
	c.do("POST", "/api/kinds", map[string]any{"name": "bug"}, 200, nil)
	var rule store.Rule
	c.do("POST", "/api/rules", map[string]any{"name": "r", "to_lane_id": b.Lanes[1].ID, "prompt": "x", "kind": "bug"}, 200, &rule)
	c.do("POST", "/api/skills", map[string]any{"name": "s", "body": "x", "automations": []map[string]any{{"kind": "bug"}}}, 200, nil)
	var e struct{ Error string }
	c.do("DELETE", "/api/kinds/bug", nil, 409, &e)
	if e.Error != "kind bug is used by 1 transition rule and 1 skill automation" {
		t.Fatalf("error %q", e.Error)
	}
	c.do("GET", "/api/kinds", nil, 200, &kinds)
	if k := kinds[len(kinds)-1]; k.Name != "bug" || k.Rules != 1 || k.Automations != 1 {
		t.Fatalf("kinds = %+v", kinds)
	}
}

func TestMCPKinds(t *testing.T) {
	c := newClient(t)
	if text, isErr := c.tool("add_kind", map[string]any{"name": "bug"}); isErr || !strings.Contains(text, `"name":"bug"`) {
		t.Fatalf("add_kind: %s", text)
	}
	if text, isErr := c.tool("add_kind", map[string]any{"name": "bug"}); !isErr || !strings.Contains(text, "kind bug exists") {
		t.Fatalf("add_kind twice: %s", text)
	}
	if text, isErr := c.tool("create_task", map[string]any{"title": "t", "kind": "bug"}); isErr || !strings.Contains(text, `"kind":"bug"`) {
		t.Fatalf("create_task: %s", text)
	}
	if text, isErr := c.tool("list_kinds", nil); isErr || !strings.Contains(text, `{"name":"bug","built_in":false,"tasks":1,"rules":0,"automations":0}`) {
		t.Fatalf("list_kinds: %s", text)
	}
	if text, _ := c.tool("get_board", nil); !strings.Contains(text, `"kinds":["task","coding","research","discussion","reminder","bug"]`) {
		t.Fatalf("get_board: %s", text)
	}
	if text, isErr := c.tool("delete_kind", map[string]any{"name": "task"}); !isErr || !strings.Contains(text, "kind task is built in") {
		t.Fatalf("delete_kind task: %s", text)
	}
	if text, isErr := c.tool("delete_kind", map[string]any{"name": "bug"}); isErr || strings.Contains(text, `"bug"`) {
		t.Fatalf("delete_kind: %s", text)
	}
}
