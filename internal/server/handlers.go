package server

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/jeanpetric/assistant/internal/agent"
	"github.com/jeanpetric/assistant/internal/app"
	"github.com/jeanpetric/assistant/internal/store"
	"github.com/jeanpetric/assistant/internal/trello"
)

// ---- board & meta ----

var taskKinds = []string{"task", "coding", "research", "discussion", "reminder"}

// board returns the lanes and the tasks of one project (?project_id=, else the
// first project), plus every project to switch to.
func (s *Server) board(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	lanes, err := s.app.Store.Lanes(ctx)
	if err != nil {
		writeErr(w, err)
		return
	}
	projects, err := s.app.Store.Projects(ctx)
	if err != nil {
		writeErr(w, err)
		return
	}
	projectID, err := queryID(r, "project_id")
	if err != nil {
		writeErr(w, err)
		return
	}
	// An unknown project (e.g. one deleted in another tab) shows the first one.
	known := false
	for _, p := range projects {
		known = known || p.ID == projectID
	}
	if !known && len(projects) > 0 {
		projectID = projects[0].ID
	}
	tasks, err := s.app.Store.Tasks(ctx, projectID)
	if err != nil {
		writeErr(w, err)
		return
	}
	fields, err := s.app.Store.FieldDefs(ctx)
	if err != nil {
		writeErr(w, err)
		return
	}
	running, err := s.app.Store.Sessions(ctx, 0, store.StatusRunning)
	if err != nil {
		writeErr(w, err)
		return
	}
	needsInput, err := s.app.Store.NeedsInputByTask(ctx)
	if err != nil {
		writeErr(w, err)
		return
	}
	statuses, err := s.app.Store.AgentStatuses(ctx)
	if err != nil {
		writeErr(w, err)
		return
	}
	agentStatus := map[int64]store.AgentStatus{}
	for _, t := range tasks {
		if st, ok := statuses[t.ID]; ok {
			agentStatus[t.ID] = st
		}
	}
	// Per task: how many agents run, and the usage limits last reported by the
	// most recently active of them and how full its context is.
	type contextOut struct {
		Tokens int `json:"tokens"`
		Window int `json:"window"`
	}
	runningByTask := map[int64]int{}
	limitsByTask := map[int64]agent.RateLimits{}
	contextByTask := map[int64]contextOut{}
	for _, x := range running {
		if x.TaskID == 0 {
			continue
		}
		runningByTask[x.TaskID]++
		if _, ok := contextByTask[x.TaskID]; !ok && x.ContextTokens > 0 {
			contextByTask[x.TaskID] = contextOut{x.ContextTokens, x.ContextWindow}
		}
		if _, ok := limitsByTask[x.TaskID]; ok {
			continue
		}
		if l := s.app.Limits(x.ID); l.FiveHour != nil || l.SevenDay != nil {
			limitsByTask[x.TaskID] = l
		}
	}
	writeJSON(w, 200, map[string]any{"projects": projects, "project_id": projectID, "lanes": lanes, "tasks": tasks, "fields": fields,
		"running": runningByTask, "running_limits": limitsByTask, "running_context": contextByTask, "needs_input": needsInput,
		"agent_status": agentStatus})
}

// agentMeta lists the agents a session can run on with their model suggestions.
func agentMeta() []map[string]any {
	out := []map[string]any{}
	for _, a := range agent.Agents {
		out = append(out, map[string]any{"name": a, "models": agent.ModelsFor(a)})
	}
	return out
}

func (s *Server) meta(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{
		"modes":         agent.Modes,
		"models":        agent.Models,
		"agents":        agentMeta(),
		"default_mode":  s.app.DefaultMode,
		"default_agent": cmp.Or(s.app.DefaultAgent, agent.Claude),
		"workdir":       s.app.DefaultWorkdir,
		"kinds":         taskKinds,
	})
}

// ---- settings ----

func (s *Server) getSettings(w http.ResponseWriter, r *http.Request) {
	x, err := s.app.Store.Settings(r.Context())
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, x)
}

