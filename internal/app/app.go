// Package app holds the behaviour that ties the store to agents:
// starting/continuing sessions, kanban transition rules and schedules.
package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"text/template"
	"time"

	"github.com/jeanpetric/assistant/internal/agent"
	"github.com/jeanpetric/assistant/internal/cron"
	"github.com/jeanpetric/assistant/internal/store"
)

var (
	ErrBusy      = errors.New("session is running")
	ErrNoResume  = errors.New("session has no agent session to resume")
	ErrBadAction = errors.New("action must be agent or remind")
	ErrBadCron   = errors.New("invalid cron expression")
)

type App struct {
	Store          *store.Store
	Runner         agent.Runner
	DefaultWorkdir string
	DefaultMode    string
	Now            func() time.Time

	mu       sync.Mutex
	running  map[int64]context.CancelFunc
	stopping bool // set by Shutdown: cancelled turns end as interrupted, not cancelled
	wg       sync.WaitGroup
	limits   map[int64]agent.RateLimits // per session, last reported; in memory only
}

func New(s *store.Store, r agent.Runner, workdir, mode string) *App {
	return &App{Store: s, Runner: r, DefaultWorkdir: workdir, DefaultMode: mode, Now: time.Now,
		running: map[int64]context.CancelFunc{}, limits: map[int64]agent.RateLimits{}}
}

// Wait blocks until all agent turns started so far have finished.
func (a *App) Wait() { a.wg.Wait() }

// Shutdown cancels running turns and waits for them. The turns are marked
// interrupted so they can be resumed after the next start.
func (a *App) Shutdown() {
	a.mu.Lock()
	a.stopping = true
	for _, cancel := range a.running {
		cancel()
	}
	a.mu.Unlock()
	a.wg.Wait()
}

type StartOpts struct {
	TaskID  int64 // 0 starts a standalone session
	Prompt  string
	Title   string
	Mode    string
	Model   string
	Workdir string
	Trigger string // manual | rule:<name> | schedule:<id>
	// DoneLaneID moves the task to this lane when a turn finishes successfully
	// (see moveWhenDone). Nil leaves the task where it is.
	DoneLaneID *int64
}

// StartSession creates a session, for the task if one is given, and runs its
// first turn in the background.
func (a *App) StartSession(ctx context.Context, o StartOpts) (store.Session, error) {
	var task store.Task
	if o.TaskID != 0 {
		var err error
		if task, err = a.Store.Task(ctx, o.TaskID); err != nil {
			return store.Session{}, err
		}
	}
	if strings.TrimSpace(o.Prompt) == "" {
		return store.Session{}, errors.New("prompt is required")
	}
	if o.Mode == "" {
		o.Mode = a.DefaultMode
	}
	if o.Workdir == "" {
		o.Workdir = task.Workdir
	}
	if o.Workdir == "" {
		o.Workdir = a.DefaultWorkdir
	}
	if o.Trigger == "" {
		o.Trigger = "manual"
	}
	if o.Title == "" {
		o.Title = firstLine(o.Prompt, 80)
	}
	if o.TaskID == 0 {
		o.DoneLaneID = nil
	}
	sess, err := a.Store.CreateSession(ctx, store.Session{
		TaskID: o.TaskID, Title: o.Title, Mode: o.Mode, Model: o.Model, Workdir: o.Workdir, Trigger: o.Trigger,
		DoneLaneID: o.DoneLaneID,
	})
	if err != nil {
		return sess, err
	}
	return sess, a.launch(sess, o.Prompt, "")
}

// SendMessage continues an idle session with a follow-up prompt.
func (a *App) SendMessage(ctx context.Context, sessionID int64, text string) error {
	sess, err := a.Store.Session(ctx, sessionID)
	if err != nil {
		return err
	}
	a.mu.Lock()
	_, busy := a.running[sessionID]
	a.mu.Unlock()
	if busy {
		return ErrBusy
	}
	if sess.ClaudeSessionID == "" {
		return ErrNoResume
	}
	return a.launch(sess, text, sess.ClaudeSessionID)
}

