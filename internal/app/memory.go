package app

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"

	"github.com/jeanpetric/assistant/internal/agent"
	"github.com/jeanpetric/assistant/internal/store"
)

// ErrNoMemory: the session's task is linked to no memory.
var ErrNoMemory = errors.New("the session's task has no memory")

// MemoryIntro opens the part of a task brief that carries its memories.
const MemoryIntro = "Memory: lasting knowledge kept across tasks, linked to this task. " +
	"After each of your turns it is updated from this session's work, so you need not write it down yourself."

// NoChanges is the reply of an agent that has nothing to add to a memory.
const NoChanges = "NO_CHANGES"

// memoryRules tell an agent how to write a memory's new version.
const memoryRules = `Write the memory's new version:
- add the lasting knowledge from the session: facts and findings with their concrete details (file paths, links, commands, names, numbers), decisions and why, what the user answered or prefers, approaches that failed;
- correct or remove what the session showed to be wrong or outdated;
- keep what is still true, even if the session did not touch it;
- leave out what only mattered to the session's own task: its progress, temporary state, next steps;
- keep it organised by topic and concise, within about 5000 words: merge repeats, tighten wording.
If the session found out nothing lasting about the memory's subject, reply with ` + NoChanges + ` only. ` +
	`Otherwise reply with the whole new memory only, in Markdown, without an introduction or code fences.`

// MemoryPrompt asks a session's agent, in a fork of its conversation, for the
// new version of a memory.
func MemoryPrompt(m store.Memory) string {
	return fmt.Sprintf("Update the memory %q with what this session found out. %s"+
		"Agents of later tasks read it instead of this conversation.\n\n%s\nDo not change anything else now.\n\n<memory>\n%s\n</memory>",
		m.Name, memorySubject(m), memoryRules, m.Body)
}

// TranscriptMemoryPrompt asks for the same from the stored transcript, when
// the agent's own conversation can't be continued.
func TranscriptMemoryPrompt(m store.Memory, transcript string) string {
	return fmt.Sprintf("Below are the memory %q and the transcript of an AI agent's work session. Update the memory with what the session found out. %s"+
		"Agents of later tasks read it instead of the transcript.\n\n%s\nDo not use any tools.\n\n<memory>\n%s\n</memory>\n\n<transcript>\n%s</transcript>",
		m.Name, memorySubject(m), memoryRules, m.Body, transcript)
}

func memorySubject(m store.Memory) string {
	s := "The memory is a Markdown document of lasting knowledge, kept across tasks"
	if d := strings.TrimSpace(m.Description); d != "" {
		s += ", about: " + d
	}
	return s + ". "
}

// memoryRunes bounds each memory in a task brief.
const memoryRunes = 60000

// memoryBrief renders the task's memories for its brief, "" without any.
func (a *App) memoryBrief(ctx context.Context, t store.Task) string {
	var parts []string
	for _, id := range t.Memories {
		m, err := a.Store.Memory(ctx, id)
		if err != nil {
			continue
		}
		var b strings.Builder
		fmt.Fprintf(&b, "## Memory #%d: %s\n", m.ID, m.Name)
		if d := strings.TrimSpace(m.Description); d != "" {
			fmt.Fprintf(&b, "\n%s\n", d)
		}
		body := strings.TrimSpace(m.Body)
		if body == "" {
			body = "(Empty so far.)"
		}
		fmt.Fprintf(&b, "\n%s", truncate(body, memoryRunes))
		parts = append(parts, b.String())
	}
	if len(parts) == 0 {
		return ""
	}
	return MemoryIntro + "\n\n" + strings.Join(parts, "\n\n")
}

// UpdateMemories updates the memories of the session's task from the session
// in the background. A notification tells if it fails.
func (a *App) UpdateMemories(ctx context.Context, sessionID int64) error {
	sess, err := a.Store.Session(ctx, sessionID)
	if err != nil {
		return err
	}
	if sess.TaskID == 0 {
		return ErrNoMemory
	}
	t, err := a.Store.Task(ctx, sess.TaskID)
	if err != nil {
		return err
	}
	if len(t.Memories) == 0 {
		return ErrNoMemory
	}
	a.mu.Lock()
	_, busy := a.running[sessionID]
	a.mu.Unlock()
	if busy {
		return ErrBusy
	}
	a.startMemorize(sessionID)
	return nil
}

// Memorizing tells whether memories are being updated from the session.
func (a *App) Memorizing(sessionID int64) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	_, ok := a.memorizing[sessionID]
	return ok
}