func (s *Server) saveSettings(w http.ResponseWriter, r *http.Request) {
	var in struct {
		store.Settings
		SessionPrompt *string         `json:"session_prompt"`
		SessionLaneID json.RawMessage `json:"session_lane_id"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, err)
		return
	}
	x := in.Settings
	cur, err := s.app.Store.Settings(r.Context())
	if err != nil {
		writeErr(w, err)
		return
	}
	// Leaving session_prompt out keeps the saved one.
	x.SessionPrompt = cur.SessionPrompt
	if in.SessionPrompt != nil {
		x.SessionPrompt = *in.SessionPrompt
		if _, err := app.RenderSessionPrompt(x.SessionPrompt, app.SessionPromptData{}); err != nil {
			writeErr(w, badRequest{"session_prompt: " + err.Error()})
			return
		}
	}
	if x.ArchiveAfterMinutes < 0 {
		writeErr(w, badRequest{"archive_after_minutes must not be negative"})
		return
	}
	if err := s.checkLane(r.Context(), x.ArchiveLaneID, "archive_lane_id"); err != nil {
		writeErr(w, err)
		return
	}
	// Leaving session_lane_id out keeps the saved lane; null means the first lane.
	x.SessionLaneID = cur.SessionLaneID
	if in.SessionLaneID != nil {
		x.SessionLaneID = nil
		if err := json.Unmarshal(in.SessionLaneID, &x.SessionLaneID); err != nil {
			writeErr(w, badRequest{"session_lane_id: " + err.Error()})
			return
		}
		if err := s.checkLane(r.Context(), x.SessionLaneID, "session_lane_id"); err != nil {
			writeErr(w, err)
			return
		}
	}
	// Leaving workdirs out keeps the saved list.
	if x.Workdirs == nil {
		x.Workdirs = cur.Workdirs
	} else {
		dirs, err := resolveWorkdirs(x.Workdirs)
		if err != nil {
			writeErr(w, err)
			return
		}
		x.Workdirs = dirs
	}
	if err := s.app.Store.SaveSettings(r.Context(), x); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, x)
}

// ---- trello ----

type trelloBoardView struct {
	store.TrelloBoard
	app.TrelloStatus
}

type trelloView struct {
	Configured bool              `json:"configured"` // key and token are in the config
	Boards     []trelloBoardView `json:"boards"`
}

func (s *Server) getTrello(w http.ResponseWriter, r *http.Request) {
	boards, err := s.app.Store.TrelloBoards(r.Context())
	if err != nil {
		writeErr(w, err)
		return
	}
	v := trelloView{Configured: s.app.Trello != nil, Boards: []trelloBoardView{}}
	for _, b := range boards {
		v.Boards = append(v.Boards, trelloBoardView{b, s.app.TrelloStatus(b.ID)})
	}
	writeJSON(w, 200, v)
}

// checkTrelloBoard validates a board link; id is the link's own id when
// updating, 0 when adding.
func (s *Server) checkTrelloBoard(ctx context.Context, b *store.TrelloBoard, id int64) error {
	b.Board = strings.TrimSpace(b.Board)
	if b.Board == "" {
		return badRequest{"board is required"}
	}
	boards, err := s.app.Store.TrelloBoards(ctx)
	if err != nil {
		return err
	}
	for _, o := range boards {
		if o.ID != id && trello.BoardID(o.Board) == trello.BoardID(b.Board) {
			return badRequest{"board is already synced"}
		}
	}
	if b.ProjectID != nil {
		if _, err := s.app.Store.Project(ctx, *b.ProjectID); errors.Is(err, store.ErrNotFound) {
			return badRequest{"unknown project_id"}
		} else if err != nil {
			return err
		}
	}
	if err := s.checkLane(ctx, b.LaneID, "lane_id"); err != nil {
		return err
	}
	return s.checkLane(ctx, b.DoneLaneID, "done_lane_id")
}

func (s *Server) addTrelloBoard(w http.ResponseWriter, r *http.Request) {
	var b store.TrelloBoard
	if err := decode(r, &b); err != nil {
		writeErr(w, err)
		return
	}
	if err := s.checkTrelloBoard(r.Context(), &b, 0); err != nil {
		writeErr(w, err)
		return
	}
	b, err := s.app.Store.AddTrelloBoard(r.Context(), b)
	if err != nil {
		writeErr(w, err)
		return
	}
	s.app.KickTrello()
	writeJSON(w, 201, trelloBoardView{TrelloBoard: b})
}

func (s *Server) updateTrelloBoard(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, err)
		return
	}
	var b store.TrelloBoard
	if err := decode(r, &b); err != nil {
		writeErr(w, err)
		return
	}
	if _, err := s.app.Store.TrelloBoard(r.Context(), id); err != nil {
		writeErr(w, err)
		return
	}
	if err := s.checkTrelloBoard(r.Context(), &b, id); err != nil {
		writeErr(w, err)
		return
	}
	b.ID = id
	if err := s.app.Store.UpdateTrelloBoard(r.Context(), b); err != nil {
		writeErr(w, err)
		return
	}
	s.app.KickTrello()
	writeJSON(w, 200, trelloBoardView{b, s.app.TrelloStatus(id)})
}

func (s *Server) deleteTrelloBoard(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, err)
		return
	}
	if err := s.app.Store.DeleteTrelloBoard(r.Context(), id); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(204)
}

// syncTrello syncs every board now and reports how it went; a failed sync is
// in the boards' status, not an error of the request.
func (s *Server) syncTrello(w http.ResponseWriter, r *http.Request) {
	s.app.SyncTrello(r.Context())
	s.getTrello(w, r)
}

// ---- lanes ----

type laneInput struct {
	Name  string `json:"name"`
	Color string `json:"color"`
}

func (in laneInput) validate() error {
	if strings.TrimSpace(in.Name) == "" {
		return badRequest{"name is required"}
	}
	return nil
}

func (s *Server) createLane(w http.ResponseWriter, r *http.Request) {
	var in laneInput
	if err := decode(r, &in); err != nil {
		writeErr(w, err)
		return
	}
	if err := in.validate(); err != nil {
		writeErr(w, err)
		return
	}
	l, err := s.app.Store.CreateLane(r.Context(), strings.TrimSpace(in.Name), in.Color)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 201, l)
}

func (s *Server) updateLane(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, err)
		return
	}
	var in laneInput
	if err := decode(r, &in); err != nil {
		writeErr(w, err)
		return
	}
	if err := in.validate(); err != nil {
		writeErr(w, err)
		return
	}
	if err := s.app.Store.UpdateLane(r.Context(), id, strings.TrimSpace(in.Name), in.Color); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(204)
}

func (s *Server) deleteLane(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, err)
		return
	}
	if err := s.app.Store.DeleteLane(r.Context(), id); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(204)
}

func (s *Server) reorderLanes(w http.ResponseWriter, r *http.Request) {
	var in struct {
		IDs []int64 `json:"ids"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, err)
		return
	}
	if err := s.app.Store.ReorderLanes(r.Context(), in.IDs); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(204)
}

// ---- projects ----

type projectInput struct {
	Name    string  `json:"name"`
	Color   string  `json:"color"`
	Workdir *string `json:"workdir"` // nil keeps the saved one
}

func (in *projectInput) validate() error {
	in.Name = strings.TrimSpace(in.Name)
	if in.Name == "" {
		return badRequest{"name is required"}
	}
	if in.Workdir == nil {
		return nil
	}
	dir, err := resolveWorkdir(*in.Workdir)
	in.Workdir = &dir
	return err
}

func (s *Server) createProject(w http.ResponseWriter, r *http.Request) {
	var in projectInput
	if err := decode(r, &in); err != nil {
		writeErr(w, err)
		return
	}
	if err := in.validate(); err != nil {
		writeErr(w, err)
		return
	}
	p := store.Project{Name: in.Name, Color: in.Color}
	if in.Workdir != nil {
		p.Workdir = *in.Workdir
	}
	p, err := s.app.Store.CreateProject(r.Context(), p)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 201, p)
}

