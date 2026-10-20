// Package app holds the behaviour that ties the store to agents:
// starting/continuing sessions, kanban transition rules and schedules.
package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"regexp"
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
	ErrStopping  = errors.New("the app is shutting down")
	ErrNoSession = errors.New("the task has no agent session to run the skill in")
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
	// summarizing holds the sessions whose summary is being written (see Summarize).
	summarizing map[int64]context.CancelFunc
	// memorizing holds the sessions whose task's memories are being updated
	// (see UpdateMemories); memorizeAgain those to update once more after.
	memorizing    map[int64]context.CancelFunc
	memorizeAgain map[int64]bool
	memoryLocks   map[int64]*sync.Mutex        // one update of a memory at a time
	filling       map[int64]context.CancelFunc // memories being written from a task's sessions (see FillMemory)

	Trello       TrelloClient // nil: Trello sync off
	trelloMu     sync.Mutex   // one sync at a time
	trelloKick   chan struct{}
	trelloStatus map[int64]TrelloStatus // by board id
}

func New(s *store.Store, r agent.Runner, workdir, mode string) *App {
	return &App{Store: s, Runner: r, DefaultWorkdir: workdir, DefaultMode: mode, Now: time.Now,
		running: map[int64]context.CancelFunc{}, limits: map[int64]agent.RateLimits{}, summarizing: map[int64]context.CancelFunc{},
		memorizing: map[int64]context.CancelFunc{}, memorizeAgain: map[int64]bool{}, memoryLocks: map[int64]*sync.Mutex{},
		filling: map[int64]context.CancelFunc{}, trelloKick: make(chan struct{}, 1)}
}

// Wait blocks until all agent turns started so far have finished.
func (a *App) Wait() { a.wg.Wait() }

// Shutdown cancels running turns, summaries and memory updates and waits for them. The turns
// are marked interrupted so they can be resumed after the next start.
func (a *App) Shutdown() {
	a.mu.Lock()
	a.stopping = true
	for _, cancel := range a.running {
		cancel()
	}
	for _, cancel := range a.summarizing {
		cancel()
	}
	for _, cancel := range a.memorizing {
		cancel()
	}
	for _, cancel := range a.filling {
		cancel()
	}
	a.mu.Unlock()
	a.wg.Wait()
}

type StartOpts struct {
	TaskID int64 // 0 starts a standalone session
	// ProjectID gives a standalone session its project's working directory;
	// a task's session uses the task's project.
	ProjectID int64
	Prompt    string
	Title     string
	Mode      string
	Model     string
	Workdir   string
	Trigger   string // manual | rule:<name> | schedule:<id>
	// DoneLaneID moves the task to this lane when a turn finishes successfully
	// (see moveWhenDone). Nil leaves the task where it is.
	DoneLaneID *int64
	ParentID   *int64 // the session this one continues (see Handoff)
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
	if o.TaskID != 0 {
		o.ProjectID = task.ProjectID
	}
	if o.Workdir == "" && o.ProjectID != 0 {
		p, err := a.Store.Project(ctx, o.ProjectID)
		if err != nil {
			return store.Session{}, err
		}
		o.Workdir = p.Workdir
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
		TaskID: o.TaskID, ProjectID: o.ProjectID, Title: o.Title, Mode: o.Mode, Model: o.Model, Workdir: o.Workdir, Trigger: o.Trigger,
		DoneLaneID: o.DoneLaneID, ParentID: o.ParentID,
	})
	if err != nil {
		return sess, err
	}
	return sess, a.launch(sess, o.Prompt, "", nil)
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
	a.answered(ctx, sess)
	return a.launch(sess, text, sess.ClaudeSessionID, nil)
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
	a.answered(ctx, sess)
	if sess.ClaudeSessionID != "" {
		return a.launch(sess, ResumePrompt, sess.ClaudeSessionID, nil)
	}
	msgs, err := a.Store.Messages(ctx, sessionID, 0)
	if err != nil {
		return err
	}
	for _, m := range msgs {
		if m.Role == "user" {
			return a.launch(sess, m.Content, "", nil)
		}
	}
	return ErrNoResume
}

