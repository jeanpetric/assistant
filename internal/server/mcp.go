package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
)

// MCP (Model Context Protocol) over the Streamable HTTP transport: clients POST
// JSON-RPC messages to /mcp and get plain JSON back (no SSE streams, no sessions).
// Every tool is a call into the JSON API below, so it gets the same validation
// and behaviour (rules on lane moves, etc.) as the web UI.

const mcpLatestVersion = "2025-06-18"

var mcpVersions = []string{"2025-11-25", "2025-06-18", "2025-03-26", "2024-11-05"}

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"` // absent: a notification
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

// mcpTool describes a tool and maps its arguments to an API request.
type mcpTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
	call        func(a mcpArgs) (method, path string, body any, err error)
	// post shapes the API response for the agent; nil passes it through.
	post func(raw []byte) ([]byte, error)
}

type mcpArgs map[string]json.RawMessage

func (a mcpArgs) id(key string) (int64, error) {
	var id int64
	if err := json.Unmarshal(a[key], &id); err != nil || id <= 0 {
		return 0, fmt.Errorf("%s must be a positive integer", key)
	}
	return id, nil
}

// without returns the arguments minus the given keys, as an API body.
func (a mcpArgs) without(keys ...string) map[string]json.RawMessage {
	out := map[string]json.RawMessage{}
	for k, v := range a {
		out[k] = v
	}
	for _, k := range keys {
		delete(out, k)
	}
	return out
}

// query builds a query string from the given integer/string arguments that are present.
func (a mcpArgs) query(keys ...string) (string, error) {
	q := url.Values{}
	for _, k := range keys {
		if a[k] == nil {
			continue
		}
		var v any
		if err := json.Unmarshal(a[k], &v); err != nil {
			return "", fmt.Errorf("invalid %s", k)
		}
		switch v := v.(type) {
		case float64:
			q.Set(k, strconv.FormatInt(int64(v), 10))
		case string:
			q.Set(k, v)
		}
	}
	if len(q) == 0 {
		return "", nil
	}
	return "?" + q.Encode(), nil
}

func schema(required []string, props map[string]any) map[string]any {
	s := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		s["required"] = required
	}
	return s
}

func prop(typ, desc string) map[string]any { return map[string]any{"type": typ, "description": desc} }

var (
	pTaskID    = prop("integer", "Task id")
	pLaneID    = prop("integer", "Lane id (see get_board)")
	pProjectID = prop("integer", "Project id (see get_board); every project has its own board on the shared lanes")
	pFields    = map[string]any{"type": "object", "additionalProperties": map[string]any{"type": "string"}, "description": "Custom fields by key, e.g. {\"pull_request\": \"https://…\"}; see get_board for the keys. An empty value removes a field."}
	pDueAt     = prop("string", "Due time, RFC 3339 (e.g. 2026-10-21T09:00:00+02:00); a notification fires when it's reached")
	pKind      = map[string]any{"type": "string", "enum": []string{"task", "coding", "research", "discussion", "reminder"}}
	pWorkdir   = prop("string", "Absolute working directory for agent sessions")
)