func (s *Server) updateProject(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, err)
		return
	}
	var in projectInput
	if err := decode(r, &in); err != nil {
		writeErr(w, err)
		return
	}
	if err := in.validate(); err != nil {
		writeErr(w, err)
		return
	}
	p, err := s.app.Store.Project(r.Context(), id)
	if err != nil {
		writeErr(w, err)
		return
	}
	p.Name, p.Color = in.Name, in.Color
	if in.Workdir != nil {
		p.Workdir = *in.Workdir
	}
	if err := s.app.Store.UpdateProject(r.Context(), p); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(204)
}

func (s *Server) deleteProject(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, err)
		return
	}
	if err := s.app.Store.DeleteProject(r.Context(), id); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(204)
}

func (s *Server) reorderProjects(w http.ResponseWriter, r *http.Request) {
	var in struct {
		IDs []int64 `json:"ids"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, err)
		return
	}
	if err := s.app.Store.ReorderProjects(r.Context(), in.IDs); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(204)
}

// checkProject rejects a project id that doesn't exist; 0 is fine.
func (s *Server) checkProject(ctx context.Context, id int64) error {
	if id == 0 {
		return nil
	}
	_, err := s.app.Store.Project(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return badRequest{"unknown project_id"}
	}
	return err
}

// ---- field definitions ----

var fieldKeyRe = regexp.MustCompile(`^[a-z][a-z0-9_]{0,40}$`)

func (s *Server) saveField(w http.ResponseWriter, r *http.Request) {
	var f store.FieldDef
	if err := decode(r, &f); err != nil {
		writeErr(w, err)
		return
	}
	if !fieldKeyRe.MatchString(f.Key) {
		writeErr(w, badRequest{"key must be lowercase letters, digits or underscores"})
		return
	}
	if f.Type != "text" && f.Type != "url" {
		writeErr(w, badRequest{"type must be text or url"})
		return
	}
	if strings.TrimSpace(f.Label) == "" {
		f.Label = f.Key
	}
	if err := s.app.Store.UpsertFieldDef(r.Context(), f); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(204)
}

func (s *Server) deleteField(w http.ResponseWriter, r *http.Request) {
	if err := s.app.Store.DeleteFieldDef(r.Context(), r.PathValue("key")); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(204)
}

// ---- tasks ----

func (s *Server) createTask(w http.ResponseWriter, r *http.Request) {
	var t store.Task
	if err := decode(r, &t); err != nil {
		writeErr(w, err)
		return
	}
	if strings.TrimSpace(t.Title) == "" {
		writeErr(w, badRequest{"title is required"})
		return
	}
	if err := s.checkProject(r.Context(), t.ProjectID); err != nil {
		writeErr(w, err)
		return
	}
	t, err := s.app.Store.CreateTask(r.Context(), t)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 201, t)
}

func (s *Server) getTask(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, err := pathID(r)
	if err != nil {
		writeErr(w, err)
		return
	}
	t, err := s.app.Store.Task(ctx, id)
	if err != nil {
		writeErr(w, err)
		return
	}
	// The whole history of every session, oldest first, so the task page
	// reads top to bottom without scrolling back through a terminal.
	sessions, err := s.app.Store.Sessions(ctx, id, "")
	if err != nil {
		writeErr(w, err)
		return
	}
	type sessionWithMessages struct {
		sessionOut
		Messages []store.Message `json:"messages"`
	}
	out := make([]sessionWithMessages, 0, len(sessions))
	for i := len(sessions) - 1; i >= 0; i-- {
		msgs, err := s.app.Store.Messages(ctx, sessions[i].ID, 0)
		if err != nil {
			writeErr(w, err)
			return
		}
		out = append(out, sessionWithMessages{s.withLimits(sessions[i]), msgs})
	}
	schedules, err := s.app.Store.Schedules(ctx, id)
	if err != nil {
		writeErr(w, err)
		return
	}
	builds, err := s.taskRefs(ctx, t.ContextFrom)
	if err != nil {
		writeErr(w, err)
		return
	}
	usedByIDs, err := s.app.Store.ContextUsers(ctx, id)
	if err != nil {
		writeErr(w, err)
		return
	}
	usedBy, err := s.taskRefs(ctx, usedByIDs)
	if err != nil {
		writeErr(w, err)
		return
	}
	artifacts, err := s.app.Store.Artifacts(ctx, id)
	if err != nil {
		writeErr(w, err)
		return
	}
	memories, err := s.memoryRefs(ctx, t.Memories)
	if err != nil {
		writeErr(w, err)
		return
	}
	statuses, err := s.app.Store.AgentStatuses(ctx)
	if err != nil {
		writeErr(w, err)
		return
	}
	var agentStatus *store.AgentStatus
	if st, ok := statuses[id]; ok {
		agentStatus = &st
	}
	writeJSON(w, 200, map[string]any{"task": t, "sessions": out, "schedules": schedules, "builds_on": builds, "used_by": usedBy,
		"artifacts": artifacts, "memories": memories, "agent_status": agentStatus})
}

// taskRef names a related task for the task page.
type taskRef struct {
	ID        int64  `json:"id"`
	Title     string `json:"title"`
	ProjectID int64  `json:"project_id"`
	Archived  bool   `json:"archived"`
}

func (s *Server) taskRefs(ctx context.Context, ids []int64) ([]taskRef, error) {
	out := []taskRef{}
	for _, id := range ids {
		t, err := s.app.Store.Task(ctx, id)
		if err != nil {
			return nil, err
		}
		out = append(out, taskRef{t.ID, t.Title, t.ProjectID, t.ArchivedAt != nil})
	}
	return out, nil
}

func (s *Server) updateTask(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, err)
		return
	}
	cur, err := s.app.Store.Task(r.Context(), id)
	if err != nil {
		writeErr(w, err)
		return
	}
	// Partial update: only provided keys change.
	var in struct {
		Title       *string           `json:"title"`
		Description *string           `json:"description"`
		Kind        *string           `json:"kind"`
		Workdir     *string           `json:"workdir"`
		ProjectID   *int64            `json:"project_id"` // moves the task to the end of its lane there
		DueAt       json.RawMessage   `json:"due_at"`     // absent: keep, null: clear
		Fields      map[string]string `json:"fields"`
		ContextFrom *[]int64          `json:"context_from"` // replaces the list
		Memories    *[]int64          `json:"memories"`     // replaces the list
		Watched     *bool             `json:"watched"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, err)
		return
	}
	if in.Title != nil {
		if strings.TrimSpace(*in.Title) == "" {
			writeErr(w, badRequest{"title is required"})
			return
		}
		cur.Title = *in.Title
	}
	if in.Description != nil {
		cur.Description = *in.Description
	}
	if in.Kind != nil {
		cur.Kind = *in.Kind
	}
	if in.Workdir != nil {
		cur.Workdir = *in.Workdir
	}
	if in.DueAt != nil {
		cur.DueAt = nil
		if err := json.Unmarshal(in.DueAt, &cur.DueAt); err != nil {
			writeErr(w, badRequest{"due_at must be an RFC 3339 time or null"})
			return
		}
	}
	cur.Fields = in.Fields
	if in.ProjectID != nil {
		if err := s.checkProject(r.Context(), *in.ProjectID); err != nil {
			writeErr(w, err)
			return
		}
	}
	if in.ContextFrom != nil {
		if err := s.app.Store.SetContextFrom(r.Context(), id, *in.ContextFrom); err != nil {
			writeErr(w, err)
			return
		}
	}
	if in.Memories != nil {
		if err := s.app.Store.SetTaskMemories(r.Context(), id, *in.Memories); err != nil {
			writeErr(w, err)
			return
		}
	}
	if in.Watched != nil {
		if err := s.app.Store.SetWatched(r.Context(), id, *in.Watched); err != nil {
			writeErr(w, err)
			return
		}
	}
	if err := s.app.Store.UpdateTask(r.Context(), cur); err != nil {
		writeErr(w, err)
		return
	}
	if in.ProjectID != nil && *in.ProjectID != 0 {
		if err := s.app.Store.MoveTaskToProject(r.Context(), id, *in.ProjectID); err != nil {
			writeErr(w, err)
			return
		}
	}
	t, _ := s.app.Store.Task(r.Context(), id)
	writeJSON(w, 200, t)
}

