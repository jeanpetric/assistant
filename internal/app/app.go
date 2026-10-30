// Package app holds the behaviour that ties the store to agents:
// starting/continuing sessions, kanban transition rules and schedules.
package app

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"log"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"text/template"
	"time"

	"github.com/jeanpetric/assistant/internal/agent"
	"github.com/jeanpetric/assistant/internal/cron"
	"github.com/jeanpetric/assistant/internal/slack"
	"github.com/jeanpetric/assistant/internal/store"
)

var (
	ErrBusy      = errors.New("session is running")
	ErrNoResume  = errors.New("session has no agent session to resume")
	ErrBadAction = errors.New("action must be agent or remind")
	ErrBadCron   = errors.New("invalid cron expression")
	ErrStopping  = errors.New("the app is shutting down")
	// ErrBadReportTo: a new session's report_to names no session.
	ErrBadReportTo = errors.New("report_to is not a session")
	// ErrForeignSession: the session runs on another node, the only one that
	// can go on with it (see ForeignSessionError).
	ErrForeignSession = errors.New("session runs on another node")
)

// ForeignSessionError is ErrForeignSession naming the session's node.
type ForeignSessionError struct{ Node string }

func (e ForeignSessionError) Error() string { return "session runs on " + e.Node + "; open it there" }

func (e ForeignSessionError) Is(target error) bool { return target == ErrForeignSession }

// own refuses a session another node runs: its agent and conversation live there.
func own(sess store.Session) error {
	if !sess.Owned {
		return ForeignSessionError{Node: sess.Node()}
	}
	return nil
}

type App struct {
	Store          *store.Store
	Runner         agent.Runner
	DefaultWorkdir string
	DefaultMode    string
	DefaultAgent   string // for sessions that name none, rules' and schedules' too; empty is claude
	Now            func() time.Time

	mu       sync.Mutex
	running  map[int64]context.CancelFunc
	turns    map[int64]int64 // per session, how many turns this process started (see report)
	stopping bool            // set by Shutdown: cancelled turns end as interrupted, not cancelled
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

	Slack       SlackClient // nil: Slack off
	slackMu     sync.Mutex  // one poll at a time
	slackKick   chan struct{}
	slackStatus map[int64]SlackStatus // by task id
	slackMe     *slack.Identity       // the bot, once known
	slackURL    string                // the workspace's URL, once known; under mu

	// SyncRole is this node's part in sync: RoleOff, RoleHub or RoleClient.
	// A hub is only the shared database: it runs no schedules, reminders
	// or auto-archive.
	SyncRole string
	// SyncServer is the hub a client syncs with, SyncClient how that goes,
	// nil while the database is not in sync yet.
	SyncServer string
	SyncClient func(context.Context) (ClientStatus, error)
}

const (
	RoleOff    = "off"
	RoleHub    = "hub"
	RoleClient = "client"
)

// SyncStatus is this node's part in sync and, on a client, how its sync goes.
type SyncStatus struct {
	Role string `json:"role"`
	*ClientStatus
}

// ClientStatus is a sync client's syncclient.Status (that package's tests
// use this one's, so it is not imported).
type ClientStatus struct {
	Server   string     `json:"server"`
	Online   bool       `json:"online"`
	Pending  int        `json:"pending"`
	SyncedAt *time.Time `json:"synced_at,omitempty"`
	Error    string     `json:"error,omitempty"`
}

// errNotInSync: a client's database was neither seeded nor joined.
const errNotInSync = "this database is not in sync yet: run assistant sync seed (the first node) or assistant sync join (an empty database), then restart"

func (a *App) SyncStatus(ctx context.Context) (SyncStatus, error) {
	st := SyncStatus{Role: a.SyncRole}
	if st.Role == "" {
		st.Role = RoleOff
	}
	if st.Role != RoleClient {
		return st, nil
	}
	if a.SyncClient == nil {
		st.ClientStatus = &ClientStatus{Server: a.SyncServer, Error: errNotInSync}
		return st, nil
	}
	cs, err := a.SyncClient(ctx)
	st.ClientStatus = &cs
	return st, err
}