// HandoffPrompt asks a session's agent to brief the agent that takes over from it.
const HandoffPrompt = `Your context is getting full, so this work continues in a new agent session that starts with an empty context. ` +
	`Write the handoff brief for that agent: it is the only thing it will know about this session. Make it self-contained and include:
- the goal and what "done" looks like;
- what has been done so far and the current state (branch, files changed, commands that matter);
- decisions made and why, and approaches that were tried and failed;
- open questions, known problems and anything the user asked for that is still pending;
- the exact next steps.
Reply with the brief only; do not change anything now.`

// HandoffIntro opens the first prompt of the session that takes over.
const HandoffIntro = "You are taking over from a previous agent session whose context filled up. Its handoff brief follows."

// Handoff continues an idle session in a fresh one: the session's agent writes
// a brief of its work, and a new session (same task, settings and done lane)
// starts from that brief. note, if given, is added as the user's instruction
// for the new session. The brief is written in the background; the new
// session appears once it is done, or a notification tells why not.
func (a *App) Handoff(ctx context.Context, sessionID int64, note string) error {
	sess, err := a.Store.Session(ctx, sessionID)
	if err != nil {
		return err
	}
	if sess.ClaudeSessionID == "" {
		return ErrNoResume
	}
	a.answered(ctx, sess)
	return a.launch(sess, HandoffPrompt, sess.ClaudeSessionID, func(bg context.Context, status string) {
		var taskID *int64
		if sess.TaskID != 0 {
			taskID = &sess.TaskID
		}
		var brief string
		err := fmt.Errorf("the brief turn ended %s", status)
		if status == store.StatusIdle {
			brief, err = a.lastReply(bg, sess.ID)
		}
		if err == nil {
			_, err = a.StartSession(bg, StartOpts{
				TaskID: sess.TaskID, ProjectID: sess.ProjectID, Prompt: a.handoffPrompt(bg, sess, brief, note), Title: sess.Title,
				Mode: sess.Mode, Model: sess.Model, Workdir: sess.Workdir, Trigger: "handoff",
				DoneLaneID: sess.DoneLaneID, ParentID: &sess.ID,
			})
		}
		if err != nil {
			a.Store.Notify(bg, taskID, fmt.Sprintf("Could not hand off session %q: %v", sess.Title, err))
		}
	})
}

// handoffPrompt is the new session's first prompt: the task as it is now with
// its memories, the brief, then the user's note or a nudge to go on.
func (a *App) handoffPrompt(ctx context.Context, sess store.Session, brief, note string) string {
	parts := []string{HandoffIntro}
	if sess.TaskID != 0 {
		if t, err := a.Store.Task(ctx, sess.TaskID); err == nil {
			parts = append(parts, TaskContext(t))
			if mem := a.memoryBrief(ctx, t); mem != "" {
				parts = append(parts, mem)
			}
		}
	}
	parts = append(parts, strings.TrimSpace(brief))
	if note = strings.TrimSpace(note); note != "" {
		parts = append(parts, note)
	} else {
		parts = append(parts, "Continue with the next steps.")
	}
	return strings.Join(parts, "\n\n---\n\n")
}

// lastReply is the agent's final answer in the session's latest turn.
func (a *App) lastReply(ctx context.Context, sessionID int64) (string, error) {
	msgs, err := a.Store.Messages(ctx, sessionID, 0)
	if err != nil {
		return "", err
	}
	text := ""
	for i := len(msgs) - 1; i >= 0 && msgs[i].Role != "user"; i-- {
		m := msgs[i]
		if m.Kind == "result" && strings.TrimSpace(m.Content) != "" {
			return m.Content, nil
		}
		if text == "" && m.Role == "assistant" && m.Kind == "text" && strings.TrimSpace(m.Content) != "" {
			text = m.Content
		}
	}
	if text == "" {
		return "", errors.New("the agent wrote no brief")
	}
	return text, nil
}