func (s *Server) deleteTask(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, err)
		return
	}
	running, _ := s.app.Store.Sessions(r.Context(), id, store.StatusRunning)
	for _, x := range running {
		s.app.Cancel(x.ID)
	}
	if err := s.app.Store.DeleteTask(r.Context(), id); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(204)
}

func (s *Server) moveTask(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, err)
		return
	}
	var in struct {
		LaneID int64 `json:"lane_id"`
		Index  int   `json:"index"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, err)
		return
	}
	// Moving a task by hand is the user's next step after its agents' turns:
	// how they ended has been seen. Sessions the move starts are not touched.
	if err := s.app.Store.SeeTaskSessions(r.Context(), id); err != nil {
		writeErr(w, err)
		return
	}
	started, err := s.app.MoveTask(r.Context(), id, in.LaneID, in.Index)
	if err != nil {
		writeErr(w, err)
		return
	}
	if started == nil {
		started = []store.Session{}
	}
	writeJSON(w, 200, map[string]any{"started": started})
}

// searchTasks finds tasks of every project, archived ones included, by
// words of their title or description or by id (q); at most limit (default 20).
func (s *Server) searchTasks(w http.ResponseWriter, r *http.Request) {
	limit, err := queryID(r, "limit")
	if err != nil {
		writeErr(w, err)
		return
	}
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	tasks, err := s.app.Store.SearchTasks(r.Context(), r.URL.Query().Get("q"), int(limit))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, tasks)
}

// watchedTask is a watched task with its agent status, if it has one.
type watchedTask struct {
	store.Task
	AgentStatus *store.AgentStatus `json:"agent_status,omitempty"`
}

// watchedTasks lists the watched tasks on the board, of every project, each
// with its agent status.
func (s *Server) watchedTasks(w http.ResponseWriter, r *http.Request) {
	tasks, err := s.app.Store.WatchedTasks(r.Context())
	if err != nil {
		writeErr(w, err)
		return
	}
	statuses, err := s.app.Store.AgentStatuses(r.Context())
	if err != nil {
		writeErr(w, err)
		return
	}
	out := make([]watchedTask, len(tasks))
	for i, t := range tasks {
		out[i].Task = t
		if st, ok := statuses[t.ID]; ok {
			out[i].AgentStatus = &st
		}
	}
	writeJSON(w, 200, out)
}

// archivedTasks lists one project's archived tasks (?project_id=), or all of them.
func (s *Server) archivedTasks(w http.ResponseWriter, r *http.Request) {
	projectID, err := queryID(r, "project_id")
	if err != nil {
		writeErr(w, err)
		return
	}
	tasks, err := s.app.Store.ArchivedTasks(r.Context(), projectID)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, tasks)
}

func (s *Server) archiveTask(w http.ResponseWriter, r *http.Request) {
	s.setArchived(w, r, s.app.Store.ArchiveTask)
}

func (s *Server) unarchiveTask(w http.ResponseWriter, r *http.Request) {
	s.setArchived(w, r, s.app.Store.UnarchiveTask)
}

func (s *Server) setArchived(w http.ResponseWriter, r *http.Request, fn func(context.Context, int64) error) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, err)
		return
	}
	if err := fn(r.Context(), id); err != nil {
		writeErr(w, err)
		return
	}
	t, err := s.app.Store.Task(r.Context(), id)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, t)
}

// ---- sessions ----

type sessionInput struct {
	Prompt         string  `json:"prompt"`
	Title          string  `json:"title"`
	Mode           string  `json:"mode"`
	Agent          string  `json:"agent"` // empty: claude
	Model          string  `json:"model"`
	Workdir        string  `json:"workdir"`
	IncludeContext bool    `json:"include_context"`
	DoneLaneID     *int64  `json:"done_lane_id"` // task sessions only
	ProjectID      int64   `json:"project_id"`   // standalone sessions only: runs in the project's directory
	ContextFrom    []int64 `json:"context_from"` // standalone sessions only: the tasks its new task builds on
	Watched        bool    `json:"watched"`      // standalone sessions only: watch its new task
}

func (in *sessionInput) validate() error {
	if !validMode(in.Mode) {
		return badRequest{"unknown mode"}
	}
	if strings.TrimSpace(in.Prompt) == "" {
		return badRequest{"prompt is required"}
	}
	if in.Agent != "" && !slices.Contains(agent.Agents, in.Agent) {
		return badRequest{"unknown agent"}
	}
	if in.Model = strings.TrimSpace(in.Model); !validModel(in.Model) {
		return badRequest{"invalid model"}
	}
	dir, err := resolveWorkdir(in.Workdir)
	in.Workdir = dir
	return err
}

// validModel accepts empty (the default) or anything that passes as one --model argument.
func validModel(m string) bool {
	return !strings.ContainsAny(m, " \t\n") && !strings.HasPrefix(m, "-")
}

// resolveWorkdir expands a leading ~ and checks the directory exists.
// Empty stays empty so the app default applies.
func resolveWorkdir(dir string) (string, error) {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return "", nil
	}
	if dir == "~" || strings.HasPrefix(dir, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		dir = filepath.Join(home, dir[1:])
	}
	if !filepath.IsAbs(dir) {
		return "", badRequest{"workdir must be an absolute path"}
	}
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		return "", badRequest{"workdir is not a directory: " + dir}
	}
	return filepath.Clean(dir), nil
}

// resolveWorkdirs resolves each directory like resolveWorkdir, dropping
// blanks and duplicates and keeping the order.
func resolveWorkdirs(in []string) ([]string, error) {
	out := []string{}
	seen := map[string]bool{}
	for _, d := range in {
		dir, err := resolveWorkdir(d)
		if err != nil {
			return nil, err
		}
		if dir != "" && !seen[dir] {
			seen[dir] = true
			out = append(out, dir)
		}
	}
	return out, nil
}

// startStandaloneSession runs an agent session in a new task made from its prompt.
func (s *Server) startStandaloneSession(w http.ResponseWriter, r *http.Request) {
	var in sessionInput
	if err := decode(r, &in); err != nil {
		writeErr(w, err)
		return
	}
	if err := in.validate(); err != nil {
		writeErr(w, err)
		return
	}
	if err := s.checkProject(r.Context(), in.ProjectID); err != nil {
		writeErr(w, err)
		return
	}
	sess, err := s.app.StartSessionInNewTask(r.Context(), app.StartOpts{
		Prompt: in.Prompt, Title: in.Title, Mode: in.Mode, Agent: in.Agent, Model: in.Model, Workdir: in.Workdir, ProjectID: in.ProjectID,
		ContextFrom: in.ContextFrom, Watched: in.Watched,
	})
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 201, sess)
}

func (s *Server) startSession(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, err)
		return
	}
	var in sessionInput
	if err := decode(r, &in); err != nil {
		writeErr(w, err)
		return
	}
	if err := in.validate(); err != nil {
		writeErr(w, err)
		return
	}
	prompt := in.Prompt
	if in.IncludeContext {
		t, err := s.app.Store.Task(r.Context(), id)
		if err != nil {
			writeErr(w, err)
			return
		}
		prompt = s.app.TaskBrief(r.Context(), t) + "\n\n---\n\n" + prompt
	}
	if err := s.checkLane(r.Context(), in.DoneLaneID, "done_lane_id"); err != nil {
		writeErr(w, err)
		return
	}
	sess, err := s.app.StartSession(r.Context(), app.StartOpts{
		TaskID: id, Prompt: prompt, Title: in.Title, Mode: in.Mode, Agent: in.Agent, Model: in.Model, Workdir: in.Workdir,
		DoneLaneID: in.DoneLaneID,
	})
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 201, sess)
}

func (s *Server) listSessions(w http.ResponseWriter, r *http.Request) {
	taskID, _ := strconv.ParseInt(r.URL.Query().Get("task_id"), 10, 64)
	xs, err := s.app.Store.Sessions(r.Context(), taskID, r.URL.Query().Get("status"))
	if err != nil {
		writeErr(w, err)
		return
	}
	out := make([]sessionOut, len(xs))
	for i, x := range xs {
		out[i] = s.withLimits(x)
	}
	writeJSON(w, 200, out)
}

// sessionOut is a session with its live state, which is never stored: usage
// limits and whether its summary or memories are being written.
type sessionOut struct {
	store.Session
	Limits      agent.RateLimits `json:"limits"`
	Summarizing bool             `json:"summarizing"`
	Memorizing  bool             `json:"memorizing"` // its task's memories are being updated
}

func (s *Server) withLimits(x store.Session) sessionOut {
	return sessionOut{x, s.app.Limits(x.ID), s.app.Summarizing(x.ID), s.app.Memorizing(x.ID)}
}

func (s *Server) getSession(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, err)
		return
	}
	sess, err := s.app.Store.Session(r.Context(), id)
	if err != nil {
		writeErr(w, err)
		return
	}
	after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
	msgs, err := s.app.Store.Messages(r.Context(), id, after)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"session": s.withLimits(sess), "messages": msgs})
}

func (s *Server) sendMessage(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, err)
		return
	}
	var in struct {
		Text string `json:"text"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, err)
		return
	}
	if strings.TrimSpace(in.Text) == "" {
		writeErr(w, badRequest{"text is required"})
		return
	}
	queued, err := s.app.SendMessage(r.Context(), id, in.Text)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 202, map[string]bool{"queued": queued})
}

