package server

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/jeanpetric/assistant/internal/store"
)

// Editing a workdir over the API or MCP changes this computer's, not the
// default shared with other computers; clearing it brings the default back.
func TestLocalWorkdirsAPI(t *testing.T) {
	c := newClient(t)
	shared, here, there := t.TempDir(), t.TempDir(), t.TempDir()

	var p store.Project
	c.do("POST", "/api/projects", map[string]any{"name": "P", "workdir": shared}, 201, &p)
	var task store.Task
	c.do("POST", "/api/tasks", map[string]any{"title": "t", "project_id": p.ID, "workdir": shared}, 201, &task)
	var rule store.Rule
	c.do("POST", "/api/rules", map[string]any{"name": "r", "to_lane_id": 3, "prompt": "p", "workdir": shared, "enabled": true}, 200, &rule)
	if p.Workdir != shared || p.WorkdirDefault != shared || task.Workdir != shared || task.WorkdirDefault != shared ||
		rule.Workdir != shared || rule.WorkdirDefault != shared {
		t.Fatalf("created: %+v %+v %+v", p, task, rule)
	}

	c.do("PATCH", "/api/projects/"+itoa(p.ID), map[string]any{"name": "P", "workdir": here}, 204, nil)
	c.do("PATCH", "/api/tasks/"+itoa(task.ID), map[string]any{"workdir": here}, 200, nil)
	c.do("PUT", "/api/rules/"+itoa(rule.ID), map[string]any{"name": "r", "to_lane_id": 3, "prompt": "p", "workdir": here, "enabled": true}, 200, &rule)
	var b board
	c.do("GET", "/api/board?project_id="+itoa(p.ID), nil, 200, &b)
	task = getTask(c, task.ID)
	if got := b.Projects[len(b.Projects)-1]; got.Workdir != here || got.WorkdirDefault != shared {
		t.Fatalf("project = %+v", got)
	}
	if task.Workdir != here || task.WorkdirDefault != shared || rule.Workdir != here || rule.WorkdirDefault != shared {
		t.Fatalf("task = %+v, rule = %+v", task, rule)
	}

	if text, isErr := c.tool("update_project", map[string]any{"project_id": p.ID, "name": "P", "workdir": there}); isErr {
		t.Fatalf("update_project: %s", text)
	}
	text, isErr := c.tool("update_task", map[string]any{"task_id": task.ID, "workdir": there})
	if isErr {
		t.Fatalf("update_task: %s", text)
	}
	json.Unmarshal([]byte(text), &task)
	if task.Workdir != there || task.WorkdirDefault != shared {
		t.Fatalf("task after update_task = %+v", task)
	}
	got, _ := c.app.Store.Project(t.Context(), p.ID)
	if got.Workdir != there || got.WorkdirDefault != shared {
		t.Fatalf("project after update_project = %+v", got)
	}

	// Clearing brings the shared default back.
	c.do("PATCH", "/api/tasks/"+itoa(task.ID), map[string]any{"workdir": ""}, 200, nil)
	if task = getTask(c, task.ID); task.Workdir != shared {
		t.Fatalf("cleared task = %+v", task)
	}
}

func getTask(c *client, id int64) store.Task {
	var out struct {
		Task store.Task `json:"task"`
	}
	c.do("GET", "/api/tasks/"+itoa(id), nil, 200, &out)
	return out.Task
}

// The workdirs offered are this computer's.
func TestSettingsWorkdirsLocal(t *testing.T) {
	c := newClient(t)
	dir := t.TempDir()
	var x store.Settings
	c.do("GET", "/api/settings", nil, 200, &x)
	c.do("PUT", "/api/settings", map[string]any{"workdirs": []string{dir}}, 200, nil)
	c.do("GET", "/api/settings", nil, 200, &x)
	if !slices.Equal(x.Workdirs, []string{dir}) {
		t.Fatalf("settings = %+v", x)
	}
}