// memorizeAfterTurn updates the task's memories after a successful turn.
func (a *App) memorizeAfterTurn(ctx context.Context, sess store.Session) {
	if sess.TaskID == 0 {
		return
	}
	if t, err := a.Store.Task(ctx, sess.TaskID); err != nil || len(t.Memories) == 0 {
		return
	}
	a.startMemorize(sess.ID)
}

// startMemorize updates the memories from the session in the background. If
// that is under way already, it runs once more when done, so the newest turn
// is covered too.
func (a *App) startMemorize(sessionID int64) {
	a.mu.Lock()
	if a.stopping {
		a.mu.Unlock()
		return
	}
	if _, ok := a.memorizing[sessionID]; ok {
		a.memorizeAgain[sessionID] = true
		a.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	a.memorizing[sessionID] = cancel
	a.wg.Add(1)
	a.mu.Unlock()

	go func() {
		defer a.wg.Done()
		defer cancel()
		for {
			a.memorize(ctx, sessionID)
			a.mu.Lock()
			again := a.memorizeAgain[sessionID] && ctx.Err() == nil
			delete(a.memorizeAgain, sessionID)
			if !again {
				delete(a.memorizing, sessionID)
			}
			a.mu.Unlock()
			if !again {
				return
			}
		}
	}()
}

// memorize updates each memory of the session's task in turn and notifies
// about failures; changes are visible in the memories' revisions.
func (a *App) memorize(ctx context.Context, sessionID int64) {
	sess, err := a.Store.Session(ctx, sessionID)
	if err != nil {
		return
	}
	t, err := a.Store.Task(ctx, sess.TaskID)
	if err != nil {
		return
	}
	for _, id := range t.Memories {
		m, _, err := a.memorizeOne(ctx, sess, id)
		switch {
		case ctx.Err() != nil:
			return
		case errors.Is(err, store.ErrNotFound): // deleted meanwhile
		case err != nil:
			log.Printf("session %d: memory %d: %v", sess.ID, id, err)
			a.notifySession(ctx, sess, fmt.Sprintf("Could not update memory %q from session %q: %v", m.Name, sess.Title, err))
		}
	}
}

// FillMemory writes a memory just linked to a task from the task's sessions
// so far, oldest first, in the background, like an update after each of
// their turns. A notification tells when it is done, or why it failed.
func (a *App) FillMemory(taskID, memoryID int64) {
	a.mu.Lock()
	if _, ok := a.filling[memoryID]; ok || a.stopping {
		a.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	a.filling[memoryID] = cancel
	a.wg.Add(1)
	a.mu.Unlock()

	go func() {
		defer a.wg.Done()
		defer func() {
			a.mu.Lock()
			delete(a.filling, memoryID)
			a.mu.Unlock()
			cancel()
		}()
		sessions, err := a.Store.Sessions(ctx, taskID, "")
		if err != nil {
			log.Printf("task %d: memory %d: %v", taskID, memoryID, err)
			return
		}
		var last store.Session
		var m store.Memory
		changed := false
		for i := len(sessions) - 1; i >= 0; i-- { // listed newest first
			sess := sessions[i]
			if n, err := a.Store.LastMessageID(ctx, sess.ID); err != nil || n == 0 {
				continue
			}
			var ch bool
			m, ch, err = a.memorizeOne(ctx, sess, memoryID)
			switch {
			case ctx.Err() != nil, errors.Is(err, store.ErrNotFound):
				return
			case err != nil:
				log.Printf("session %d: memory %d: %v", sess.ID, memoryID, err)
				a.notifySession(ctx, sess, fmt.Sprintf("Could not write memory %q from session %q: %v", m.Name, sess.Title, err))
				return
			}
			changed = changed || ch
			last = sess
		}
		if changed {
			a.notifySession(ctx, last, fmt.Sprintf("Memory %q written from the task's sessions", m.Name))
		}
	}()
}

// Filling tells whether the memory is being written from a task's sessions.
func (a *App) Filling(memoryID int64) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	_, ok := a.filling[memoryID]
	return ok
}

// memorizeOne asks for the memory's new version and saves it. Updates of one
// memory run one at a time; if it was edited meanwhile, it is asked again.
func (a *App) memorizeOne(ctx context.Context, sess store.Session, memoryID int64) (store.Memory, bool, error) {
	lock := a.memoryLock(memoryID)
	lock.Lock()
	defer lock.Unlock()
	var m store.Memory
	for attempt := 0; ; attempt++ {
		var err error
		if m, err = a.Store.Memory(ctx, memoryID); err != nil {
			return m, false, err
		}
		body, cost, err := a.askMemory(ctx, sess, m)
		if cost > 0 {
			a.Store.AddBackgroundCost(context.Background(), sess.ID, cost)
		}
		if err != nil || body == NoChanges {
			return m, false, err
		}
		taskID := sess.TaskID
		_, changed, err := a.Store.SetMemoryBody(ctx, store.MemoryRevision{MemoryID: m.ID, Body: body, Source: store.SourceAgent,
			SessionID: &sess.ID, TaskID: &taskID}, &m.Revision)
		if errors.Is(err, store.ErrMemoryChanged) && attempt == 0 {
			continue
		}
		return m, changed, err
	}
}

func (a *App) memoryLock(id int64) *sync.Mutex {
	a.mu.Lock()
	defer a.mu.Unlock()
	l, ok := a.memoryLocks[id]
	if !ok {
		l = &sync.Mutex{}
		a.memoryLocks[id] = l
	}
	return l
}

// askMemory asks a fork of the session's agent conversation for the memory's
// new version, or, failing that, the stored transcript. It returns the new
// body or NoChanges.
func (a *App) askMemory(ctx context.Context, sess store.Session, m store.Memory) (string, float64, error) {
	var reply string
	var cost float64
	var err error
	if sess.ClaudeSessionID != "" && agent.CanFork(sess.Agent) {
		reply, cost, err = a.ask(ctx, agent.Request{Prompt: MemoryPrompt(m), Workdir: sess.Workdir, Mode: "plan",
			Model: sess.Model, ResumeID: sess.ClaudeSessionID, Fork: true})
		if ctx.Err() != nil {
			return "", cost, ctx.Err()
		}
		if err != nil {
			log.Printf("session %d: memory from the agent: %v; using the transcript", sess.ID, err)
		}
	}
	if reply == "" {
		upto, err := a.Store.LastMessageID(ctx, sess.ID)
		if err != nil {
			return "", cost, err
		}
		if upto == 0 {
			return "", cost, errors.New("the session has no messages")
		}
		msgs, err := a.Store.Messages(ctx, sess.ID, 0)
		if err != nil {
			return "", cost, err
		}
		var c float64
		reply, c, err = a.ask(ctx, agent.Request{Prompt: TranscriptMemoryPrompt(m, transcript(msgs, upto)),
			Workdir: a.DefaultWorkdir, Mode: "plan", Model: SummaryModel})
		cost += c
		if err != nil {
			return "", cost, err
		}
	}
	return cleanMemory(reply), cost, nil
}

// cleanMemory unwraps a reply the agent put in a code fence anyway.
func cleanMemory(s string) string {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "```") && strings.HasSuffix(s, "```") && len(s) > 6 {
		if i := strings.IndexByte(s, '\n'); i > 0 {
			s = strings.TrimSpace(strings.TrimSuffix(s[i+1:], "```"))
		}
	}
	if strings.Trim(s, "`*_ .") == NoChanges {
		return NoChanges
	}
	return s
}