// deleteQueuedMessage drops a message queued for a session's next turn.
func (s *Server) deleteQueuedMessage(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, err)
		return
	}
	if err := s.app.Store.DeleteQueuedMessage(r.Context(), id); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(204)
}

// updateSession changes where the task moves when the session's turn finishes.
func (s *Server) updateSession(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, err)
		return
	}
	var in struct {
		DoneLaneID *int64 `json:"done_lane_id"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, err)
		return
	}
	sess, err := s.app.Store.Session(r.Context(), id)
	if err != nil {
		writeErr(w, err)
		return
	}
	if sess.TaskID == 0 && in.DoneLaneID != nil {
		writeErr(w, badRequest{"a session without a task has no lane to move"})
		return
	}
	if err := s.checkLane(r.Context(), in.DoneLaneID, "done_lane_id"); err != nil {
		writeErr(w, err)
		return
	}
	if err := s.app.Store.SetSessionDoneLane(r.Context(), id, in.DoneLaneID); err != nil {
		writeErr(w, err)
		return
	}
	sess, err = s.app.Store.Session(r.Context(), id)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, s.withLimits(sess))
}

// checkLane rejects a lane id that doesn't exist; nil is fine. field names it in the error.
func (s *Server) checkLane(ctx context.Context, id *int64, field string) error {
	if id == nil {
		return nil
	}
	_, err := s.app.Store.Lane(ctx, *id)
	if errors.Is(err, store.ErrNotFound) {
		return badRequest{"unknown " + field}
	}
	return err
}

func (s *Server) cancelSession(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, err)
		return
	}
	s.app.Cancel(id)
	w.WriteHeader(202)
}

func (s *Server) dismissNeedsInput(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, err)
		return
	}
	if err := s.app.DismissNeedsInput(r.Context(), id); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(204)
}

func (s *Server) resumeSession(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, err)
		return
	}
	if err := s.app.Resume(r.Context(), id); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(202)
}

// handoffSession continues the session in a fresh one, started from a brief
// the session's agent writes; note is the user's instruction for the new one.
func (s *Server) handoffSession(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, err)
		return
	}
	var in struct {
		Note string `json:"note"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, err)
		return
	}
	if err := s.app.Handoff(r.Context(), id, in.Note); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(202)
}