func New(s *store.Store, r agent.Runner, workdir, mode string) *App {
	return &App{Store: s, Runner: r, DefaultWorkdir: workdir, DefaultMode: mode, Now: time.Now,
		running: map[int64]context.CancelFunc{}, turns: map[int64]int64{}, limits: map[int64]agent.RateLimits{}, summarizing: map[int64]context.CancelFunc{},
		memorizing: map[int64]context.CancelFunc{}, memorizeAgain: map[int64]bool{}, memoryLocks: map[int64]*sync.Mutex{},
		filling: map[int64]context.CancelFunc{}, trelloKick: make(chan struct{}, 1),
		slackKick: make(chan struct{}, 1), slackStatus: map[int64]SlackStatus{}}
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
	Agent     string // one of agent.Agents; empty is claude
	Model     string
	Workdir   string
	Trigger   string // manual | rule:<name> | schedule:<id>
	// DoneLaneID moves the task to this lane when a turn finishes successfully
	// (see moveWhenDone). Nil leaves the task where it is.
	DoneLaneID *int64
	ParentID   *int64 // the session this one continues (see Handoff)
	// ReportTo is told how each of this session's turns ends (see report).
	ReportTo *int64
	// ContextFrom: the tasks StartSessionInNewTask's task builds on; the first
	// prompt starts with their results and the memories the task inherits.
	ContextFrom []int64
	Watched     bool // StartSessionInNewTask's task starts out watched
	// SkillID makes the first turn apply this skill to the task (see
	// skillPrompt); Prompt, optional then, goes with it as more context.
	SkillID *int64
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
	var skill *store.Skill
	if o.SkillID != nil {
		if o.TaskID == 0 {
			return store.Session{}, errors.New("a skill runs on a task")
		}
		sk, err := a.Store.Skill(ctx, *o.SkillID)
		if err != nil {
			return store.Session{}, fmt.Errorf("skill: %w", err)
		}
		if o.Prompt, err = a.skillPrompt(ctx, task, sk, true, o.Prompt); err != nil {
			return store.Session{}, err
		}
		if o.Title == "" {
			o.Title = sk.Name
		}
		skill = &sk
	}
	if strings.TrimSpace(o.Prompt) == "" {
		return store.Session{}, errors.New("prompt is required")
	}
	if o.ReportTo != nil {
		if _, err := a.Store.Session(ctx, *o.ReportTo); errors.Is(err, store.ErrNotFound) {
			return store.Session{}, ErrBadReportTo
		} else if err != nil {
			return store.Session{}, err
		}
	}
	if o.Mode == "" {
		o.Mode = a.DefaultMode
	}
	if o.Agent == "" {
		o.Agent = a.DefaultAgent
	}
	if o.Agent == "" {
		o.Agent = agent.Claude
	}
	if !slices.Contains(agent.Agents, o.Agent) {
		return store.Session{}, fmt.Errorf("unknown agent %q", o.Agent)
	}
	// Skills name claude's models.
	if skill != nil && o.Model == "" && o.Agent == agent.Claude {
		o.Model = skill.Model
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
		TaskID: o.TaskID, ProjectID: o.ProjectID, Title: o.Title, Mode: o.Mode, Agent: o.Agent, Model: o.Model, Workdir: o.Workdir, Trigger: o.Trigger,
		DoneLaneID: o.DoneLaneID, ParentID: o.ParentID, ReportTo: o.ReportTo,
	})
	if err != nil {
		return sess, err
	}
	if skill == nil {
		return sess, a.launch(sess, o.Prompt, "", nil)
	}
	// The turn ends as any first turn does, after the skill's artifact is kept.
	sk, startLane := *skill, task.LaneID
	return sess, a.launch(sess, o.Prompt, "", func(ctx context.Context, status string) {
		if status == store.StatusIdle {
			a.keepArtifact(ctx, sess, sk)
		}
		if (status == store.StatusIdle || status == store.StatusError) && a.sendQueued(ctx, sess.ID) {
			return
		}
		if status == store.StatusIdle {
			a.afterTurn(ctx, sess.ID, startLane)
		}
	})
}