// ResumePrompt is sent to an agent whose turn was cut short.
const ResumePrompt = "Your previous run was interrupted before it finished. Continue from where you left off."

// Resume restarts a session whose last turn did not finish (interrupted,
// cancelled or failed). With an agent session it continues that conversation;
// without one (the agent died before starting) it reruns the first prompt.
func (a *App) Resume(ctx context.Context, sessionID int64) error {
	sess, err := a.Store.Session(ctx, sessionID)
	if err != nil {
		return err
	}
	if sess.ClaudeSessionID != "" {
		return a.launch(sess, ResumePrompt, sess.ClaudeSessionID)
	}
	msgs, err := a.Store.Messages(ctx, sessionID, 0)
	if err != nil {
		return err
	}
	for _, m := range msgs {
		if m.Role == "user" {
			return a.launch(sess, m.Content, "")
		}
	}
	return ErrNoResume
}

// Cancel stops a running turn. It is a no-op for sessions that are not running.
func (a *App) Cancel(sessionID int64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if cancel, ok := a.running[sessionID]; ok {
		cancel()
	}
}

func (a *App) launch(sess store.Session, prompt, resumeID string) error {
	ctx, cancel := context.WithCancel(context.Background())
	a.mu.Lock()
	if _, busy := a.running[sess.ID]; busy {
		a.mu.Unlock()
		cancel()
		return ErrBusy
	}
	a.running[sess.ID] = cancel
	a.wg.Add(1)
	a.mu.Unlock()

	// The lane the task is in now; moveWhenDone only acts if it is still there.
	var startLane int64
	if sess.TaskID != 0 {
		if t, err := a.Store.Task(ctx, sess.TaskID); err == nil {
			startLane = t.LaneID
		}
	}
	// Record status and prompt before returning so the transcript is ordered.
	if err := a.Store.SetSessionStatus(ctx, sess.ID, store.StatusRunning, ""); err != nil {
		log.Printf("session %d: %v", sess.ID, err)
	}
	if _, err := a.Store.AddMessage(ctx, store.Message{SessionID: sess.ID, Role: "user", Kind: "text", Content: prompt}); err != nil {
		log.Printf("session %d: %v", sess.ID, err)
	}

	go func() {
		defer a.wg.Done()
		defer func() {
			a.mu.Lock()
			delete(a.running, sess.ID)
			a.mu.Unlock()
			cancel()
		}()
		bg := context.Background()
		req := agent.Request{Prompt: prompt, Workdir: sess.Workdir, Mode: sess.Mode, Model: sess.Model, ResumeID: resumeID}
		failed := false
		err := a.Runner.Run(ctx, req, func(ev agent.Event) {
			if ev.ClaudeSessionID != "" && ev.ClaudeSessionID != sess.ClaudeSessionID {
				sess.ClaudeSessionID = ev.ClaudeSessionID
				a.Store.SetClaudeSessionID(bg, sess.ID, ev.ClaudeSessionID)
			}
			if ev.CostUSD > 0 {
				a.Store.AddSessionCost(bg, sess.ID, ev.CostUSD)
			}
			if ev.Kind == "error" {
				failed = true
			}
			if ev.Kind == "rate_limit" {
				a.recordLimits(sess.ID, ev.Limits)
				return
			}
			if ev.Kind == "init" {
				return
			}
			a.Store.AddMessage(bg, store.Message{SessionID: sess.ID, Role: ev.Role, Kind: ev.Kind, Content: ev.Content})
		})
		status, msg := store.StatusIdle, ""
		switch {
		case ctx.Err() != nil:
			status = store.StatusCancelled
			a.mu.Lock()
			if a.stopping {
				status = store.StatusInterrupted
			}
			a.mu.Unlock()
		case err != nil:
			status, msg = store.StatusError, err.Error()
			a.Store.AddMessage(bg, store.Message{SessionID: sess.ID, Role: "system", Kind: "error", Content: msg})
		case failed:
			status = store.StatusError
		}
		a.Store.SetSessionStatus(bg, sess.ID, status, msg)
		if status == store.StatusIdle {
			a.moveWhenDone(bg, sess.ID, startLane)
		}
	}()
	return nil
}