// summarizeSession (re)writes the session's summary in the background.
func (s *Server) summarizeSession(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, err)
		return
	}
	if err := s.app.Summarize(r.Context(), id); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(202)
}

// memorizeSession updates the memories of the session's task from it in the background.
func (s *Server) memorizeSession(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, err)
		return
	}
	if err := s.app.UpdateMemories(r.Context(), id); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(202)
}

// ---- rules ----

func (s *Server) listRules(w http.ResponseWriter, r *http.Request) {
	rules, err := s.app.Store.Rules(r.Context())
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, rules)
}

func (s *Server) saveRule(w http.ResponseWriter, r *http.Request) {
	var rule store.Rule
	if err := decode(r, &rule); err != nil {
		writeErr(w, err)
		return
	}
	rule.ID = 0
	if r.PathValue("id") != "" {
		id, err := pathID(r)
		if err != nil {
			writeErr(w, err)
			return
		}
		rule.ID = id
	}
	if strings.TrimSpace(rule.Name) == "" || rule.ToLaneID == 0 {
		writeErr(w, badRequest{"name and to_lane_id are required"})
		return
	}
	switch hasPrompt := strings.TrimSpace(rule.Prompt) != ""; {
	case hasPrompt && rule.SkillID != nil:
		writeErr(w, badRequest{"a rule takes a prompt or a skill_id, not both"})
		return
	case !hasPrompt && rule.SkillID == nil:
		// Its action is moving the task on once an agent turn on it finishes.
		if rule.DoneLaneID == nil {
			writeErr(w, badRequest{"a rule needs a prompt, a skill_id, or a done_lane_id to move the task to after an agent turn"})
			return
		}
		if *rule.DoneLaneID == rule.ToLaneID {
			writeErr(w, badRequest{"a rule moving the task after an agent turn needs a done_lane_id other than to_lane_id, the lane it watches"})
			return
		}
		rule.Mode, rule.Workdir = "", ""
	}
	if !validMode(rule.Mode) {
		writeErr(w, badRequest{"unknown mode"})
		return
	}
	if err := s.checkLane(r.Context(), rule.DoneLaneID, "done_lane_id"); err != nil {
		writeErr(w, err)
		return
	}
	if rule.ProjectID != nil && *rule.ProjectID == 0 {
		rule.ProjectID = nil // any project
	}
	if rule.ProjectID != nil {
		if err := s.checkProject(r.Context(), *rule.ProjectID); err != nil {
			writeErr(w, err)
			return
		}
	}
	if rule.SkillID != nil {
		if _, err := s.app.Store.Skill(r.Context(), *rule.SkillID); errors.Is(err, store.ErrNotFound) {
			writeErr(w, badRequest{"unknown skill_id"})
			return
		} else if err != nil {
			writeErr(w, err)
			return
		}
	}
	if _, err := app.RenderPrompt(rule.Prompt, store.Task{}); err != nil {
		writeErr(w, badRequest{"prompt template: " + err.Error()})
		return
	}
	saved, err := s.app.Store.SaveRule(r.Context(), rule)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, saved)
}

func (s *Server) deleteRule(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, err)
		return
	}
	if err := s.app.Store.DeleteRule(r.Context(), id); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(204)
}

// ---- skills ----

func (s *Server) listSkills(w http.ResponseWriter, r *http.Request) {
	xs, err := s.app.Store.Skills(r.Context())
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, xs)
}

func (s *Server) createSkill(w http.ResponseWriter, r *http.Request) {
	var x store.Skill
	if err := decode(r, &x); err != nil {
		writeErr(w, err)
		return
	}
	x.ID = 0
	s.saveSkill(w, r, x)
}

