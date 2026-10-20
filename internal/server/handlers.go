package server

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/jeanpetric/assistant/internal/agent"
	"github.com/jeanpetric/assistant/internal/app"
	"github.com/jeanpetric/assistant/internal/store"
)

// ---- board & meta ----

func (s *Server) board(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	lanes, err := s.app.Store.Lanes(ctx)
	if err != nil {
		writeErr(w, err)
		return
	}
	tasks, err := s.app.Store.Tasks(ctx)
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
	runningByTask := map[int64]int{}
	for _, x := range running {
		if x.TaskID != 0 {
			runningByTask[x.TaskID]++
		}
	}
	writeJSON(w, 200, map[string]any{"lanes": lanes, "tasks": tasks, "fields": fields, "running": runningByTask})
}

func (s *Server) meta(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{
		"modes":        agent.Modes,
		"models":       agent.Models,
		"default_mode": s.app.DefaultMode,
		"workdir":      s.app.DefaultWorkdir,
		"kinds":        []string{"task", "coding", "research", "discussion", "reminder"},
	})
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
		store.Session
		Messages []store.Message `json:"messages"`
	}
	out := make([]sessionWithMessages, 0, len(sessions))
	for i := len(sessions) - 1; i >= 0; i-- {
		msgs, err := s.app.Store.Messages(ctx, sessions[i].ID, 0)
		if err != nil {
			writeErr(w, err)
			return
		}
		out = append(out, sessionWithMessages{sessions[i], msgs})
	}
	schedules, err := s.app.Store.Schedules(ctx, id)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"task": t, "sessions": out, "schedules": schedules})
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
		DueAt       json.RawMessage   `json:"due_at"` // absent: keep, null: clear
		Fields      map[string]string `json:"fields"`
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
	if err := s.app.Store.UpdateTask(r.Context(), cur); err != nil {
		writeErr(w, err)
		return
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

// ---- sessions ----

type sessionInput struct {
	Prompt         string `json:"prompt"`
	Title          string `json:"title"`
	Mode           string `json:"mode"`
	Model          string `json:"model"`
	Workdir        string `json:"workdir"`
	IncludeContext bool   `json:"include_context"`
}

func (in *sessionInput) validate() error {
	if !validMode(in.Mode) {
		return badRequest{"unknown mode"}
	}
	if strings.TrimSpace(in.Prompt) == "" {
		return badRequest{"prompt is required"}
	}
	in.Model = strings.TrimSpace(in.Model)
	if strings.ContainsAny(in.Model, " \t\n") || strings.HasPrefix(in.Model, "-") {
		return badRequest{"invalid model"}
	}
	dir, err := resolveWorkdir(in.Workdir)
	in.Workdir = dir
	return err
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

// startStandaloneSession runs an agent session that belongs to no task.
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
	sess, err := s.app.StartSession(r.Context(), app.StartOpts{
		Prompt: in.Prompt, Title: in.Title, Mode: in.Mode, Model: in.Model, Workdir: in.Workdir,
	})
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 201, sess)
}

func (s *Server) recentWorkdirs(w http.ResponseWriter, r *http.Request) {
	dirs, err := s.app.Store.RecentWorkdirs(r.Context(), 30)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, dirs)
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
		prompt = app.TaskContext(t) + "\n\n---\n\n" + prompt
	}
	sess, err := s.app.StartSession(r.Context(), app.StartOpts{
		TaskID: id, Prompt: prompt, Title: in.Title, Mode: in.Mode, Model: in.Model, Workdir: in.Workdir,
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
	writeJSON(w, 200, xs)
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
	writeJSON(w, 200, map[string]any{"session": sess, "messages": msgs})
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
	if err := s.app.SendMessage(r.Context(), id, in.Text); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(202)
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
	if strings.TrimSpace(rule.Name) == "" || strings.TrimSpace(rule.Prompt) == "" || rule.ToLaneID == 0 {
		writeErr(w, badRequest{"name, prompt and to_lane_id are required"})
		return
	}
	if !validMode(rule.Mode) {
		writeErr(w, badRequest{"unknown mode"})
		return
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