// Cancel stops a running turn. It is a no-op for sessions that are not running.
func (a *App) Cancel(sessionID int64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if cancel, ok := a.running[sessionID]; ok {
		cancel()
	}
}

// launch runs one turn in the background. done, if set, runs after the turn
// with its final status instead of the usual move to the done lane.
func (a *App) launch(sess store.Session, prompt, resumeID string, done func(ctx context.Context, status string)) error {
	ctx, cancel := context.WithCancel(context.Background())
	a.mu.Lock()
	if _, busy := a.running[sess.ID]; busy {
		a.mu.Unlock()
		cancel()
		return ErrBusy
	}
	if a.stopping {
		a.mu.Unlock()
		cancel()
		return ErrStopping
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
		bg := context.Background()
		req := agent.Request{Prompt: prompt, Workdir: sess.Workdir, Mode: sess.Mode, Model: sess.Model, ResumeID: resumeID}
		failed := false
		var wait waitSigns
		var model string // as the CLI names it
		err := a.Runner.Run(ctx, req, func(ev agent.Event) {
			wait.see(ev)
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
			if ev.Kind == "usage" {
				a.Store.SetSessionContext(bg, sess.ID, ev.ContextTokens, 0)
				return
			}
			if w := contextWindow(ev.ContextWindows, model); w > 0 {
				a.Store.SetSessionContext(bg, sess.ID, 0, w)
			}
			if ev.Kind == "init" {
				model = ev.Model
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
		if reason := wait.reason(); status == store.StatusIdle && reason != "" {
			a.needsInput(bg, sess, reason)
		}
		// Free the session first: what follows may start its next turn.
		a.mu.Lock()
		delete(a.running, sess.ID)
		a.mu.Unlock()
		cancel()
		switch {
		case done != nil:
			done(bg, status)
		case status == store.StatusIdle:
			a.afterTurn(bg, sess.ID, startLane)
		}
	}()
	return nil
}

// contextWindow picks the session's model's window from those a turn used,
// which include its subagents' models. A turn on one model can only be on that.
func contextWindow(windows map[string]int, model string) int {
	if w, ok := windows[model]; ok {
		return w
	}
	if len(windows) == 1 {
		for _, w := range windows {
			return w
		}
	}
	return 0
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

// afterTurn follows a successful turn: the skills whose automations match the
// task run in the session, one turn each, then the task moves to its done lane
// and, if other tasks build on it, the session's summary is written; the
// task's memories are updated from the session.
// A skill turn that does not finish successfully ends the chain there.
func (a *App) afterTurn(ctx context.Context, sessionID, startLane int64) {
	sess, err := a.Store.Session(ctx, sessionID)
	if err != nil {
		return
	}
	var skills []store.Skill
	if sess.TaskID != 0 {
		if task, err := a.Store.Task(ctx, sess.TaskID); err == nil {
			if skills, err = a.Store.MatchingSkills(ctx, task.Kind, task.ProjectID, task.LaneID); err != nil {
				log.Printf("session %d: skills: %v", sessionID, err)
			}
		}
	}
	a.runSkills(ctx, sess, skills, func(ctx context.Context) {
		a.moveWhenDone(ctx, sessionID, startLane)
		a.summarizeForUsers(ctx, sess)
		a.memorizeAfterTurn(ctx, sess)
	})
}

func (a *App) runSkills(ctx context.Context, sess store.Session, skills []store.Skill, then func(context.Context)) {
	if len(skills) == 0 {
		then(ctx)
		return
	}
	sk := skills[0]
	err := a.runSkill(ctx, sess, sk, func(ctx context.Context, status string) {
		if status != store.StatusIdle {
			a.notifySession(ctx, sess, fmt.Sprintf("Skill %q ended %s in session %q", sk.Name, status, sess.Title))
			return
		}
		a.runSkills(ctx, sess, skills[1:], then)
	})
	if err != nil {
		a.notifySession(ctx, sess, fmt.Sprintf("Skill %q could not run in session %q: %v", sk.Name, sess.Title, err))
	}
}

// SkillIntro opens the prompt that applies a skill; ArtifactNote follows it
// for skills whose reply is kept.
const (
	SkillIntro   = "Apply the skill %q to the work done in this session so far. Its instructions follow."
	ArtifactNote = "Your final reply is saved as an artifact on the task: reply with the artifact itself, nothing before or after it."
)

// RunSkill applies a skill to an idle session by hand, as its next turn.
func (a *App) RunSkill(ctx context.Context, sessionID, skillID int64) error {
	sess, err := a.Store.Session(ctx, sessionID)
	if err != nil {
		return err
	}
	sk, err := a.Store.Skill(ctx, skillID)
	if err != nil {
		return err
	}
	a.answered(ctx, sess)
	return a.runSkill(ctx, sess, sk, nil)
}

// runSkill continues the session with the skill's prompt. Once the turn ends,
// a successful reply is kept as an artifact if the skill asks for it, then
// done, if set, gets the turn's status.
func (a *App) runSkill(ctx context.Context, sess store.Session, sk store.Skill, done func(context.Context, string)) error {
	if sess.ClaudeSessionID == "" {
		return ErrNoResume
	}
	var task store.Task
	brief := ""
	if sess.TaskID != 0 {
		var err error
		if task, err = a.Store.Task(ctx, sess.TaskID); err != nil {
			return err
		}
		brief = a.TaskBrief(ctx, task)
	}
	body, err := renderPrompt(sk.Body, task, brief)
	if err != nil {
		return fmt.Errorf("skill template: %w", err)
	}
	parts := []string{fmt.Sprintf(SkillIntro, sk.Name)}
	if sk.Artifact && sess.TaskID != 0 {
		parts = append(parts, ArtifactNote)
	}
	prompt := strings.Join(append(parts, body), "\n\n")
	if sk.Model != "" {
		sess.Model = sk.Model // this turn only: the session keeps its model
	}
	return a.launch(sess, prompt, sess.ClaudeSessionID, func(ctx context.Context, status string) {
		if status == store.StatusIdle && sk.Artifact && sess.TaskID != 0 {
			if reply, err := a.lastReply(ctx, sess.ID); err != nil {
				a.notifySession(ctx, sess, fmt.Sprintf("Skill %q wrote no artifact in session %q", sk.Name, sess.Title))
			} else if _, err := a.Store.AddArtifact(ctx, store.Artifact{TaskID: sess.TaskID, SessionID: &sess.ID, Skill: sk.Name,
				Content: strings.TrimSpace(reply)}); err != nil {
				log.Printf("session %d: artifact: %v", sess.ID, err)
			}
		}
		if done != nil {
			done(ctx, status)
		}
	})
}

// waitSigns collects what a turn shows of its agent waiting for the user.
// Print mode cannot stop to ask, so the turn ends instead.
type waitSigns struct {
	denied []string // tools refused for want of the user's permission
	asked  bool     // the agent used its tool for asking the user
	plan   bool     // the agent proposed a plan to approve
	reply  string   // the final reply
}

func (w *waitSigns) see(ev agent.Event) {
	switch ev.Kind {
	case "tool_use":
		name, _, _ := strings.Cut(ev.Content, " ")
		w.asked = w.asked || name == "AskUserQuestion"
		w.plan = w.plan || name == "ExitPlanMode"
	case "result":
		w.denied, w.reply = ev.Denied, ev.Content
	}
}

// reason says why the agent waits for the user, empty if it does not seem to.
func (w *waitSigns) reason() string {
	switch {
	case len(w.denied) > 0:
		seen := map[string]bool{}
		var tools []string
		for _, t := range w.denied {
			if !seen[t] {
				seen[t] = true
				tools = append(tools, t)
			}
		}
		return "needs permission for " + strings.Join(tools, ", ")
	case w.plan:
		return "waits for its plan to be approved"
	case w.asked || endsWithQuestion(w.reply):
		return "asked a question"
	}
	return ""
}

// endsWithQuestion reports whether the reply's last line is a question,
// markdown emphasis and closing brackets aside.
func endsWithQuestion(reply string) bool {
	lines := strings.Split(strings.TrimSpace(reply), "\n")
	last := strings.TrimRight(lines[len(lines)-1], " \t*_`)]\"'")
	return strings.HasSuffix(last, "?")
}

// needsInput flags the session as waiting for the user and notifies them.
func (a *App) needsInput(ctx context.Context, sess store.Session, reason string) {
	if err := a.Store.SetSessionNeedsInput(ctx, sess.ID, reason); err != nil {
		log.Printf("session %d: %v", sess.ID, err)
		return
	}
	a.notifySession(ctx, sess, fmt.Sprintf("Agent needs your input in %q: it %s", sess.Title, reason))
}

// answered clears the session's wait for the user, who is continuing it.
func (a *App) answered(ctx context.Context, sess store.Session) {
	if sess.NeedsInput != "" {
		a.Store.SetSessionNeedsInput(ctx, sess.ID, "")
	}
}

// DismissNeedsInput clears the session's wait for the user without continuing
// it, and marks how its last turn ended as seen.
func (a *App) DismissNeedsInput(ctx context.Context, sessionID int64) error {
	return a.Store.DismissSession(ctx, sessionID)
}

// notifySession notifies about a session, on its task if it has one.
func (a *App) notifySession(ctx context.Context, sess store.Session, msg string) {
	var taskID *int64
	if sess.TaskID != 0 {
		taskID = &sess.TaskID
	}
	a.Store.NotifySession(ctx, taskID, &sess.ID, msg)
}

// moveWhenDone moves the session's task to its done lane after a successful
// turn, firing that lane's transition rules. It does nothing if the task left
// startLane during the turn: someone moved it by hand, and that wins.
func (a *App) moveWhenDone(ctx context.Context, sessionID, startLane int64) {
	sess, err := a.Store.Session(ctx, sessionID) // the done lane may have changed mid-turn
	if err != nil || sess.TaskID == 0 || sess.DoneLaneID == nil {
		return
	}
	a.moveDone(ctx, sess, startLane, *sess.DoneLaneID)
}

// moveDone moves the session's task from startLane to doneLane, unless it
// left startLane meanwhile.
func (a *App) moveDone(ctx context.Context, sess store.Session, startLane, doneLane int64) {
	task, err := a.Store.Task(ctx, sess.TaskID)
	if err != nil || task.LaneID != startLane || task.LaneID == doneLane {
		return
	}
	taskID := task.ID
	if _, err := a.MoveTask(ctx, taskID, doneLane, -1); err != nil {
		a.Store.Notify(ctx, &taskID, fmt.Sprintf("Session %q finished but the task could not be moved: %v", sess.Title, err))
		return
	}
	lane, _ := a.Store.Lane(ctx, doneLane)
	a.Store.Notify(ctx, &taskID, fmt.Sprintf("Agent finished %q — moved to %s", sess.Title, lane.Name))
}

// MoveTask moves a task on the board and fires matching transition rules.
// It returns the sessions rules started, or continue with a skill.
func (a *App) MoveTask(ctx context.Context, taskID, laneID int64, index int) ([]store.Session, error) {
	from, err := a.Store.MoveTask(ctx, taskID, laneID, index)
	if err != nil || from == laneID {
		return nil, err
	}
	a.KickTrello() // the task's card may need archiving, or bringing back
	task, err := a.Store.Task(ctx, taskID)
	if err != nil {
		return nil, err
	}
	rules, err := a.Store.MatchingRules(ctx, task.ProjectID, from, laneID)
	if err != nil {
		return nil, err
	}
	// Skills continue the task's latest session as it was before the move,
	// not one started by another rule here.
	latest, err := a.latestSession(ctx, taskID)
	if err != nil {
		return nil, err
	}
	var started []store.Session
	for _, r := range rules {
		if r.SkillID != nil {
			if err := a.ruleSkill(ctx, r, latest); err != nil {
				a.Store.Notify(ctx, &taskID, fmt.Sprintf("Rule %q: %v", r.Name, err))
				continue
			}
			started = append(started, *latest)
			continue
		}
		prompt, err := renderPrompt(r.Prompt, task, a.TaskBrief(ctx, task))
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

// latestSession returns the task's most recently active session that can be
// continued, or nil.
func (a *App) latestSession(ctx context.Context, taskID int64) (*store.Session, error) {
	sessions, err := a.Store.Sessions(ctx, taskID, "")
	if err != nil {
		return nil, err
	}
	for _, s := range sessions {
		if s.ClaudeSessionID != "" {
			return &s, nil
		}
	}
	return nil, nil
}

// ruleSkill runs the rule's skill in sess. Once it finishes successfully the
// task moves to the rule's done lane, unless it left the rule's lane meanwhile.
func (a *App) ruleSkill(ctx context.Context, r store.Rule, sess *store.Session) error {
	if sess == nil {
		return ErrNoSession
	}
	sk, err := a.Store.Skill(ctx, *r.SkillID)
	if err != nil {
		return fmt.Errorf("skill: %w", err)
	}
	s := *sess
	return a.runSkill(ctx, s, sk, func(ctx context.Context, status string) {
		if status != store.StatusIdle {
			a.notifySession(ctx, s, fmt.Sprintf("Skill %q ended %s in session %q", sk.Name, status, s.Title))
			return
		}
		if r.DoneLaneID != nil {
			a.moveDone(ctx, s, r.ToLaneID, *r.DoneLaneID)
		}
	})
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
			prompt, err := renderPrompt(x.Prompt, task, a.TaskBrief(ctx, task))
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
// Context is the task brief, with the results of the tasks it builds on.
type PromptData struct {
	Task    store.Task
	Fields  map[string]string
	Context string
}

// RenderPrompt renders a prompt template for the task. Its {{.Context}} is the
// task alone; agents get TaskBrief instead (see renderPrompt).
func RenderPrompt(tmpl string, task store.Task) (string, error) {
	return renderPrompt(tmpl, task, TaskContext(task))
}

func renderPrompt(tmpl string, task store.Task, context string) (string, error) {
	t, err := template.New("prompt").Option("missingkey=zero").Parse(tmpl)
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	err = t.Execute(&buf, PromptData{Task: task, Fields: task.Fields, Context: context})
	return strings.TrimSpace(buf.String()), err
}

// Limits on what TaskBrief takes from each task the brief's task builds on.
const (
	sharedSessions = 5    // the latest sessions with a result
	sharedRunes    = 8000 // per result; longer ones are cut
)

// SharedIntro opens the part of a task brief that carries earlier tasks' results.
const SharedIntro = "This task builds on earlier tasks. What their agents did, oldest session first: " +
	"a summary of each session, or its final reply where the session has no up-to-date summary."

// TaskBrief is TaskContext followed by the task's memories and what the agents
// of the tasks it builds on (ContextFrom) did: per task, each of its latest sessions' summary, or its
// final reply if the summary is missing or stale. Such sessions get a fresh
// summary in the background, for the next brief. Sessions that were handed
// off are left out; the session that took over carries on.
func (a *App) TaskBrief(ctx context.Context, t store.Task) string {
	brief := TaskContext(t)
	if links := LinksBrief(t); links != "" {
		brief += "\n\n---\n\n" + links
	}
	if mem := a.memoryBrief(ctx, t); mem != "" {
		brief += "\n\n---\n\n" + mem
	}
	if len(t.ContextFrom) == 0 {
		return brief
	}
	var sources []string
	for _, id := range t.ContextFrom {
		src, err := a.Store.Task(ctx, id)
		if err != nil {
			continue
		}
		sources = append(sources, a.taskResults(ctx, src))
	}
	return brief + "\n\n---\n\n" + SharedIntro + "\n\n" + strings.Join(sources, "\n\n---\n\n")
}

// taskResults renders one source task for TaskBrief.
func (a *App) taskResults(ctx context.Context, t store.Task) string {
	var b strings.Builder
	fmt.Fprintf(&b, "## Task #%d: %s\n", t.ID, t.Title)
	if d := strings.TrimSpace(t.Description); d != "" {
		fmt.Fprintf(&b, "\n%s\n", truncate(d, sharedRunes))
	}
	sessions, err := a.Store.Sessions(ctx, t.ID, "") // newest first
	if err != nil {
		return b.String()
	}
	var results []string
	for _, x := range sessions {
		if len(results) == sharedSessions {
			break
		}
		if x.ChildID != nil || x.Status == store.StatusRunning {
			continue
		}
		if x.Summary != "" && !x.SummaryStale {
			results = append(results, fmt.Sprintf("### Summary of session %d: %s (%s)\n\n%s",
				x.ID, x.Title, x.UpdatedAt.Format("2006-01-02"), truncate(x.Summary, sharedRunes)))
			continue
		}
		reply, err := a.lastReply(ctx, x.ID)
		if err != nil {
			continue
		}
		a.startSummary(x.ID, false)
		results = append(results, fmt.Sprintf("### Final reply of session %d: %s (%s)\n\n%s",
			x.ID, x.Title, x.UpdatedAt.Format("2006-01-02"), truncate(strings.TrimSpace(reply), sharedRunes)))
	}
	if len(results) == 0 {
		b.WriteString("\n(No agent results yet.)\n")
	}
	for i := len(results) - 1; i >= 0; i-- {
		fmt.Fprintf(&b, "\n%s\n", results[i])
	}
	return strings.TrimSpace(b.String())
}

func truncate(s string, max int) string {
	if r := []rune(s); len(r) > max {
		return string(r[:max]) + "\n… (cut; the full transcript is in the session)"
	}
	return s
}

// LinksIntro opens the part of a task brief that lists the task's links.
const LinksIntro = "The task links to the pages below. Before you start, read each one with your tools " +
	"(the Productive or Slack MCP tools for those apps, a web fetch for other pages) and take what they say " +
	"as part of the task. If a page can't be read, say so and go on without it."

var urlRe = regexp.MustCompile(`https?://[^\s<>"'()\[\]{}]+`)

// TaskURLs are the URLs in the task's description and links, in order, once each.
func TaskURLs(t store.Task) []string {
	text := t.Description
	keys := make([]string, 0, len(t.Fields))
	for k := range t.Fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		text += "\n" + t.Fields[k]
	}
	var urls []string
	seen := map[string]bool{}
	for _, u := range urlRe.FindAllString(text, -1) {
		u = strings.TrimRight(u, ".,;:!?*_`")
		if !seen[u] {
			seen[u] = true
			urls = append(urls, u)
		}
	}
	return urls
}

// LinksBrief asks the agent to read the task's linked pages; "" without links.
func LinksBrief(t store.Task) string {
	urls := TaskURLs(t)
	if len(urls) == 0 {
		return ""
	}
	return LinksIntro + "\n\n- " + strings.Join(urls, "\n- ")
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