// updateSkill changes the given keys only; automations, if given, replace the list.
func (s *Server) updateSkill(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, err)
		return
	}
	x, err := s.app.Store.Skill(r.Context(), id)
	if err != nil {
		writeErr(w, err)
		return
	}
	cur := x.Automations
	x.Automations = nil // decoding into the old list would mix old and new entries
	if err := decode(r, &x); err != nil {
		writeErr(w, err)
		return
	}
	if x.Automations == nil {
		x.Automations = cur
	}
	x.ID = id
	s.saveSkill(w, r, x)
}

func (s *Server) saveSkill(w http.ResponseWriter, r *http.Request, x store.Skill) {
	x.Name = strings.TrimSpace(x.Name)
	if x.Name == "" || strings.TrimSpace(x.Body) == "" {
		writeErr(w, badRequest{"name and body are required"})
		return
	}
	if x.Model = strings.TrimSpace(x.Model); !validModel(x.Model) {
		writeErr(w, badRequest{"invalid model"})
		return
	}
	if _, err := app.RenderPrompt(x.Body, store.Task{}); err != nil {
		writeErr(w, badRequest{"body template: " + err.Error()})
		return
	}
	if x.Automations == nil {
		x.Automations = []store.SkillAutomation{}
	}
	for i := range x.Automations {
		a := &x.Automations[i]
		if a.ProjectID != nil && *a.ProjectID == 0 {
			a.ProjectID = nil
		}
		if a.LaneID != nil && *a.LaneID == 0 {
			a.LaneID = nil
		}
		if a.Kind != "" && !slices.Contains(taskKinds, a.Kind) {
			writeErr(w, badRequest{"unknown kind " + a.Kind})
			return
		}
		if a.ProjectID != nil {
			if err := s.checkProject(r.Context(), *a.ProjectID); err != nil {
				writeErr(w, err)
				return
			}
		}
		if err := s.checkLane(r.Context(), a.LaneID, "lane_id"); err != nil {
			writeErr(w, err)
			return
		}
	}
	saved, err := s.app.Store.SaveSkill(r.Context(), x)
	if errors.Is(err, store.ErrSkillExists) {
		err = badRequest{err.Error()}
	}
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, saved)
}

func (s *Server) deleteSkill(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, err)
		return
	}
	if err := s.app.Store.DeleteSkill(r.Context(), id); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(204)
}

// runSkill applies a skill to an idle session as its next turn.
func (s *Server) runSkill(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, err)
		return
	}
	var in struct {
		SkillID int64 `json:"skill_id"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, err)
		return
	}
	if _, err := s.app.Store.Skill(r.Context(), in.SkillID); errors.Is(err, store.ErrNotFound) {
		writeErr(w, badRequest{"unknown skill_id"})
		return
	}
	if err := s.app.RunSkill(r.Context(), id, in.SkillID); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(202)
}

func (s *Server) listArtifacts(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, err)
		return
	}
	xs, err := s.app.Store.Artifacts(r.Context(), id)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, xs)
}

func (s *Server) deleteArtifact(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, err)
		return
	}
	if err := s.app.Store.DeleteArtifact(r.Context(), id); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(204)
}

// ---- schedules ----

func (s *Server) listSchedules(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, err)
		return
	}
	xs, err := s.app.Store.Schedules(r.Context(), id)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, xs)
}

func (s *Server) createSchedule(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, err)
		return
	}
	if _, err := s.app.Store.Task(r.Context(), id); err != nil {
		writeErr(w, err)
		return
	}
	var x store.Schedule
	if err := decode(r, &x); err != nil {
		writeErr(w, err)
		return
	}
	x.ID, x.TaskID = 0, id
	s.saveSchedule(w, r, x)
}

func (s *Server) updateSchedule(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, err)
		return
	}
	cur, err := s.app.Store.Schedule(r.Context(), id)
	if err != nil {
		writeErr(w, err)
		return
	}
	var x store.Schedule
	if err := decode(r, &x); err != nil {
		writeErr(w, err)
		return
	}
	x.ID, x.TaskID = cur.ID, cur.TaskID
	s.saveSchedule(w, r, x)
}

func (s *Server) saveSchedule(w http.ResponseWriter, r *http.Request, x store.Schedule) {
	if !validMode(x.Mode) {
		writeErr(w, badRequest{"unknown mode"})
		return
	}
	if err := s.checkLane(r.Context(), x.DoneLaneID, "done_lane_id"); err != nil {
		writeErr(w, err)
		return
	}
	if x.Action == store.ActionAgent {
		if strings.TrimSpace(x.Prompt) == "" {
			writeErr(w, badRequest{"prompt is required for agent schedules"})
			return
		}
		if _, err := app.RenderPrompt(x.Prompt, store.Task{}); err != nil {
			writeErr(w, badRequest{"prompt template: " + err.Error()})
			return
		}
	}
	saved, err := s.app.SaveSchedule(r.Context(), x)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, saved)
}

func (s *Server) deleteSchedule(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, err)
		return
	}
	if err := s.app.Store.DeleteSchedule(r.Context(), id); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(204)
}

// ---- notifications ----

func (s *Server) listNotifications(w http.ResponseWriter, r *http.Request) {
	xs, err := s.app.Store.Notifications(r.Context(), r.URL.Query().Get("unread") == "1")
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, xs)
}

func (s *Server) readNotification(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, err)
		return
	}
	if err := s.app.Store.MarkNotificationRead(r.Context(), id); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(204)
}

func (s *Server) archiveNotifications(w http.ResponseWriter, r *http.Request) {
	if err := s.app.Store.ArchiveNotifications(r.Context()); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(204)
}

// ---- memories ----

// memoryRef names a memory for the task page.
type memoryRef struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

func (s *Server) memoryRefs(ctx context.Context, ids []int64) ([]memoryRef, error) {
	out := []memoryRef{}
	for _, id := range ids {
		m, err := s.app.Store.Memory(ctx, id)
		if err != nil {
			return nil, err
		}
		out = append(out, memoryRef{m.ID, m.Name})
	}
	return out, nil
}

// listMemories lists the memories without their bodies (getMemory has
// them), the most recently used first; with q only those matching its words,
// best first.
func (s *Server) listMemories(w http.ResponseWriter, r *http.Request) {
	var xs []store.Memory
	var err error
	if q, ok := r.URL.Query()["q"]; ok {
		xs, err = s.app.Store.SearchMemories(r.Context(), strings.Join(q, " "))
	} else {
		xs, err = s.app.Store.Memories(r.Context())
	}
	if err != nil {
		writeErr(w, err)
		return
	}
	for i := range xs {
		xs[i].Body = ""
	}
	writeJSON(w, 200, xs)
}

// createMemory adds a memory. With task_id it is linked to that task too,
// and with fill it is then written from the task's sessions in the background.
func (s *Server) createMemory(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var in struct {
		store.Memory
		TaskID int64 `json:"task_id"`
		Fill   bool  `json:"fill"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, err)
		return
	}
	x := in.Memory
	if err := checkMemory(&x); err != nil {
		writeErr(w, err)
		return
	}
	var task store.Task
	if in.TaskID != 0 {
		var err error
		if task, err = s.app.Store.Task(ctx, in.TaskID); errors.Is(err, store.ErrNotFound) {
			writeErr(w, badRequest{"unknown task_id"})
			return
		} else if err != nil {
			writeErr(w, err)
			return
		}
	}
	x, err := s.app.Store.CreateMemory(ctx, x)
	if err != nil {
		writeErr(w, err)
		return
	}
	if in.TaskID != 0 {
		if err := s.app.Store.SetTaskMemories(ctx, task.ID, append(task.Memories, x.ID)); err != nil {
			writeErr(w, err)
			return
		}
		if in.Fill {
			s.app.FillMemory(task.ID, x.ID)
		}
		if x, err = s.app.Store.Memory(ctx, x.ID); err != nil {
			writeErr(w, err)
			return
		}
	}
	writeJSON(w, 201, x)
}

