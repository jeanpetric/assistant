package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/jeanpetric/assistant/internal/store"
)

// rpc posts one JSON-RPC message to /mcp and returns the HTTP status and decoded body.
func (c *client) rpc(msg map[string]any) (int, map[string]any) {
	c.t.Helper()
	msg["jsonrpc"] = "2.0"
	b, _ := json.Marshal(msg)
	req, _ := http.NewRequest("POST", c.srv.URL+"/mcp", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	var out map[string]any
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &out); err != nil {
			c.t.Fatalf("decode %s: %v", raw, err)
		}
	}
	return res.StatusCode, out
}

// tool calls an MCP tool and returns its text and whether it is an error result.
func (c *client) tool(name string, args map[string]any) (string, bool) {
	c.t.Helper()
	status, out := c.rpc(map[string]any{"id": 1, "method": "tools/call", "params": map[string]any{"name": name, "arguments": args}})
	if status != 200 || out["error"] != nil {
		c.t.Fatalf("%s: status %d, %v", name, status, out)
	}
	res := out["result"].(map[string]any)
	text := res["content"].([]any)[0].(map[string]any)["text"].(string)
	return text, res["isError"].(bool)
}

func TestMCPHandshake(t *testing.T) {
	c := newClient(t)
	status, out := c.rpc(map[string]any{"id": 1, "method": "initialize", "params": map[string]any{
		"protocolVersion": "2025-03-26", "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "test", "version": "0"}}})
	if status != 200 {
		t.Fatalf("status %d", status)
	}
	res := out["result"].(map[string]any)
	if res["protocolVersion"] != "2025-03-26" || res["capabilities"].(map[string]any)["tools"] == nil {
		t.Fatalf("initialize: %v", res)
	}
	if out["id"].(float64) != 1 {
		t.Fatalf("id not echoed: %v", out)
	}
	_, out = c.rpc(map[string]any{"id": 2, "method": "initialize", "params": map[string]any{"protocolVersion": "1999-01-01"}})
	if v := out["result"].(map[string]any)["protocolVersion"]; v != mcpLatestVersion {
		t.Fatalf("unknown version answered with %v", v)
	}

	if status, _ := c.rpc(map[string]any{"method": "notifications/initialized"}); status != 202 {
		t.Fatalf("notification: status %d", status)
	}
	_, out = c.rpc(map[string]any{"id": "a", "method": "tools/list"})
	tools := out["result"].(map[string]any)["tools"].([]any)
	names := map[string]bool{}
	for _, x := range tools {
		tl := x.(map[string]any)
		names[tl["name"].(string)] = true
		if tl["inputSchema"].(map[string]any)["type"] != "object" || tl["description"] == "" {
			t.Fatalf("bad tool: %v", tl)
		}
	}
	for _, n := range []string{"get_board", "create_task", "update_task", "move_task", "delete_task", "start_session"} {
		if !names[n] {
			t.Fatalf("missing tool %s in %v", n, names)
		}
	}
	_, out = c.rpc(map[string]any{"id": 3, "method": "nope"})
	if out["error"].(map[string]any)["code"].(float64) != -32601 {
		t.Fatalf("unknown method: %v", out)
	}
	_, out = c.rpc(map[string]any{"id": 4, "method": "tools/call", "params": map[string]any{"name": "nope"}})
	if out["error"] == nil {
		t.Fatalf("unknown tool: %v", out)
	}
}

func TestMCPTaskTools(t *testing.T) {
	c := newClient(t)
	var board struct {
		Lanes []store.Lane `json:"lanes"`
	}
	text, isErr := c.tool("get_board", nil)
	if isErr || json.Unmarshal([]byte(text), &board) != nil || len(board.Lanes) < 2 {
		t.Fatalf("get_board: %s", text)
	}
	if strings.Contains(text, "running_limits") {
		t.Fatalf("board should drop UI-only data: %s", text)
	}
	second := board.Lanes[1].ID

	text, isErr = c.tool("create_task", map[string]any{"title": "From an agent", "description": "d",
		"lane_id": second, "fields": map[string]string{"pull_request": "https://example.com/pr/1"}})
	var task store.Task
	if isErr || json.Unmarshal([]byte(text), &task) != nil || task.LaneID != second || task.Fields["pull_request"] == "" {
		t.Fatalf("create_task: %s", text)
	}
	if text, isErr = c.tool("create_task", map[string]any{"title": " "}); !isErr || !strings.Contains(text, "title is required") {
		t.Fatalf("blank title: %s", text)
	}

	text, isErr = c.tool("update_task", map[string]any{"task_id": task.ID, "title": "Renamed", "due_at": "2030-01-02T09:00:00Z"})
	if isErr || !strings.Contains(text, `"title":"Renamed"`) || !strings.Contains(text, `"description":"d"`) || !strings.Contains(text, "2030-01-02") {
		t.Fatalf("update_task: %s", text)
	}
	if text, isErr = c.tool("update_task", map[string]any{"task_id": "x"}); !isErr || !strings.Contains(text, "task_id") {
		t.Fatalf("bad id: %s", text)
	}
	if text, isErr = c.tool("update_task", map[string]any{"task_id": 999, "title": "x"}); !isErr || !strings.HasPrefix(text, "404") {
		t.Fatalf("missing task: %s", text)
	}

	if text, isErr = c.tool("move_task", map[string]any{"task_id": task.ID, "lane_id": board.Lanes[0].ID}); isErr || !strings.Contains(text, "started") {
		t.Fatalf("move_task: %s", text)
	}
	if got, _ := c.app.Store.Task(t.Context(), task.ID); got.LaneID != board.Lanes[0].ID {
		t.Fatalf("not moved: %+v", got)
	}

	text, isErr = c.tool("start_session", map[string]any{"task_id": task.ID, "prompt": "hello"})
	var sess store.Session
	if isErr || json.Unmarshal([]byte(text), &sess) != nil || sess.TaskID != task.ID {
		t.Fatalf("start_session: %s", text)
	}
	c.app.Wait()
	if text, _ = c.tool("get_task", map[string]any{"task_id": task.ID}); strings.Contains(text, "did: hello") || !strings.Contains(text, `"sessions":[{`) {
		t.Fatalf("get_task should list sessions without transcripts: %s", text)
	}
	if text, _ = c.tool("get_session", map[string]any{"session_id": sess.ID}); !strings.Contains(text, "did: hello") {
		t.Fatalf("get_session: %s", text)
	}
	if text, _ = c.tool("list_sessions", map[string]any{"task_id": task.ID, "status": "idle"}); !strings.Contains(text, `"task_id":`) {
		t.Fatalf("list_sessions: %s", text)
	}

	if text, isErr = c.tool("archive_task", map[string]any{"task_id": task.ID}); isErr || !strings.Contains(text, `"archived_at":"`) {
		t.Fatalf("archive_task: %s", text)
	}
	if text, isErr = c.tool("delete_task", map[string]any{"task_id": task.ID}); isErr || text != "ok" {
		t.Fatalf("delete_task: %s %v", text, isErr)
	}
	if _, err := c.app.Store.Task(t.Context(), task.ID); err != store.ErrNotFound {
		t.Fatalf("task still there: %v", err)
	}
}

func TestMCPLaneTools(t *testing.T) {
	c := newClient(t)
	text, isErr := c.tool("create_lane", map[string]any{"name": "Blocked", "color": "#f00"})
	var lane store.Lane
	if isErr || json.Unmarshal([]byte(text), &lane) != nil || lane.Name != "Blocked" {
		t.Fatalf("create_lane: %s", text)
	}
	if text, isErr = c.tool("update_lane", map[string]any{"lane_id": lane.ID, "name": "Waiting"}); isErr {
		t.Fatalf("update_lane: %s", text)
	}
	if text, isErr = c.tool("delete_lane", map[string]any{"lane_id": lane.ID}); isErr {
		t.Fatalf("delete_lane: %s", text)
	}
}

func TestMCPTransport(t *testing.T) {
	c := newClient(t)
	res, err := http.Get(c.srv.URL + "/mcp")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 405 {
		t.Fatalf("GET /mcp: %d", res.StatusCode)
	}
	// The request guard covers /mcp too: a web page can't drive the tools.
	req, _ := http.NewRequest("POST", c.srv.URL+"/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "https://evil.example")
	res, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 403 {
		t.Fatalf("foreign origin: %d", res.StatusCode)
	}
	if status, out := c.rpc(map[string]any{"id": 1, "method": "ping"}); status != 200 || out["result"] == nil {
		t.Fatalf("ping: %d %v", status, out)
	}
}