// StartSessionInNewTask creates a task for the prompt in o.ProjectID, in the
// lane set for new sessions (the first lane if none), and starts the session on it, so no session is left without a task. The
// task's title is the prompt's first line; its description is the whole
// prompt unless that is the title already. No rule fires for a task created
// in a lane, so unless o sets one, the session takes the done lane of a rule
// into that lane: as if the task had been moved there.
func (a *App) StartSessionInNewTask(ctx context.Context, o StartOpts) (store.Session, error) {
	if strings.TrimSpace(o.Prompt) == "" {
		return store.Session{}, errors.New("prompt is required")
	}
	t := store.Task{Title: firstLine(o.Prompt, 80), ProjectID: o.ProjectID, ContextFrom: o.ContextFrom, Watched: o.Watched}
	set, err := a.Store.Settings(ctx)
	if err != nil {
		return store.Session{}, err
	}
	if set.SessionLaneID != nil {
		t.LaneID = *set.SessionLaneID
	}
	if desc := strings.TrimSpace(o.Prompt); desc != t.Title {
		t.Description = desc
	}
	t.Fields = a.WithLinkFields(ctx, t.Description, t.Fields)
	t, err = a.Store.CreateTask(ctx, t)
	if err != nil {
		return store.Session{}, err
	}
	o.TaskID = t.ID
	if o.DoneLaneID == nil {
		if o.DoneLaneID, err = a.Store.DoneLaneInto(ctx, t.Kind, t.ProjectID, t.LaneID); err != nil {
			_ = a.Store.DeleteTask(ctx, t.ID)
			return store.Session{}, err
		}
	}
	if shared := a.sharedBrief(ctx, t); shared != "" {
		o.Prompt = shared + "\n\n---\n\n" + o.Prompt
	}
	sess, err := a.StartSession(ctx, o)
	if err != nil && sess.ID == 0 {
		_ = a.Store.DeleteTask(ctx, t.ID)
	}
	return sess, err
}

// SendMessage continues an idle session with a follow-up prompt. While the
// session's agent works, the prompt is queued instead (queued is true): it
// starts the next turn once this one ends (see sendQueued). Either way it
// answers the questions the session asked on its task's thread.
func (a *App) SendMessage(ctx context.Context, sessionID int64, text string) (queued bool, err error) {
	return a.sendMessage(ctx, sessionID, text, true)
}

// sendMessage is SendMessage; answers says whether the message answers the
// session's questions on the task's thread. A report does not: it comes
// from another session, not the user.
func (a *App) sendMessage(ctx context.Context, sessionID int64, text string, answers bool) (queued bool, err error) {
	sess, err := a.Store.Session(ctx, sessionID)
	if err != nil {
		return false, err
	}
	if err := own(sess); err != nil {
		return false, err
	}
	// Queue under the lock: a turn frees the session under it before it
	// looks at the queue, so the message is either seen there or sent now.
	a.mu.Lock()
	if _, busy := a.running[sessionID]; busy {
		_, err := a.Store.QueueMessage(ctx, sessionID, text)
		a.mu.Unlock()
		if err == nil && answers {
			err = a.Store.AnswerSessionQuestions(ctx, sessionID)
		}
		return err == nil, err
	}
	a.mu.Unlock()
	if sess.ClaudeSessionID == "" {
		return false, ErrNoResume
	}
	if answers {
		if err := a.Store.AnswerSessionQuestions(ctx, sessionID); err != nil {
			return false, err
		}
		a.answered(ctx, sess)
	}
	return false, a.launch(sess, text, sess.ClaudeSessionID, nil)
}

func (a *App) hasQueued(ctx context.Context, sessionID int64) bool {
	sess, err := a.Store.Session(ctx, sessionID)
	return err == nil && len(sess.Queued) > 0
}