// checkMemory trims and checks a memory's attributes.
func checkMemory(x *store.Memory) error {
	if x.Name = strings.TrimSpace(x.Name); x.Name == "" {
		return badRequest{"name is required"}
	}
	x.Description = strings.TrimSpace(x.Description)
	return nil
}

// getMemory returns a memory with its revisions (without bodies), the tasks
// linked to it and whether it is being written from a task's sessions.
func (s *Server) getMemory(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, err := pathID(r)
	if err != nil {
		writeErr(w, err)
		return
	}
	m, err := s.app.Store.Memory(ctx, id)
	if err != nil {
		writeErr(w, err)
		return
	}
	revs, err := s.app.Store.MemoryRevisions(ctx, id)
	if err != nil {
		writeErr(w, err)
		return
	}
	ids, err := s.app.Store.MemoryTasks(ctx, id)
	if err != nil {
		writeErr(w, err)
		return
	}
	tasks, err := s.taskRefs(ctx, ids)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"memory": m, "revisions": revs, "tasks": tasks, "filling": s.app.Filling(id)})
}

// updateMemory changes the given keys only. A new body is a revision; with
// base_revision it is refused if the memory changed since that revision.
func (s *Server) updateMemory(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, err := pathID(r)
	if err != nil {
		writeErr(w, err)
		return
	}
	cur, err := s.app.Store.Memory(ctx, id)
	if err != nil {
		writeErr(w, err)
		return
	}
	var in struct {
		Name         *string `json:"name"`
		Description  *string `json:"description"`
		Body         *string `json:"body"`
		BaseRevision *int64  `json:"base_revision"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, err)
		return
	}
	if in.Name != nil || in.Description != nil {
		if in.Name != nil {
			cur.Name = *in.Name
		}
		if in.Description != nil {
			cur.Description = *in.Description
		}
		if err := checkMemory(&cur); err != nil {
			writeErr(w, err)
			return
		}
		if err := s.app.Store.UpdateMemory(ctx, cur); err != nil {
			writeErr(w, err)
			return
		}
	}
	if in.Body != nil {
		if _, _, err := s.app.Store.SetMemoryBody(ctx, store.MemoryRevision{MemoryID: id, Body: *in.Body, Source: store.SourceUser},
			in.BaseRevision); err != nil {
			writeErr(w, err)
			return
		}
	}
	m, err := s.app.Store.Memory(ctx, id)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, m)
}

func (s *Server) deleteMemory(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, err)
		return
	}
	if err := s.app.Store.DeleteMemory(r.Context(), id); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(204)
}

// addMemoryNote appends a note to the memory; the next update from a
// session works it in.
func (s *Server) addMemoryNote(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, err)
		return
	}
	var in struct {
		Text string `json:"text"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, err)
		return
	}
	if strings.TrimSpace(in.Text) == "" {
		writeErr(w, badRequest{"text is required"})
		return
	}
	if _, err := s.app.Store.AppendMemoryNote(r.Context(), store.MemoryRevision{MemoryID: id, Body: in.Text, Source: store.SourceNote}); err != nil {
		writeErr(w, err)
		return
	}
	m, err := s.app.Store.Memory(r.Context(), id)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, m)
}

// getMemoryRevision returns a revision with its body and its diff to the one before.
func (s *Server) getMemoryRevision(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, err)
		return
	}
	rev, prev, err := s.app.Store.MemoryRevision(r.Context(), id)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"revision": rev, "diff": app.LineDiff(prev, rev.Body)})
}

// restoreMemoryRevision makes an earlier revision's body the memory's body again.
func (s *Server) restoreMemoryRevision(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, err)
		return
	}
	rev, _, err := s.app.Store.MemoryRevision(r.Context(), id)
	if err != nil {
		writeErr(w, err)
		return
	}
	if _, _, err := s.app.Store.SetMemoryBody(r.Context(), store.MemoryRevision{MemoryID: rev.MemoryID, Body: rev.Body, Source: store.SourceUser}, nil); err != nil {
		writeErr(w, err)
		return
	}
	m, err := s.app.Store.Memory(r.Context(), rev.MemoryID)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, m)
}