// recordLimits keeps the usage windows a session's turn reported. A window
// not reported keeps its previous value.
func (a *App) recordLimits(sessionID int64, l *agent.RateLimits) {
	a.mu.Lock()
	defer a.mu.Unlock()
	cur := a.limits[sessionID]
	if l.FiveHour != nil {
		cur.FiveHour = l.FiveHour
	}
	if l.SevenDay != nil {
		cur.SevenDay = l.SevenDay
	}
	a.limits[sessionID] = cur
}

// Limits returns the usage windows last reported by the session's turns since
// this process started. They are live data and never stored.
func (a *App) Limits(sessionID int64) agent.RateLimits {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.limits[sessionID]
}

// moveWhenDone moves the session's task to its done lane after a successful
// turn, firing that lane's transition rules. It does nothing if the task left
// startLane during the turn: someone moved it by hand, and that wins.
func (a *App) moveWhenDone(ctx context.Context, sessionID, startLane int64) {
	sess, err := a.Store.Session(ctx, sessionID) // the done lane may have changed mid-turn
	if err != nil || sess.TaskID == 0 || sess.DoneLaneID == nil {
		return
	}
	task, err := a.Store.Task(ctx, sess.TaskID)
	if err != nil || task.LaneID != startLane || task.LaneID == *sess.DoneLaneID {
		return
	}
	taskID := task.ID
	if _, err := a.MoveTask(ctx, taskID, *sess.DoneLaneID, -1); err != nil {
		a.Store.Notify(ctx, &taskID, fmt.Sprintf("Session %q finished but the task could not be moved: %v", sess.Title, err))
		return
	}
	lane, _ := a.Store.Lane(ctx, *sess.DoneLaneID)
	a.Store.Notify(ctx, &taskID, fmt.Sprintf("Agent finished %q — moved to %s", sess.Title, lane.Name))
}

// MoveTask moves a task on the board and fires matching transition rules.
// It returns the sessions started by rules.
func (a *App) MoveTask(ctx context.Context, taskID, laneID int64, index int) ([]store.Session, error) {
	from, err := a.Store.MoveTask(ctx, taskID, laneID, index)
	if err != nil || from == laneID {
		return nil, err
	}
	rules, err := a.Store.MatchingRules(ctx, from, laneID)
	if err != nil {
		return nil, err
	}
	task, err := a.Store.Task(ctx, taskID)
	if err != nil {
		return nil, err
	}
	var started []store.Session
	for _, r := range rules {
		prompt, err := RenderPrompt(r.Prompt, task)
		if err != nil {
			a.Store.Notify(ctx, &taskID, fmt.Sprintf("Rule %q: %v", r.Name, err))
			continue
		}
		sess, err := a.StartSession(ctx, StartOpts{
			TaskID: taskID, Prompt: prompt, Title: r.Name, Mode: r.Mode, Workdir: r.Workdir, Trigger: "rule:" + r.Name,
			DoneLaneID: r.DoneLaneID,
		})
		if err != nil {
			a.Store.Notify(ctx, &taskID, fmt.Sprintf("Rule %q: %v", r.Name, err))
			continue
		}
		started = append(started, sess)
	}
	return started, nil
}

// SaveSchedule validates the cron expression and computes the next run.
func (a *App) SaveSchedule(ctx context.Context, x store.Schedule) (store.Schedule, error) {
	if x.Action != store.ActionAgent && x.Action != store.ActionRemind {
		return x, ErrBadAction
	}
	spec, err := cron.Parse(x.Cron)
	if err != nil {
		return x, fmt.Errorf("%w: %v", ErrBadCron, err)
	}
	x.NextRunAt = nil
	if next := spec.Next(a.Now()); !next.IsZero() {
		x.NextRunAt = &next
	}
	return a.Store.SaveSchedule(ctx, x)
}

