package server

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/jeanpetric/assistant/internal/store"
)

func TestSessionPromptsAPI(t *testing.T) {
	c := newClient(t)
	var xs []store.SessionPrompt
	c.do("GET", "/api/session-prompts", nil, 200, &xs)
	if len(xs) != 1 || xs[0].Name != "Default" || xs[0].Prompt != store.DefaultSessionPrompt || !xs[0].AllProjects || len(xs[0].ProjectIDs) != 0 {
		t.Fatalf("default prompts = %+v", xs)
	}
	var web, docs store.Project
	c.do("POST", "/api/projects", map[string]any{"name": "Web"}, 201, &web)
	c.do("POST", "/api/projects", map[string]any{"name": "Docs"}, 201, &docs)

	// Name and a valid template are required, and known projects only.
	c.do("POST", "/api/session-prompts", map[string]any{"name": " ", "prompt": "x"}, 400, nil)
	c.do("POST", "/api/session-prompts", map[string]any{"name": "a", "prompt": "{{.Project.Name"}, 400, nil)
	c.do("POST", "/api/session-prompts", map[string]any{"name": "a", "prompt": "x", "all_projects": false, "project_ids": []int64{999}}, 400, nil)

	// all_projects defaults to true; project ids are deduplicated.
	var all, some store.SessionPrompt
	c.do("POST", "/api/session-prompts", map[string]any{"name": " Everywhere ", "prompt": "hi"}, 200, &all)
	if all.ID == 0 || all.Name != "Everywhere" || !all.AllProjects {
		t.Fatalf("created = %+v", all)
	}
	c.do("POST", "/api/session-prompts", map[string]any{"name": "Web only", "prompt": "In {{.Project.Name}}", "all_projects": false,
		"project_ids": []int64{web.ID, web.ID}}, 200, &some)
	if some.AllProjects || !slices.Equal(some.ProjectIDs, []int64{web.ID}) {
		t.Fatalf("created = %+v", some)
	}

	// PATCH changes the given keys only.
	c.do("PATCH", fmt.Sprintf("/api/session-prompts/%d", some.ID), map[string]any{"project_ids": []int64{docs.ID, web.ID}}, 200, &some)
	if some.Name != "Web only" || some.Prompt != "In {{.Project.Name}}" || some.AllProjects || !slices.Equal(some.ProjectIDs, []int64{docs.ID, web.ID}) {
		t.Fatalf("patched = %+v", some)
	}
	c.do("PATCH", fmt.Sprintf("/api/session-prompts/%d", some.ID), map[string]any{"name": "Two"}, 200, &some)
	if some.Name != "Two" || len(some.ProjectIDs) != 2 {
		t.Fatalf("patched = %+v", some)
	}
	c.do("PATCH", fmt.Sprintf("/api/session-prompts/%d", some.ID), map[string]any{"prompt": "{{"}, 400, nil)
	c.do("PATCH", "/api/session-prompts/999", map[string]any{"name": "x"}, 404, nil)

	// A deleted project drops out of the lists.
	c.do("DELETE", fmt.Sprintf("/api/projects/%d", docs.ID), nil, 204, nil)
	c.do("GET", "/api/session-prompts", nil, 200, &xs)
	if len(xs) != 3 || xs[2].ID != some.ID || !slices.Equal(xs[2].ProjectIDs, []int64{web.ID}) {
		t.Fatalf("prompts = %+v", xs)
	}

	c.do("DELETE", fmt.Sprintf("/api/session-prompts/%d", all.ID), nil, 204, nil)
	c.do("DELETE", fmt.Sprintf("/api/session-prompts/%d", all.ID), nil, 404, nil)
	c.do("GET", "/api/session-prompts", nil, 200, &xs)
	if len(xs) != 2 {
		t.Fatalf("after delete = %+v", xs)
	}
}

func TestSessionPromptsMCP(t *testing.T) {
	c := newClient(t)
	var web store.Project
	c.do("POST", "/api/projects", map[string]any{"name": "Web"}, 201, &web)
	text, isErr := c.tool("create_session_prompt", map[string]any{"name": "Web", "prompt": "In web", "all_projects": false, "project_ids": []int64{web.ID}})
	var x store.SessionPrompt
	if isErr || json.Unmarshal([]byte(text), &x) != nil || x.AllProjects || !slices.Equal(x.ProjectIDs, []int64{web.ID}) {
		t.Fatalf("create: %s", text)
	}
	if text, isErr = c.tool("update_session_prompt", map[string]any{"session_prompt_id": x.ID, "all_projects": true}); isErr || !strings.Contains(text, `"all_projects":true`) {
		t.Fatalf("update: %s", text)
	}
	if text, isErr = c.tool("list_session_prompts", map[string]any{}); isErr || !strings.Contains(text, `"name":"Web"`) {
		t.Fatalf("list: %s", text)
	}
	if text, isErr = c.tool("delete_session_prompt", map[string]any{"session_prompt_id": x.ID}); isErr {
		t.Fatalf("delete: %s", text)
	}
	var xs []store.SessionPrompt
	c.do("GET", "/api/session-prompts", nil, 200, &xs)
	if len(xs) != 1 {
		t.Fatalf("after delete = %+v", xs)
	}
}
