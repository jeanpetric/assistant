// Package server exposes the JSON API and serves the embedded web UI.
package server

import (
	"embed"
	"encoding/json"
	"errors"
	"io/fs"
	"log"
	"mime"
	"net"
	"net/http"
	"strconv"
	"strings"

	"github.com/jeanpetric/assistant/internal/agent"
	"github.com/jeanpetric/assistant/internal/app"
	"github.com/jeanpetric/assistant/internal/store"
)

//go:embed web
var webFS embed.FS

type Server struct {
	app *app.App
	mux *http.ServeMux
}

func New(a *app.App) http.Handler {
	s := &Server{app: a, mux: http.NewServeMux()}
	s.routes()
	return guard(s.mux)
}

func (s *Server) routes() {
	m := s.mux
	m.HandleFunc("GET /api/board", s.board)
	m.HandleFunc("GET /api/meta", s.meta)
	m.HandleFunc("GET /api/settings", s.getSettings)
	m.HandleFunc("PUT /api/settings", s.saveSettings)
	m.HandleFunc("GET /api/archive", s.archivedTasks)

	m.HandleFunc("POST /api/lanes", s.createLane)
	m.HandleFunc("PATCH /api/lanes/{id}", s.updateLane)
	m.HandleFunc("DELETE /api/lanes/{id}", s.deleteLane)
	m.HandleFunc("POST /api/lanes/reorder", s.reorderLanes)

	m.HandleFunc("POST /api/projects", s.createProject)
	m.HandleFunc("PATCH /api/projects/{id}", s.updateProject)
	m.HandleFunc("DELETE /api/projects/{id}", s.deleteProject)
	m.HandleFunc("POST /api/projects/reorder", s.reorderProjects)

	m.HandleFunc("POST /api/fields", s.saveField)
	m.HandleFunc("DELETE /api/fields/{key}", s.deleteField)

	m.HandleFunc("POST /api/tasks", s.createTask)
	m.HandleFunc("GET /api/tasks/{id}", s.getTask)
	m.HandleFunc("PATCH /api/tasks/{id}", s.updateTask)
	m.HandleFunc("DELETE /api/tasks/{id}", s.deleteTask)
	m.HandleFunc("POST /api/tasks/{id}/move", s.moveTask)
	m.HandleFunc("POST /api/tasks/{id}/archive", s.archiveTask)
	m.HandleFunc("POST /api/tasks/{id}/unarchive", s.unarchiveTask)
	m.HandleFunc("POST /api/tasks/{id}/sessions", s.startSession)
	m.HandleFunc("GET /api/tasks/{id}/schedules", s.listSchedules)
	m.HandleFunc("POST /api/tasks/{id}/schedules", s.createSchedule)

	m.HandleFunc("GET /api/sessions", s.listSessions)
	m.HandleFunc("POST /api/sessions", s.startStandaloneSession)
	m.HandleFunc("GET /api/sessions/{id}", s.getSession)
	m.HandleFunc("PATCH /api/sessions/{id}", s.updateSession)
	m.HandleFunc("POST /api/sessions/{id}/messages", s.sendMessage)
	m.HandleFunc("POST /api/sessions/{id}/cancel", s.cancelSession)
	m.HandleFunc("POST /api/sessions/{id}/resume", s.resumeSession)
	m.HandleFunc("POST /api/sessions/{id}/handoff", s.handoffSession)

	m.HandleFunc("GET /api/rules", s.listRules)
	m.HandleFunc("POST /api/rules", s.saveRule)
	m.HandleFunc("PUT /api/rules/{id}", s.saveRule)
	m.HandleFunc("DELETE /api/rules/{id}", s.deleteRule)

	m.HandleFunc("PUT /api/schedules/{id}", s.updateSchedule)
	m.HandleFunc("DELETE /api/schedules/{id}", s.deleteSchedule)

	m.HandleFunc("GET /api/notifications", s.listNotifications)
	m.HandleFunc("POST /api/notifications/{id}/read", s.readNotification)

	m.HandleFunc("POST /mcp", s.mcp)
	m.HandleFunc("GET /mcp", mcpNoStream)
	m.HandleFunc("DELETE /mcp", mcpNoStream)

	static, _ := fs.Sub(webFS, "web")
	m.Handle("GET /", http.FileServerFS(static))
}

// guard protects a localhost server that can launch agents from browser-based
// attacks: DNS rebinding (Host check) and cross-site form posts (JSON-only writes;
// a cross-origin JSON request needs a CORS preflight, which we never grant).
func guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := r.Host
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		if host != "localhost" && host != "127.0.0.1" && host != "::1" {
			http.Error(w, "forbidden host", http.StatusForbidden)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			if origin := r.Header.Get("Origin"); origin != "" && !strings.HasSuffix(origin, "://"+r.Host) {
				http.Error(w, "forbidden origin", http.StatusForbidden)
				return
			}
			if r.ContentLength != 0 || r.Body != http.NoBody {
				ct, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
				if ct != "application/json" {
					http.Error(w, "content-type must be application/json", http.StatusUnsupportedMediaType)
					return
				}
			}
		}
		next.ServeHTTP(w, r)
	})
}

// ---- helpers ----

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	switch {
	case errors.Is(err, store.ErrNotFound):
		status = http.StatusNotFound
	case errors.Is(err, store.ErrLaneNotEmpty), errors.Is(err, store.ErrProjectNotEmpty), errors.Is(err, store.ErrLastProject),
		errors.Is(err, app.ErrBusy), errors.Is(err, app.ErrNoResume):
		status = http.StatusConflict
	case errors.As(err, new(badRequest)), errors.Is(err, app.ErrBadAction), errors.Is(err, app.ErrBadCron), errors.Is(err, store.ErrBadContext):
		status = http.StatusBadRequest
	}
	if status == http.StatusInternalServerError {
		log.Printf("error: %v", err)
	}
	writeJSON(w, status, map[string]string{"error": err.Error()})
}

type badRequest struct{ msg string }

func (b badRequest) Error() string { return b.msg }

func decode(r *http.Request, v any) error {
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		return badRequest{"invalid JSON: " + err.Error()}
	}
	return nil
}

func pathID(r *http.Request) (int64, error) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		return 0, badRequest{"invalid id"}
	}
	return id, nil
}

// queryID reads an optional id from the query string; absent is 0.
func queryID(r *http.Request, key string) (int64, error) {
	v := r.URL.Query().Get(key)
	if v == "" {
		return 0, nil
	}
	id, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0, badRequest{"invalid " + key}
	}
	return id, nil
}

func validMode(m string) bool {
	if m == "" {
		return true
	}
	for _, x := range agent.Modes {
		if x == m {
			return true
		}
	}
	return false
}