// RunDueSchedules fires every schedule due at now. Missed runs collapse into one.
func (a *App) RunDueSchedules(ctx context.Context) error {
	now := a.Now()
	due, err := a.Store.DueSchedules(ctx, now)
	if err != nil {
		return err
	}
	for _, x := range due {
		var next *time.Time
		if spec, err := cron.Parse(x.Cron); err == nil {
			if n := spec.Next(now); !n.IsZero() {
				next = &n
			}
		}
		if err := a.Store.MarkScheduleRun(ctx, x.ID, now, next); err != nil {
			return err
		}
		task, err := a.Store.Task(ctx, x.TaskID)
		if err != nil {
			continue
		}
		taskID := x.TaskID
		switch x.Action {
		case store.ActionRemind:
			msg := x.Prompt
			if msg == "" {
				msg = "Reminder: " + task.Title
			}
			a.Store.Notify(ctx, &taskID, msg)
		case store.ActionAgent:
			prompt, err := RenderPrompt(x.Prompt, task)
			if err == nil {
				_, err = a.StartSession(ctx, StartOpts{TaskID: taskID, Prompt: prompt, Mode: x.Mode,
					Trigger: fmt.Sprintf("schedule:%d", x.ID), DoneLaneID: x.DoneLaneID})
			}
			if err != nil {
				a.Store.Notify(ctx, &taskID, fmt.Sprintf("Scheduled session failed: %v", err))
			}
		}
	}
	return nil
}

// RunDueReminders notifies once for every task whose due time has been reached.
func (a *App) RunDueReminders(ctx context.Context) error {
	now := a.Now()
	due, err := a.Store.DueTasks(ctx, now)
	if err != nil {
		return err
	}
	for _, t := range due {
		if err := a.Store.MarkReminded(ctx, t.ID, now); err != nil {
			return err
		}
		taskID := t.ID
		if err := a.Store.Notify(ctx, &taskID, "Due: "+t.Title); err != nil {
			return err
		}
	}
	return nil
}

// RunAutoArchive archives the tasks that have sat in the archive lane longer
// than the configured time. It does nothing while auto-archive is off.
func (a *App) RunAutoArchive(ctx context.Context) error {
	set, err := a.Store.Settings(ctx)
	if err != nil || set.ArchiveLaneID == nil || set.ArchiveAfterMinutes <= 0 {
		return err
	}
	cutoff := a.Now().Add(-time.Duration(set.ArchiveAfterMinutes) * time.Minute)
	_, err = a.Store.ArchiveStale(ctx, *set.ArchiveLaneID, cutoff)
	return err
}

// RunScheduler checks schedules, due reminders and auto-archive every interval until ctx is done.
func (a *App) RunScheduler(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		if err := a.RunDueSchedules(ctx); err != nil {
			log.Printf("scheduler: %v", err)
		}
		if err := a.RunDueReminders(ctx); err != nil {
			log.Printf("reminders: %v", err)
		}
		if err := a.RunAutoArchive(ctx); err != nil {
			log.Printf("archive: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// PromptData is what prompt templates can reference, e.g.
// "{{.Task.Title}}", "{{.Fields.pull_request}}" or "{{.Context}}".
type PromptData struct {
	Task    store.Task
	Fields  map[string]string
	Context string
}

func RenderPrompt(tmpl string, task store.Task) (string, error) {
	t, err := template.New("prompt").Option("missingkey=zero").Parse(tmpl)
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	err = t.Execute(&buf, PromptData{Task: task, Fields: task.Fields, Context: TaskContext(task)})
	return strings.TrimSpace(buf.String()), err
}

// TaskContext renders the task as a plain-text brief for an agent.
func TaskContext(t store.Task) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Task #%d: %s\n", t.ID, t.Title)
	if d := strings.TrimSpace(t.Description); d != "" {
		fmt.Fprintf(&b, "\n%s\n", d)
	}
	if len(t.Fields) > 0 {
		keys := make([]string, 0, len(t.Fields))
		for k := range t.Fields {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		b.WriteString("\nLinks:\n")
		for _, k := range keys {
			fmt.Fprintf(&b, "- %s: %s\n", k, t.Fields[k])
		}
	}
	return strings.TrimSpace(b.String())
}

func firstLine(s string, max int) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if r := []rune(s); len(r) > max {
		s = string(r[:max]) + "…"
	}
	return s
}