func (s *Server) mcpTools() []mcpTool {
	byID := func(method, format, key string, body bool) func(mcpArgs) (string, string, any, error) {
		return func(a mcpArgs) (string, string, any, error) {
			id, err := a.id(key)
			if err != nil {
				return "", "", nil, err
			}
			var b any
			if body {
				b = a.without(key)
			}
			return method, fmt.Sprintf(format, id), b, nil
		}
	}
	fixed := func(method, path string, body bool) func(mcpArgs) (string, string, any, error) {
		return func(a mcpArgs) (string, string, any, error) {
			if body {
				return method, path, a, nil
			}
			return method, path, nil, nil
		}
	}
	// list is a GET whose arguments are query parameters.
	list := func(path string, keys ...string) func(mcpArgs) (string, string, any, error) {
		return func(a mcpArgs) (string, string, any, error) {
			q, err := a.query(keys...)
			return "GET", path + q, nil, err
		}
	}
	return []mcpTool{
		{
			Name:        "get_board",
			Description: "The kanban board of one project (the first if project_id is left out): every project, the lanes (in order), the project's tasks on the board, custom field definitions and how many agent sessions run per task. Start here to find project, lane and task ids.",
			InputSchema: schema(nil, map[string]any{"project_id": pProjectID}),
			call:        list("/api/board", "project_id"),
			post:        boardSummary,
		},
		{
			Name:        "get_task",
			Description: "One task with its schedules and agent sessions (without transcripts; use get_session for those).",
			InputSchema: schema([]string{"task_id"}, map[string]any{"task_id": pTaskID}),
			call:        byID("GET", "/api/tasks/%d", "task_id", false),
			post:        taskWithoutTranscripts,
		},
		{
			Name:        "create_task",
			Description: "Add a task to a project's board (the first project if project_id is left out), at the end of a lane (the first lane if lane_id is left out).",
			InputSchema: schema([]string{"title"}, map[string]any{
				"title": prop("string", "Short title"), "description": prop("string", "Markdown description"), "project_id": pProjectID,
				"kind": pKind, "lane_id": pLaneID, "workdir": pWorkdir, "due_at": pDueAt, "fields": pFields,
			}),
			call: fixed("POST", "/api/tasks", true),
		},
		{
			Name:        "update_task",
			Description: "Change a task's title, description, kind, workdir, due time, custom fields or project (it goes to the end of its lane there). Only the given keys change; due_at null clears it. Use move_task to change its lane.",
			InputSchema: schema([]string{"task_id"}, map[string]any{
				"task_id": pTaskID, "project_id": pProjectID, "title": prop("string", "New title"), "description": prop("string", "New description (replaces the old one)"),
				"kind": pKind, "workdir": pWorkdir, "due_at": map[string]any{"type": []string{"string", "null"}, "description": pDueAt["description"]}, "fields": pFields,
			}),
			call: byID("PATCH", "/api/tasks/%d", "task_id", true),
		},
		{
			Name:        "move_task",
			Description: "Move a task to a lane, at a position (0 = top). Transition rules for that lane may start agent sessions; they are listed in the result.",
			InputSchema: schema([]string{"task_id", "lane_id"}, map[string]any{
				"task_id": pTaskID, "lane_id": pLaneID, "index": prop("integer", "Position in the lane, 0 = top (default)"),
			}),
			call: byID("POST", "/api/tasks/%d/move", "task_id", true),
		},
		{
			Name:        "delete_task",
			Description: "Delete a task for good, with its sessions and schedules; running agents are stopped. Prefer archive_task unless asked to delete.",
			InputSchema: schema([]string{"task_id"}, map[string]any{"task_id": pTaskID}),
			call:        byID("DELETE", "/api/tasks/%d", "task_id", false),
		},
		{
			Name:        "archive_task",
			Description: "Take a task off the board into the Archive; it can be restored.",
			InputSchema: schema([]string{"task_id"}, map[string]any{"task_id": pTaskID}),
			call:        byID("POST", "/api/tasks/%d/archive", "task_id", false),
		},
		{
			Name:        "unarchive_task",
			Description: "Restore an archived task to the board.",
			InputSchema: schema([]string{"task_id"}, map[string]any{"task_id": pTaskID}),
			call:        byID("POST", "/api/tasks/%d/unarchive", "task_id", false),
		},
		{
			Name:        "list_archived_tasks",
			Description: "Archived tasks of one project, or of all projects if project_id is left out.",
			InputSchema: schema(nil, map[string]any{"project_id": pProjectID}),
			call:        list("/api/archive", "project_id"),
		},
		{
			Name:        "create_project",
			Description: "Add a project: a separate board on the shared lanes.",
			InputSchema: schema([]string{"name"}, map[string]any{"name": prop("string", "Project name"), "color": prop("string", "CSS colour, e.g. #3b82f6")}),
			call:        fixed("POST", "/api/projects", true),
		},
		{
			Name:        "update_project",
			Description: "Rename or recolour a project.",
			InputSchema: schema([]string{"project_id", "name"}, map[string]any{"project_id": pProjectID, "name": prop("string", "Project name"), "color": prop("string", "CSS colour")}),
			call:        byID("PATCH", "/api/projects/%d", "project_id", true),
		},
		{
			Name:        "delete_project",
			Description: "Delete a project; it must have no tasks.",
			InputSchema: schema([]string{"project_id"}, map[string]any{"project_id": pProjectID}),
			call:        byID("DELETE", "/api/projects/%d", "project_id", false),
		},
		{
			Name:        "create_lane",
			Description: "Add a lane at the right end of the board; lanes are shared by all projects.",
			InputSchema: schema([]string{"name"}, map[string]any{"name": prop("string", "Lane name"), "color": prop("string", "CSS colour, e.g. #3b82f6")}),
			call:        fixed("POST", "/api/lanes", true),
		},
		{
			Name:        "update_lane",
			Description: "Rename or recolour a lane.",
			InputSchema: schema([]string{"lane_id", "name"}, map[string]any{"lane_id": pLaneID, "name": prop("string", "Lane name"), "color": prop("string", "CSS colour")}),
			call:        byID("PATCH", "/api/lanes/%d", "lane_id", true),
		},
		{
			Name:        "delete_lane",
			Description: "Delete a lane; it must have no tasks on the board.",
			InputSchema: schema([]string{"lane_id"}, map[string]any{"lane_id": pLaneID}),
			call:        byID("DELETE", "/api/lanes/%d", "lane_id", false),
		},
		{
			Name:        "start_session",
			Description: "Start a Claude Code agent session on a task, running prompt in the task's working directory.",
			InputSchema: schema([]string{"task_id", "prompt"}, map[string]any{
				"task_id": pTaskID, "prompt": prop("string", "What the agent should do"), "title": prop("string", "Session title"),
				"include_context": prop("boolean", "Prepend the task's title, description and fields to the prompt"),
				"mode":            prop("string", "Permission mode, e.g. bypassPermissions or plan (default: the app's)"),
				"model":           prop("string", "Model, e.g. opus (default: the CLI's)"),
				"workdir":         pWorkdir,
				"done_lane_id":    prop("integer", "Move the task to this lane once the agent finishes successfully"),
			}),
			call: byID("POST", "/api/tasks/%d/sessions", "task_id", true),
		},
		{
			Name:        "list_sessions",
			Description: "Agent sessions, newest first, with status and latest message.",
			InputSchema: schema(nil, map[string]any{
				"task_id": prop("integer", "Only this task's sessions"),
				"status":  map[string]any{"type": "string", "enum": []string{"running", "idle", "error", "cancelled", "interrupted"}},
			}),
			call: list("/api/sessions", "task_id", "status"),
		},
		{
			Name:        "get_session",
			Description: "One agent session with its whole transcript.",
			InputSchema: schema([]string{"session_id"}, map[string]any{"session_id": prop("integer", "Session id")}),
			call:        byID("GET", "/api/sessions/%d", "session_id", false),
		},
		{
			Name:        "send_message",
			Description: "Continue an idle agent session with a new message.",
			InputSchema: schema([]string{"session_id", "text"}, map[string]any{"session_id": prop("integer", "Session id"), "text": prop("string", "Message")}),
			call:        byID("POST", "/api/sessions/%d/messages", "session_id", true),
		},
	}
}

