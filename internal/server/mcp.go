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
	pFields    = map[string]any{"type": "object", "additionalProperties": map[string]any{"type": "string"}, "description": "Custom fields by key, e.g. {\"pull_request\": \"https://…\"}; see get_board for the keys. An empty value removes a field. Empty productive, pull_request, slack and trello fields fill themselves from such a link in the description, and pull_request from a PR an agent opens (gh pr create)."}
	pDueAt     = prop("string", "Due time, RFC 3339 (e.g. 2026-10-21T09:00:00+02:00); a notification fires when it's reached")
	pKind      = prop("string", "Task kind: one of kinds in get_board or list_kinds (default task)")
	pWorkdir   = prop("string", "Absolute working directory for agent sessions on this computer")
	pReportTo  = prop("integer", "Session id told how each of this session's turns ends: its final reply, its error, the question it asked on its task's thread, "+
		"or that it was cancelled, as that session's next message (queued while it works). Use it to hear back from sessions you start")
	pProjectWorkdir   = prop("string", "Default working directory on this computer for agent sessions on the project's tasks; empty means the app default")
	pSkillAutomations = map[string]any{"type": "array", "description": "When the skill runs by itself: after an agent turn on a task finishes successfully, in that session, " +
		"if the task matches one of these. Left out keeps the saved list; [] means only by hand (run_skill).",
		"items": schema(nil, map[string]any{
			"kind":       prop("string", "Task kind (one of kinds in get_board or list_kinds); empty matches any"),
			"project_id": prop("integer", "Only tasks of this project; left out matches any"),
			"lane_id":    prop("integer", "Only tasks in this lane when the turn ends; left out matches any"),
			"enabled":    prop("boolean", "Off keeps the automation without running it"),
		})}
	pSkillBody = prop("string", "Instructions for the agent (Markdown), e.g. how a summary should look and where to write it. "+
		"A prompt template: {{.Task.Title}}, {{.Task.ID}}, {{.Fields.pull_request}}, {{.Context}}")
	pSkillModel = prop("string", "Model the skill's turn runs with, e.g. fable; empty keeps the session's model")
	pMemories   = map[string]any{"type": "array", "items": map[string]any{"type": "integer"}, "description": "Ids of memories (see list_memories) linked to the task: " +
		"its agents read them with the task context and update them after each turn. On create, left out links those of the tasks it builds on; [] links none. " +
		"Use search_memories to find the memories on the task's topic."}
	pMemoryID          = prop("integer", "Memory id (see list_memories)")
	pMemoryDescription = prop("string", "What the memory is about; it tells agents what belongs in it")
	pTrelloBoardID     = prop("integer", "Id of the board link (see list_trello_boards)")
	pTrelloBoard       = prop("string", "The board's URL (https://trello.com/b/…), short link or id")
	pTrelloProject     = prop("integer", "Project its cards become tasks of (default: the first project)")
	pTrelloLane        = prop("integer", "Lane new cards land in (default: the first lane)")
	pTrelloDoneLane    = prop("integer", "Tasks moved into this lane get their card archived (default: never)")
	pContextFrom       = map[string]any{"type": "array", "items": map[string]any{"type": "integer"}, "description": "Ids of earlier tasks this task builds on, e.g. the research it follows from. Summaries of their agent sessions (or the final replies, where a session has no up-to-date summary) go into the context of this task's agents (include_context, rules and schedules using {{.Context}}). Empty: the task starts from scratch."}
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
			Description: "The kanban board of one project (the first if project_id is left out): every project, the lanes the project shows (in order; all_lanes has every lane, hidden ones included), the project's tasks on the board, custom field definitions, the task kinds, how many agent sessions run per task and, per task id, agent_status: what its agents are up to (" + agentStatusDoc + "). Start here to find project, lane and task ids.",
			InputSchema: schema(nil, map[string]any{"project_id": pProjectID}),
			call:        list("/api/board", "project_id"),
			post:        boardSummary,
		},
		{
			Name: "get_task",
			Description: "One task with its schedules and agent sessions (without transcripts; use get_session for those), the tasks it builds on (builds_on), the tasks that build on it (used_by), its memories, " +
				"its activity thread (activity: short messages from its sessions, session_id, and the user, session_id null, oldest first; a question is open while answered_at is null), " +
				"agent_status (null if none; " + agentStatusDoc + ") " +
				"and, in task, slack: its Slack channel here (channel_id, channel_name, session_id the orchestrator, url, last_sync_at, error), null if none.",
			InputSchema: schema([]string{"task_id"}, map[string]any{"task_id": pTaskID}),
			call:        byID("GET", "/api/tasks/%d", "task_id", false),
			post:        taskWithoutTranscripts,
		},
		{
			Name:        "create_task",
			Description: "Add a task to a project's board (the first project if project_id is left out), at the end of a lane (the first lane if lane_id is left out).",
			InputSchema: schema([]string{"title"}, map[string]any{
				"title": prop("string", "Short title"), "description": prop("string", "Markdown description"), "project_id": pProjectID,
				"kind": pKind, "lane_id": pLaneID, "workdir": pWorkdir, "due_at": pDueAt, "fields": pFields, "context_from": pContextFrom,
				"memories": pMemories,
				"watched":  prop("boolean", "Watch the task: the project picker counts watched tasks; moving it to Done stops watching"),
			}),
			call: fixed("POST", "/api/tasks", true),
		},
		{
			Name:        "update_task",
			Description: "Change a task's title, description, kind, workdir, due time, custom fields, the tasks it builds on (context_from replaces the list), its memories (memories replaces the list), whether the user watches it, or project (it goes to the end of its lane there). Only the given keys change; due_at null clears it. Use move_task to change its lane.",
			InputSchema: schema([]string{"task_id"}, map[string]any{
				"task_id": pTaskID, "project_id": pProjectID, "title": prop("string", "New title"), "description": prop("string", "New description (replaces the old one)"),
				"kind": pKind, "workdir": pWorkdir, "due_at": map[string]any{"type": []string{"string", "null"}, "description": pDueAt["description"]}, "fields": pFields,
				"context_from": pContextFrom, "memories": pMemories,
				"watched": prop("boolean", "Watch the task: the project picker counts watched tasks; moving it to Done stops watching"),
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
			Name:        "search_tasks",
			Description: "Find tasks of every project, archived ones included, whose title or description holds each word of query, or whose id it is (\"12\" or \"#12\"). Tasks on a board first, then the most recently updated. An empty query lists the most recently updated tasks.",
			InputSchema: schema(nil, map[string]any{"q": prop("string", "Words to look for, or a task id"), "limit": prop("integer", "At most this many tasks (default 20, max 100)")}),
			call:        list("/api/tasks", "q", "limit"),
		},
		{
			Name:        "list_watched_tasks",
			Description: "Watched tasks on the board, of every project: by project, then lane, each with its agent_status if it has one. Done ends watching a task.",
			InputSchema: schema(nil, map[string]any{}),
			call:        list("/api/watched"),
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
			InputSchema: schema([]string{"name"}, map[string]any{"name": prop("string", "Project name"), "color": prop("string", "CSS colour, e.g. #3b82f6"),
				"workdir": pProjectWorkdir}),
			call: fixed("POST", "/api/projects", true),
		},
		{
			Name:        "update_project",
			Description: "Rename or recolour a project, or change its default workdir or hidden lanes (left out keeps them).",
			InputSchema: schema([]string{"project_id", "name"}, map[string]any{"project_id": pProjectID, "name": prop("string", "Project name"), "color": prop("string", "CSS colour"),
				"workdir": pProjectWorkdir, "hidden_lanes": map[string]any{"type": "array", "items": map[string]any{"type": "integer"},
					"description": "Ids of the lanes the project's board leaves out (replaces the list; empty shows every lane). A hidden lane still shows while tasks of the project are in it, and rules can still move tasks into it. At least one lane must stay shown."}}),
			call: byID("PATCH", "/api/projects/%d", "project_id", true),
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
			Name: "sync_status",
			Description: "How this computer takes part in syncing the assistant's database between computers: role is off (not synced), hub (the shared database the others sync with; it runs no schedules or reminders) " +
				"or client (syncs with the hub). A client also gives server, online (the hub answered the latest request), pending (changed rows not pushed yet), synced_at (last exchange) and error (why the latest request failed).",
			InputSchema: schema(nil, map[string]any{}),
			call:        fixed("GET", "/api/sync/status", false),
		},
		{
			Name: "list_trello_boards",
			Description: "The Trello boards kept in sync: each board's open cards become tasks (title, description, due date, a trello link field) in its project and lane, " +
				"a due date changed on Trello updates its task, and a task moved into the board's done lane gets its card archived (moved out, the card comes back). " +
				"With each board how its last sync went; configured is false while the Trello key and token are missing from the config.",
			InputSchema: schema(nil, map[string]any{}),
			call:        fixed("GET", "/api/trello", false),
		},
		{
			Name:        "add_trello_board",
			Description: "Start syncing a Trello board; its open cards are imported within a minute. A board can be synced only once.",
			InputSchema: schema([]string{"board"}, map[string]any{"board": pTrelloBoard, "project_id": pTrelloProject, "lane_id": pTrelloLane, "done_lane_id": pTrelloDoneLane}),
			call:        fixed("POST", "/api/trello/boards", true),
		},
		{
			Name:        "update_trello_board",
			Description: "Change a synced board, or where its cards go; the whole link is replaced, so pass every setting to keep. Cards already imported stay where they are.",
			InputSchema: schema([]string{"trello_board_id", "board"}, map[string]any{"trello_board_id": pTrelloBoardID, "board": pTrelloBoard,
				"project_id": pTrelloProject, "lane_id": pTrelloLane, "done_lane_id": pTrelloDoneLane}),
			call: byID("PUT", "/api/trello/boards/%d", "trello_board_id", true),
		},
		{
			Name:        "remove_trello_board",
			Description: "Stop syncing a Trello board. Its tasks stay; added again later, its cards are not imported twice.",
			InputSchema: schema([]string{"trello_board_id"}, map[string]any{"trello_board_id": pTrelloBoardID}),
			call:        byID("DELETE", "/api/trello/boards/%d", "trello_board_id", false),
		},
		{
			Name: "start_session",
			Description: "Start an agent session (Claude Code or Gemini CLI) on a task, running prompt in the task's working directory. " +
				"With skill_id the session's first turn applies that skill to the task instead (see list_skills), given the task's context; " +
				"prompt, optional then, goes with it as more context. That turn ends like any other: the skill's artifact is kept and done_lane_id applies.",
			InputSchema: schema([]string{"task_id"}, map[string]any{
				"task_id": pTaskID, "prompt": prop("string", "What the agent should do; required unless skill_id is given"), "title": prop("string", "Session title (default: the prompt's first line, or the skill's name)"),
				"skill_id":        prop("integer", "Start the session by applying this skill to the task"),
				"include_context": prop("boolean", "Prepend the task's title, description and fields, its memories, and the results of the tasks it builds on, to the prompt (a skill gets them anyway)"),
				"mode":            prop("string", "Permission mode, e.g. bypassPermissions or plan (default: the app's)"),
				"agent":           prop("string", "CLI the session runs on: claude or gemini (default: the app's)"),
				"model":           prop("string", "Model, e.g. opus for claude or flash for gemini (default: the CLI's)"),
				"workdir":         pWorkdir,
				"done_lane_id":    prop("integer", "Move the task to this lane once the agent finishes successfully"),
				"report_to":       pReportTo,
			}),
			call: byID("POST", "/api/tasks/%d/sessions", "task_id", true),
		},
		{
			Name: "list_skills",
			Description: "The user's skills: instructions an agent applies to the work of its session, by hand (run_skill) or automatically " +
				"after a successful agent turn on a matching task. A skill with artifact on keeps the agent's reply on the task (see get_task).",
			InputSchema: schema(nil, map[string]any{}),
			call:        fixed("GET", "/api/skills", false),
		},
		{
			Name:        "create_skill",
			Description: "Add a skill.",
			InputSchema: schema([]string{"name", "body"}, map[string]any{
				"name": prop("string", "Unique name"), "description": prop("string", "What it is for"), "body": pSkillBody,
				"artifact":    prop("boolean", "Keep the agent's reply as an artifact on the task"),
				"model":       pSkillModel,
				"automations": pSkillAutomations,
			}),
			call: fixed("POST", "/api/skills", true),
		},
		{
			Name:        "update_skill",
			Description: "Change a skill; only the given keys change (automations, if given, replace the list).",
			InputSchema: schema([]string{"skill_id"}, map[string]any{
				"skill_id": prop("integer", "Skill id"), "name": prop("string", "Unique name"), "description": prop("string", "What it is for"),
				"body": pSkillBody, "artifact": prop("boolean", "Keep the agent's reply as an artifact on the task"), "model": pSkillModel,
				"automations": pSkillAutomations,
			}),
			call: byID("PATCH", "/api/skills/%d", "skill_id", true),
		},
		{
			Name: "list_kinds",
			Description: "The task kinds, in order: each with built_in (task and reminder can't be deleted) and how many tasks (archived ones included), " +
				"transition rules and skill automations have it. Rules and skill automations can be for one kind.",
			InputSchema: schema(nil, map[string]any{}),
			call:        fixed("GET", "/api/kinds", false),
		},
		{
			Name:        "add_kind",
			Description: "Add a task kind at the end of the list. Returns the list.",
			InputSchema: schema([]string{"name"}, map[string]any{"name": prop("string", "Lowercase letters, digits, _ or -, starting with a letter; at most 31")}),
			call:        fixed("POST", "/api/kinds", true),
		},
		{
			Name: "delete_kind",
			Description: "Delete a task kind; its tasks become kind task. Refused for task and reminder, and while transition rules or skill automations have it. " +
				"Returns the list.",
			InputSchema: schema([]string{"name"}, map[string]any{"name": prop("string", "The kind")}),
			call: func(a mcpArgs) (string, string, any, error) {
				var name string
				if err := json.Unmarshal(a["name"], &name); err != nil || name == "" {
					return "", "", nil, fmt.Errorf("name must be a non-empty string")
				}
				return "DELETE", "/api/kinds/" + url.PathEscape(name), nil, nil
			},
		},
		{
			Name:        "delete_skill",
			Description: "Delete a skill and its automations; artifacts it made stay.",
			InputSchema: schema([]string{"skill_id"}, map[string]any{"skill_id": prop("integer", "Skill id")}),
			call:        byID("DELETE", "/api/skills/%d", "skill_id", false),
		},
		{
			Name:        "run_skill",
			Description: "Apply a skill to an idle agent session now, as its next turn.",
			InputSchema: schema([]string{"session_id", "skill_id"}, map[string]any{"session_id": prop("integer", "Session id"), "skill_id": prop("integer", "Skill id")}),
			call:        byID("POST", "/api/sessions/%d/skill", "session_id", true),
		},
		{
			Name:        "list_sessions",
			Description: "Agent sessions, newest first, with status and latest message; needs_input, if set, says why the agent waits for the user. Summaries are left out (get_session has them); summary_at says a session has one and summary_stale that newer messages came after it.",
			InputSchema: schema(nil, map[string]any{
				"task_id": prop("integer", "Only this task's sessions"),
				"status":  map[string]any{"type": "string", "enum": []string{"running", "idle", "error", "cancelled", "interrupted"}},
				"q":       prop("string", "Only sessions whose task's title or description holds every word of this (case-insensitive)"),
			}),
			call: list("/api/sessions", "task_id", "status", "q"),
			post: sessionsWithoutSummaries,
		},
		{
			Name: "get_session",
			Description: "One agent session with its whole transcript and its summary (summary: empty if none; summary_stale: messages came after it; " +
				"summarizing: one is being written) and how full its context is (context_tokens of context_window; 0: not known yet). " +
				"Tasks that build on the session's task get the summary instead of the transcript.",
			InputSchema: schema([]string{"session_id"}, map[string]any{"session_id": prop("integer", "Session id")}),
			call:        byID("GET", "/api/sessions/%d", "session_id", false),
		},
		{
			Name: "summarize_session",
			Description: "Write or rewrite a session's summary of what was done, found and decided, for the agents of tasks that build on its task. " +
				"It is written in the background by a fork of the session's agent (the session itself is not continued); see get_session for the result.",
			InputSchema: schema([]string{"session_id"}, map[string]any{"session_id": prop("integer", "Session id")}),
			call:        byID("POST", "/api/sessions/%d/summary", "session_id", false),
		},
		{
			Name: "list_memories",
			Description: "The user's memories: Markdown documents of lasting knowledge kept across tasks. Agents of the tasks linked to a memory read it, " +
				"and after each of their turns a fork of the agent rewrites it with what the session found out. A memory is a topic, not tied to a project. " +
				"The most recently used first (last_used: its latest revision or session of a linked task). Bodies are left out; get_memory has them.",
			InputSchema: schema(nil, map[string]any{}),
			call:        fixed("GET", "/api/memories", false),
		},
		{
			Name: "search_memories",
			Description: "Find the memories on a topic: those whose name, description or body has any of the query's words (inflected forms too), " +
				"best first (score), then the most recently used. Pass e.g. a task's title and description to find the memories its agents should read; " +
				"link them with update_task (memories).",
			InputSchema: schema([]string{"q"}, map[string]any{"q": prop("string", "Words to look for")}),
			call:        list("/api/memories", "q"),
		},
		{
			Name:        "get_memory",
			Description: "One memory with its body, its revisions (newest first, without bodies) and the tasks linked to it.",
			InputSchema: schema([]string{"memory_id"}, map[string]any{"memory_id": pMemoryID}),
			call:        byID("GET", "/api/memories/%d", "memory_id", false),
		},
		{
			Name:        "create_memory",
			Description: "Add a memory on a topic. Link it to tasks with task_id here, or with create_task or update_task (memories).",
			InputSchema: schema([]string{"name"}, map[string]any{
				"name": prop("string", "Unique name, e.g. the topic"), "description": pMemoryDescription,
				"body":    prop("string", "Initial Markdown body"),
				"task_id": prop("integer", "Link the new memory to this task"),
				"fill":    prop("boolean", "With task_id: write the memory from the task's agent sessions so far, in the background"),
			}),
			call: fixed("POST", "/api/memories", true),
		},
		{
			Name: "update_memory",
			Description: "Change a memory; only the given keys change. body replaces the whole document as a new revision: read it with get_memory first " +
				"and pass its revision as base_revision, so a change made meanwhile is not overwritten. To add a single fact, use remember.",
			InputSchema: schema([]string{"memory_id"}, map[string]any{
				"memory_id": pMemoryID, "name": prop("string", "Unique name"), "description": pMemoryDescription,
				"body": prop("string", "The whole new Markdown body"), "base_revision": prop("integer", "The revision body is based on (see get_memory)"),
			}),
			call: byID("PATCH", "/api/memories/%d", "memory_id", true),
		},
		{
			Name:        "remember",
			Description: "Add a note to the end of a memory, e.g. a fact or decision the user wants kept. The next update from an agent session works it into the document.",
			InputSchema: schema([]string{"memory_id", "text"}, map[string]any{"memory_id": pMemoryID, "text": prop("string", "What to remember, self-contained")}),
			call:        byID("POST", "/api/memories/%d/notes", "memory_id", true),
		},
		{
			Name:        "delete_memory",
			Description: "Delete a memory with its revisions; tasks linked to it lose it.",
			InputSchema: schema([]string{"memory_id"}, map[string]any{"memory_id": pMemoryID}),
			call:        byID("DELETE", "/api/memories/%d", "memory_id", false),
		},
		{
			Name: "update_memories_from_session",
			Description: "Update the memories of a session's task with what the session found out, now. It also happens by itself after each successful turn. " +
				"Runs in the background in a fork of the session's agent; get_memory shows the result.",
			InputSchema: schema([]string{"session_id"}, map[string]any{"session_id": prop("integer", "Session id")}),
			call:        byID("POST", "/api/sessions/%d/memory", "session_id", false),
		},
		{
			Name:        "dismiss_needs_input",
			Description: "Dismiss a session: clear its needs_input and mark how its last turn ended as seen, so its task's agent_status (finished, done, failed, cancelled) goes away; sending a message clears needs_input too.",
			InputSchema: schema([]string{"session_id"}, map[string]any{"session_id": prop("integer", "Session id")}),
			call:        byID("DELETE", "/api/sessions/%d/needs-input", "session_id", false),
		},
		{
			Name:        "send_message",
			Description: "Continue an agent session with a new message. While its agent works, the message is queued (the result says queued: true) and starts the next turn once the current one ends.",
			InputSchema: schema([]string{"session_id", "text"}, map[string]any{"session_id": prop("integer", "Session id"), "text": prop("string", "Message")}),
			call:        byID("POST", "/api/sessions/%d/messages", "session_id", true),
		},
		{
			Name: "post_task_message",
			Description: "Post a short message (1-3 sentences, like on Slack) on a task's activity thread, the user's overview of what its agent sessions do: " +
				"what you did, decided or handed over to another session. The details stay in your session, which the message links to. " +
				"Pass your own session_id (your system prompt names it). With question: true it asks the user for a decision: they get a notification, " +
				"your session shows it needs them, and their answer arrives as your next message, so end your turn after asking. " +
				"On a task connected to a Slack channel the message is posted there too: by default in the thread of the latest Slack message you were given, " +
				"else at the top of the channel; reply_to or top_level choose otherwise. The result's slack says where it went, or its error.",
			InputSchema: schema([]string{"task_id", "session_id", "body"}, map[string]any{
				"task_id": pTaskID, "session_id": prop("integer", "Your session's id"),
				"body":      prop("string", "The message, Markdown, 1-3 sentences (longer for an answer posted to Slack)"),
				"question":  prop("boolean", "Ask the user for a decision and wait for the answer"),
				"reply_to":  prop("integer", "Slack: post in the thread of this task message (the task_message id of a Slack message you were given)"),
				"top_level": prop("boolean", "Slack: post at the top of the channel, not in a thread; only when asked to"),
			}),
			call: func(a mcpArgs) (string, string, any, error) {
				if _, err := a.id("session_id"); err != nil {
					return "", "", nil, err
				}
				return byID("POST", "/api/tasks/%d/messages", "task_id", true)(a)
			},
		},
		{
			Name: "connect_slack_channel",
			Description: "Connect a task to a Slack channel on this computer: from now on the channel's messages, top-level and in threads, come to the task's activity thread " +
				"and go to its orchestrator session, and what is posted on the thread goes to the channel (see post_task_message). Earlier messages are not taken in. " +
				"The bot must be in the channel. The orchestrator is session_id, else the task's latest session on this computer; a task without one gets a new orchestrator session. " +
				"Connecting a task already connected replaces its channel. The result is the task, its slack the connection.",
			InputSchema: schema([]string{"task_id", "channel"}, map[string]any{
				"task_id":    pTaskID,
				"channel":    prop("string", "The channel's id (C…) or link (https://….slack.com/archives/C…)"),
				"session_id": prop("integer", "The orchestrator session, one of the task's on this computer (default: its latest; none: a new one is started)"),
			}),
			call: byID("POST", "/api/tasks/%d/slack", "task_id", true),
		},
		{
			Name:        "disconnect_slack_channel",
			Description: "Disconnect a task from its Slack channel: the channel is no longer read nor posted to. The task's sessions and its activity thread stay.",
			InputSchema: schema([]string{"task_id"}, map[string]any{"task_id": pTaskID}),
			call:        byID("DELETE", "/api/tasks/%d/slack", "task_id", false),
		},
		{
			Name:        "delete_queued_message",
			Description: "Drop a message queued for an agent session's next turn before it is sent (ids are in the session's queued list).",
			InputSchema: schema([]string{"queued_message_id"}, map[string]any{"queued_message_id": prop("integer", "Queued message id")}),
			call:        byID("DELETE", "/api/queued-messages/%d", "queued_message_id", false),
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
				"Memories keep lasting knowledge across tasks; use remember to add to one. " +
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

// agentStatusDoc explains a task's agent_status to agents using the tools.
const agentStatusDoc = `{status, session_id, reason}; status is running, needs_you (reason says why), finished (the last turn ended; its result wants a look), done (it ended well and the task reached the session's done lane), failed (reason has the error) or cancelled. Outcomes show until the user dismisses the session, moves the task or a new turn starts`

// boardSummary drops UI-only data (usage limits) from the board.
func boardSummary(raw []byte) ([]byte, error) {
	var b map[string]json.RawMessage
	if err := json.Unmarshal(raw, &b); err != nil {
		return nil, err
	}
	delete(b, "running_limits")
	return json.Marshal(b)
}

// sessionsWithoutSummaries keeps list_sessions small; get_session has the summary.
func sessionsWithoutSummaries(raw []byte) ([]byte, error) {
	var xs []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &xs); err != nil {
		return nil, err
	}
	for _, x := range xs {
		delete(x, "summary")
	}
	return json.Marshal(xs)
}

// taskWithoutTranscripts keeps get_task small; get_session has the transcript.
func taskWithoutTranscripts(raw []byte) ([]byte, error) {
	var t struct {
		Task      json.RawMessage              `json:"task"`
		Sessions  []map[string]json.RawMessage `json:"sessions"`
		Schedules json.RawMessage              `json:"schedules"`
		BuildsOn  json.RawMessage              `json:"builds_on"`
		UsedBy    json.RawMessage              `json:"used_by"`
		Artifacts json.RawMessage              `json:"artifacts"`
		Activity  json.RawMessage              `json:"activity"`
		Memories  json.RawMessage              `json:"memories"`
		Status    json.RawMessage              `json:"agent_status"`
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