// DiffLine is one line of a line diff: Op is " " (kept), "-" or "+".
type DiffLine struct {
	Op   string `json:"op"`
	Text string `json:"text"`
}

// maxDiffCells bounds the work of LineDiff; beyond it the whole text changes.
const maxDiffCells = 4_000_000

// LineDiff compares two texts line by line (longest common subsequence).
func LineDiff(old, new string) []DiffLine {
	split := func(s string) []string {
		if s == "" {
			return nil
		}
		return strings.Split(s, "\n")
	}
	a, b := split(old), split(new)
	out := []DiffLine{}
	if len(a)*len(b) > maxDiffCells {
		for _, l := range a {
			out = append(out, DiffLine{"-", l})
		}
		for _, l := range b {
			out = append(out, DiffLine{"+", l})
		}
		return out
	}
	// lcs[i][j]: length of the longest common subsequence of a[i:] and b[j:].
	lcs := make([][]int, len(a)+1)
	for i := range lcs {
		lcs[i] = make([]int, len(b)+1)
	}
	for i := len(a) - 1; i >= 0; i-- {
		for j := len(b) - 1; j >= 0; j-- {
			if a[i] == b[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else {
				lcs[i][j] = max(lcs[i+1][j], lcs[i][j+1])
			}
		}
	}
	i, j := 0, 0
	for i < len(a) && j < len(b) {
		switch {
		case a[i] == b[j]:
			out = append(out, DiffLine{" ", a[i]})
			i, j = i+1, j+1
		case lcs[i+1][j] >= lcs[i][j+1]:
			out = append(out, DiffLine{"-", a[i]})
			i++
		default:
			out = append(out, DiffLine{"+", b[j]})
			j++
		}
	}
	for ; i < len(a); i++ {
		out = append(out, DiffLine{"-", a[i]})
	}
	for ; j < len(b); j++ {
		out = append(out, DiffLine{"+", b[j]})
	}
	return out
}