// sendQueued starts the session's next turn with the oldest message queued
// while it worked, and reports whether it did. A message stays queued if the
// session cannot take it now.
func (a *App) sendQueued(ctx context.Context, sessionID int64) bool {
	sess, err := a.Store.Session(ctx, sessionID)
	if err != nil || len(sess.Queued) == 0 || sess.ClaudeSessionID == "" {
		return false
	}
	q := sess.Queued[0]
	if err := a.launch(sess, q.Content, sess.ClaudeSessionID, nil); err != nil {
		return false
	}
	a.answered(ctx, sess)
	if err := a.Store.DeleteQueuedMessage(ctx, q.ID); err != nil {
		log.Printf("session %d: queued message %d: %v", sessionID, q.ID, err)
	}
	return true
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
	if err := own(sess); err != nil {
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
	if err := own(sess); err != nil {
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
				Mode: sess.Mode, Agent: sess.Agent, Model: sess.Model, Workdir: sess.Workdir, Trigger: "handoff",
				DoneLaneID: sess.DoneLaneID, ParentID: &sess.ID, ReportTo: sess.ReportTo,
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

// Cancel stops a running turn. It is a no-op for sessions that are not
// running here; another node's session is refused.
func (a *App) Cancel(ctx context.Context, sessionID int64) error {
	sess, err := a.Store.Session(ctx, sessionID)
	if err != nil {
		return err
	}
	if err := own(sess); err != nil {
		return err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if cancel, ok := a.running[sessionID]; ok {
		cancel()
	}
	return nil
}

// DeleteQueuedMessage drops a message queued for a session's next turn.
func (a *App) DeleteQueuedMessage(ctx context.Context, id int64) error {
	sessionID, err := a.Store.QueuedMessageSession(ctx, id)
	if err != nil {
		return err
	}
	sess, err := a.Store.Session(ctx, sessionID)
	if err != nil {
		return err
	}
	if err := own(sess); err != nil {
		return err
	}
	return a.Store.DeleteQueuedMessage(ctx, id)
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
	a.turns[sess.ID]++
	turn := a.turns[sess.ID]
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

	system, err := a.sessionPrompt(ctx, sess)
	if err != nil {
		log.Printf("session %d: session prompt: %v", sess.ID, err)
	}

	go func() {
		defer a.wg.Done()
		bg := context.Background()
		req := agent.Request{Agent: sess.Agent, Prompt: prompt, Workdir: sess.Workdir, Mode: sess.Mode, Model: sess.Model, ResumeID: resumeID,
			SystemPrompt: system}
		failed := false
		var wait waitSigns
		var prs openedPRs
		var model string // as the CLI names it
		err := a.Runner.Run(ctx, req, func(ev agent.Event) {
			wait.see(ev)
			prs.see(ev)
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
		a.recordPR(bg, sess.TaskID, prs)
		// A message the user queued meanwhile is their answer already.
		// A question on the task's thread flagged the session already.
		if reason := wait.reason(); status == store.StatusIdle && reason != "" && !a.hasQueued(bg, sess.ID) && !a.waitsForAnswer(bg, sess.ID) {
			a.needsInput(bg, sess, reason)
		}
		// Free the session first: what follows may start its next turn.
		a.mu.Lock()
		delete(a.running, sess.ID)
		a.mu.Unlock()
		cancel()
		// A message the user queued meanwhile goes next, before the skills and
		// moves that follow a turn: those wait for the turn it starts. A
		// stopped turn keeps the queue until a later one ends.
		ended := status == store.StatusIdle || status == store.StatusError
		if ended && done == nil && a.sendQueued(bg, sess.ID) {
			return
		}
		switch {
		case done != nil:
			done(bg, status)
		case status == store.StatusIdle:
			a.afterTurn(bg, sess.ID, startLane)
		}
		// done may have started another turn (the next skill); the queue then
		// waits for that one.
		if ended {
			a.sendQueued(bg, sess.ID)
		}
		a.report(bg, sess.ID, turn)
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
// (or, failing that, to the lane a rule set for after a turn) and, if other tasks build on it, the session's summary is written; the
// task's memories are updated from the session.
// A skill turn that does not finish successfully ends the chain there.
// A session waiting for the answer to its question on the task's thread runs
// no skills and does not move the task: its turn after the answer does.
func (a *App) afterTurn(ctx context.Context, sessionID, startLane int64) {
	sess, err := a.Store.Session(ctx, sessionID)
	if err != nil {
		return
	}
	if sess.NeedsInput != "" && a.waitsForAnswer(ctx, sess.ID) {
		a.summarizeForUsers(ctx, sess)
		a.memorizeAfterTurn(ctx, sess)
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
		a.moveToTurnLane(ctx, sess)
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

// SkillIntro opens the prompt that applies a skill to a session's work,
// NewSkillIntro the one that starts a session with a skill; ArtifactNote
// follows either for skills whose reply is kept. SkillContextIntro opens the
// context given with a skill that starts a session.
const (
	SkillIntro        = "Apply the skill %q to the work done in this session so far. Its instructions follow."
	NewSkillIntro     = "Apply the skill %q to this task. The task comes first, then the skill's instructions."
	ArtifactNote      = "Your final reply is saved as an artifact on the task: reply with the artifact itself, nothing before or after it."
	SkillContextIntro = "More context for this run of the skill:"
)

// RunSkill applies a skill to an idle session by hand, as its next turn.
func (a *App) RunSkill(ctx context.Context, sessionID, skillID int64) error {
	sess, err := a.Store.Session(ctx, sessionID)
	if err != nil {
		return err
	}
	if err := own(sess); err != nil {
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
	if sess.TaskID != 0 {
		var err error
		if task, err = a.Store.Task(ctx, sess.TaskID); err != nil {
			return err
		}
	}
	prompt, err := a.skillPrompt(ctx, task, sk, false, "")
	if err != nil {
		return err
	}
	// Skills name claude's models.
	if sk.Model != "" && sess.Agent == agent.Claude {
		sess.Model = sk.Model // this turn only: the session keeps its model
	}
	return a.launch(sess, prompt, sess.ClaudeSessionID, func(ctx context.Context, status string) {
		if status == store.StatusIdle {
			a.keepArtifact(ctx, sess, sk)
		}
		if done != nil {
			done(ctx, status)
		}
	})
}

// skillPrompt is the prompt that applies sk on task (zero for none). A fresh
// session has no work of its own to apply it to: its prompt brings the task's
// brief, and extra after the skill's instructions.
func (a *App) skillPrompt(ctx context.Context, task store.Task, sk store.Skill, fresh bool, extra string) (string, error) {
	brief := ""
	if task.ID != 0 {
		brief = a.TaskBrief(ctx, task)
	}
	body, err := renderPrompt(sk.Body, task, brief)
	if err != nil {
		return "", fmt.Errorf("skill template: %w", err)
	}
	intro := SkillIntro
	if fresh {
		intro = NewSkillIntro
	}
	parts := []string{fmt.Sprintf(intro, sk.Name)}
	if sk.Artifact && task.ID != 0 {
		parts = append(parts, ArtifactNote)
	}
	if fresh && brief != "" {
		parts = append(parts, brief, "---")
	}
	parts = append(parts, body)
	if extra = strings.TrimSpace(extra); fresh && extra != "" {
		parts = append(parts, "---", SkillContextIntro+"\n\n"+extra)
	}
	return strings.Join(parts, "\n\n"), nil
}

// keepArtifact keeps the session's last reply on its task if sk asks for it.
func (a *App) keepArtifact(ctx context.Context, sess store.Session, sk store.Skill) {
	if !sk.Artifact || sess.TaskID == 0 {
		return
	}
	if reply, err := a.lastReply(ctx, sess.ID); err != nil {
		a.notifySession(ctx, sess, fmt.Sprintf("Skill %q wrote no artifact in session %q", sk.Name, sess.Title))
	} else if _, err := a.Store.AddArtifact(ctx, store.Artifact{TaskID: sess.TaskID, SessionID: &sess.ID, Skill: sk.Name,
		Content: strings.TrimSpace(reply)}); err != nil {
		log.Printf("session %d: artifact: %v", sess.ID, err)
	}
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

// moveToTurnLane moves the session's task to the lane a rule set for after a
// turn, if the task has not moved since (moving clears it).
func (a *App) moveToTurnLane(ctx context.Context, sess store.Session) {
	if sess.TaskID == 0 {
		return
	}
	taskID := sess.TaskID
	lane, err := a.Store.TurnLane(ctx, taskID)
	if err != nil || lane == nil {
		return
	}
	if _, err := a.MoveTask(ctx, taskID, *lane, -1); err != nil {
		a.Store.Notify(ctx, &taskID, fmt.Sprintf("Session %q finished but the task could not be moved: %v", sess.Title, err))
		return
	}
	l, _ := a.Store.Lane(ctx, *lane)
	a.Store.Notify(ctx, &taskID, fmt.Sprintf("Agent finished %q — moved to %s", sess.Title, l.Name))
}

// MoveTask moves a task on the board and fires matching transition rules.
// It returns the sessions rules started, or continue with a skill. A rule
// with neither marks the task to move on once an agent turn on it finishes.
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
	rules, err := a.Store.MatchingRules(ctx, task.Kind, task.ProjectID, from, laneID)
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
		if r.SkillID == nil && strings.TrimSpace(r.Prompt) == "" {
			if err := a.Store.SetTurnLane(ctx, taskID, r.DoneLaneID); err != nil {
				a.Store.Notify(ctx, &taskID, fmt.Sprintf("Rule %q: %v", r.Name, err))
			}
			continue
		}
		if r.SkillID != nil {
			sess, err := a.ruleSkill(ctx, r, taskID, latest)
			if err != nil {
				a.Store.Notify(ctx, &taskID, fmt.Sprintf("Rule %q: %v", r.Name, err))
				continue
			}
			started = append(started, sess)
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

// ruleSkill runs the rule's skill in sess, the task's latest session, and
// returns the session it runs in. With no such session, while it works, or
// when it runs on another node, the skill starts a new session on the task
// with the rule's settings instead.
// Once the skill finishes successfully the task moves to the rule's done lane,
// unless it left the rule's lane meanwhile.
func (a *App) ruleSkill(ctx context.Context, r store.Rule, taskID int64, sess *store.Session) (store.Session, error) {
	fresh := func() (store.Session, error) {
		return a.StartSession(ctx, StartOpts{TaskID: taskID, SkillID: r.SkillID, Title: r.Name, Mode: r.Mode, Workdir: r.Workdir,
			Trigger: "rule:" + r.Name, DoneLaneID: r.DoneLaneID})
	}
	if sess == nil || !sess.Owned {
		return fresh()
	}
	sk, err := a.Store.Skill(ctx, *r.SkillID)
	if err != nil {
		return store.Session{}, fmt.Errorf("skill: %w", err)
	}
	s := *sess
	err = a.runSkill(ctx, s, sk, func(ctx context.Context, status string) {
		if status != store.StatusIdle {
			a.notifySession(ctx, s, fmt.Sprintf("Skill %q ended %s in session %q", sk.Name, status, s.Title))
			return
		}
		if r.DoneLaneID != nil {
			a.moveDone(ctx, s, r.ToLaneID, *r.DoneLaneID)
		}
	})
	if errors.Is(err, ErrBusy) {
		return fresh()
	}
	return s, err
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
// A hub runs none of them: each node runs its own (see DueSchedules, DueTasks).
func (a *App) RunScheduler(ctx context.Context, interval time.Duration) {
	if a.SyncRole == RoleHub {
		return
	}
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

// SessionPromptData is what the session prompt (Settings.SessionPrompt) can
// reference, e.g. "{{.Project.Name}}", "{{.Task.ID}}" or "{{.Session.Workdir}}".
// Task is zero for a standalone session.
type SessionPromptData struct {
	Project store.Project
	Task    store.Task
	Session store.Session
}

// RenderSessionPrompt renders the session prompt template for d.
func RenderSessionPrompt(tmpl string, d SessionPromptData) (string, error) {
	t, err := template.New("session").Option("missingkey=zero").Parse(tmpl)
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	err = t.Execute(&buf, d)
	return strings.TrimSpace(buf.String()), err
}

// sessionPrompt is the settings' session prompt for sess, rendered afresh each
// turn so it follows renames, then for a session on a task the ActivityNote.
// A session without a project gets the first one, where tasks go when no
// project is named.
func (a *App) sessionPrompt(ctx context.Context, sess store.Session) (string, error) {
	prompt, err := a.settingsPrompt(ctx, sess)
	if err != nil || sess.TaskID == 0 {
		return prompt, err
	}
	return strings.TrimSpace(prompt + "\n\n" + fmt.Sprintf(ActivityNote, sess.TaskID, sess.ID)), nil
}

func (a *App) settingsPrompt(ctx context.Context, sess store.Session) (string, error) {
	set, err := a.Store.Settings(ctx)
	if err != nil || strings.TrimSpace(set.SessionPrompt) == "" {
		return "", err
	}
	d := SessionPromptData{Session: sess}
	if sess.TaskID != 0 {
		if d.Task, err = a.Store.Task(ctx, sess.TaskID); err != nil {
			return "", err
		}
	}
	project := cmp.Or(d.Task.ProjectID, sess.ProjectID)
	if project != 0 {
		d.Project, err = a.Store.Project(ctx, project)
	} else {
		var ps []store.Project
		if ps, err = a.Store.Projects(ctx); len(ps) > 0 {
			d.Project = ps[0]
		}
	}
	if err != nil {
		return "", err
	}
	return RenderSessionPrompt(set.SessionPrompt, d)
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
	if shared := a.sharedBrief(ctx, t); shared != "" {
		brief += "\n\n---\n\n" + shared
	}
	return brief
}

// sharedBrief is the part of TaskBrief that comes from elsewhere: the task's
// memories and the results of the tasks it builds on; empty if it has neither.
func (a *App) sharedBrief(ctx context.Context, t store.Task) string {
	var parts []string
	if mem := a.memoryBrief(ctx, t); mem != "" {
		parts = append(parts, mem)
	}
	if len(t.ContextFrom) > 0 {
		var sources []string
		for _, id := range t.ContextFrom {
			src, err := a.Store.Task(ctx, id)
			if err != nil {
				continue
			}
			sources = append(sources, a.taskResults(ctx, src))
		}
		parts = append(parts, SharedIntro+"\n\n"+strings.Join(sources, "\n\n---\n\n"))
	}
	return strings.Join(parts, "\n\n---\n\n")
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