// mcpNoStream answers GET (server stream) and DELETE (end session): neither is offered.
func mcpNoStream(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Allow", "POST")
	http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
}

func (s *Server) mcp(w http.ResponseWriter, r *http.Request) {
	var req rpcRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, 400, rpcResponse{JSONRPC: "2.0", ID: json.RawMessage("null"), Error: &rpcError{-32700, "parse error: " + err.Error()}})
		return
	}
	if req.ID == nil { // notifications and responses need no answer
		w.WriteHeader(http.StatusAccepted)
		return
	}
	res := rpcResponse{JSONRPC: "2.0", ID: req.ID}
	switch req.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		json.Unmarshal(req.Params, &p)
		version := mcpLatestVersion
		for _, v := range mcpVersions {
			if v == p.ProtocolVersion {
				version = v
			}
		}
		res.Result = map[string]any{
			"protocolVersion": version,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "assistant", "version": "1"},
			"instructions": "The user's personal kanban board. Each project has its own board of tasks on the shared lanes; " +
				"call get_board first for project, lane and task ids. " +
				"Agent sessions run Claude Code on a task. Archive rather than delete unless told otherwise.",
		}
	case "ping":
		res.Result = map[string]any{}
	case "tools/list":
		res.Result = map[string]any{"tools": s.mcpTools()}
	case "tools/call":
		var p struct {
			Name      string  `json:"name"`
			Arguments mcpArgs `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			res.Error = &rpcError{-32602, "invalid params: " + err.Error()}
			break
		}
		result, err := s.callTool(r, p.Name, p.Arguments)
		if err != nil {
			res.Error = &rpcError{-32602, err.Error()}
			break
		}
		res.Result = result
	default:
		res.Error = &rpcError{-32601, "method not found: " + req.Method}
	}
	writeJSON(w, 200, res)
}

// callTool runs a tool through the API. Unknown tools are a protocol error;
// everything the API rejects is a tool result with isError so the agent sees why.
func (s *Server) callTool(r *http.Request, name string, args mcpArgs) (map[string]any, error) {
	var tool *mcpTool
	for _, t := range s.mcpTools() {
		if t.Name == name {
			tool = &t
			break
		}
	}
	if tool == nil {
		return nil, fmt.Errorf("unknown tool: %s", name)
	}
	if args == nil {
		args = mcpArgs{}
	}
	text := func(msg string, isErr bool) map[string]any {
		return map[string]any{"content": []map[string]any{{"type": "text", "text": msg}}, "isError": isErr}
	}
	method, path, body, err := tool.call(args)
	if err != nil {
		return text(err.Error(), true), nil
	}
	var buf bytes.Buffer
	if body != nil {
		json.NewEncoder(&buf).Encode(body)
	}
	sub, err := http.NewRequestWithContext(r.Context(), method, path, &buf)
	if err != nil {
		return nil, err
	}
	sub.Header.Set("Content-Type", "application/json")
	rec := &recorder{header: http.Header{}, status: 200}
	s.mux.ServeHTTP(rec, sub)
	out := bytes.TrimSpace(rec.body.Bytes())
	if rec.status >= 400 {
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(out, &e) == nil && e.Error != "" {
			out = []byte(e.Error)
		}
		return text(fmt.Sprintf("%d: %s", rec.status, out), true), nil
	}
	if len(out) == 0 {
		return text("ok", false), nil
	}
	if tool.post != nil {
		if out, err = tool.post(out); err != nil {
			return nil, err
		}
	}
	return text(string(out), false), nil
}

// recorder captures a handler's response for callTool.
type recorder struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func (r *recorder) Header() http.Header         { return r.header }
func (r *recorder) Write(b []byte) (int, error) { return r.body.Write(b) }
func (r *recorder) WriteHeader(status int)      { r.status = status }

// boardSummary drops UI-only data (usage limits) from the board.
func boardSummary(raw []byte) ([]byte, error) {
	var b map[string]json.RawMessage
	if err := json.Unmarshal(raw, &b); err != nil {
		return nil, err
	}
	delete(b, "running_limits")
	return json.Marshal(b)
}

// taskWithoutTranscripts keeps get_task small; get_session has the transcript.
func taskWithoutTranscripts(raw []byte) ([]byte, error) {
	var t struct {
		Task      json.RawMessage              `json:"task"`
		Sessions  []map[string]json.RawMessage `json:"sessions"`
		Schedules json.RawMessage              `json:"schedules"`
	}
	if err := json.Unmarshal(raw, &t); err != nil {
		return nil, err
	}
	for _, x := range t.Sessions {
		delete(x, "messages")
		delete(x, "limits")
	}
	return json.Marshal(t)
}
