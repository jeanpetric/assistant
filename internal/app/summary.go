package app

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"

	"github.com/jeanpetric/assistant/internal/agent"
	"github.com/jeanpetric/assistant/internal/store"
)

// SummaryPrompt asks a session's agent, in a fork of its conversation, to sum
// up the session for the agents of tasks that build on it.
const SummaryPrompt = `Another agent will build on the work of this session. It will not see this conversation, only your summary. ` +
	`Write a self-contained summary of the whole session (not only the last turn) for that agent:
- the goal;
- what was done and what was found out;
- the key findings with their concrete details: file paths, links, commands, names, numbers;
- decisions made and why, and approaches that were tried and dropped;
- open questions and anything left unfinished.
Reply with the summary only, without an introduction. Do not change anything now.`

// TranscriptSummaryPrompt asks for the same summary from the stored
// transcript, when the agent's own conversation can't be continued.
const TranscriptSummaryPrompt = `Below is the transcript of an AI agent's work session. Another agent will build on that work. ` +
	`It will not see the transcript, only your summary. Write a self-contained summary of the whole session for that agent:
- the goal;
- what was done and what was found out;
- the key findings with their concrete details: file paths, links, commands, names, numbers;
- decisions made and why, and approaches that were tried and dropped;
- open questions and anything left unfinished.
Reply with the summary only, without an introduction. Do not use any tools.`

// SummaryModel writes summaries from a stored transcript. The agent's own
// conversation is summed up with the session's model.
const SummaryModel = "haiku"

// maxTranscriptRunes bounds the transcript sent for a summary; the middle of a
// longer one is left out.
const maxTranscriptRunes = 150000

// Summarize writes the session's summary in the background, replacing the
// one it has. It does nothing if a summary is already being written; a
// notification tells if writing it fails.
func (a *App) Summarize(ctx context.Context, sessionID int64) error {
	if _, err := a.Store.Session(ctx, sessionID); err != nil {
		return err
	}
	a.mu.Lock()
	_, busy := a.running[sessionID]
	a.mu.Unlock()
	if busy {
		return ErrBusy
	}
	a.startSummary(sessionID, true)
	return nil
}

// Summarizing tells whether the session's summary is being written.
func (a *App) Summarizing(sessionID int64) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	_, ok := a.summarizing[sessionID]
	return ok
}

// summarizeForUsers writes the summary of a session whose turn just finished,
// if other tasks build on its task: their agents will read it.
func (a *App) summarizeForUsers(ctx context.Context, sess store.Session) {
	if sess.TaskID == 0 {
		return
	}
	if users, err := a.Store.ContextUsers(ctx, sess.TaskID); err != nil || len(users) == 0 {
		return
	}
	a.startSummary(sess.ID, false)
}

// startSummary writes the session's summary in the background, unless one is
// already being written. Failures are logged, and with notify also notified.
func (a *App) startSummary(sessionID int64, notify bool) {
	a.mu.Lock()
	if _, ok := a.summarizing[sessionID]; ok || a.stopping {
		a.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	a.summarizing[sessionID] = cancel
	a.wg.Add(1)
	a.mu.Unlock()

	go func() {
		defer a.wg.Done()
		defer func() {
			a.mu.Lock()
			delete(a.summarizing, sessionID)
			a.mu.Unlock()
			cancel()
		}()
		sess, err := a.Store.Session(ctx, sessionID)
		if err == nil {
			err = a.summarize(ctx, sess)
		}
		if err == nil || ctx.Err() != nil {
			return
		}
		log.Printf("session %d: summary: %v", sessionID, err)
		if notify {
			var taskID *int64
			if sess.TaskID != 0 {
				taskID = &sess.TaskID
			}
			a.Store.Notify(context.Background(), taskID, fmt.Sprintf("Could not summarise session %q: %v", sess.Title, err))
		}
	}()
}

// summarize asks a fork of the session's agent conversation for the summary,
// so the conversation itself is not extended and nothing goes into the
// session's transcript. Without a conversation to fork, or if that fails, the
// summary is written from the stored transcript instead.
func (a *App) summarize(ctx context.Context, sess store.Session) error {
	upto, err := a.Store.LastMessageID(ctx, sess.ID)
	if err != nil {
		return err
	}
	if upto == 0 {
		return errors.New("the session has no messages")
	}
	var summary string
	var cost float64
	if sess.ClaudeSessionID != "" {
		summary, cost, err = a.ask(ctx, agent.Request{Prompt: SummaryPrompt, Workdir: sess.Workdir, Mode: "plan",
			Model: sess.Model, ResumeID: sess.ClaudeSessionID, Fork: true})
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			log.Printf("session %d: summary from the agent: %v; using the transcript", sess.ID, err)
		}
	}
	if summary == "" {
		msgs, err := a.Store.Messages(ctx, sess.ID, 0)
		if err != nil {
			return err
		}
		var c float64
		summary, c, err = a.ask(ctx, agent.Request{Prompt: TranscriptSummaryPrompt + "\n\n<transcript>\n" + transcript(msgs, upto) + "</transcript>",
			Workdir: a.DefaultWorkdir, Mode: "plan", Model: SummaryModel})
		cost += c
		if err != nil {
			return err
		}
	}
	return a.Store.SetSessionSummary(ctx, sess.ID, summary, upto, cost)
}

// ask runs one agent turn outside any session and returns its final answer.
func (a *App) ask(ctx context.Context, req agent.Request) (string, float64, error) {
	var result, text, failure string
	var cost float64
	err := a.Runner.Run(ctx, req, func(ev agent.Event) {
		cost += ev.CostUSD
		switch {
		case ev.Kind == "error":
			failure = ev.Content
		case ev.Kind == "result":
			result = ev.Content
		case ev.Role == "assistant" && ev.Kind == "text":
			text = ev.Content
		}
	})
	if err == nil && failure != "" {
		err = errors.New(failure)
	}
	if err != nil {
		return "", cost, err
	}
	if strings.TrimSpace(result) == "" {
		result = text
	}
	if result = strings.TrimSpace(result); result == "" {
		return "", cost, errors.New("the agent wrote no summary")
	}
	return result, cost, nil
}

// transcript renders the session's messages up to upto as plain text for a
// summary. Tool output is cut short; so is the middle of a long session.
func transcript(msgs []store.Message, upto int64) string {
	var b strings.Builder
	for _, m := range msgs {
		if m.ID > upto {
			break
		}
		label, max := "Agent", 20000
		switch {
		case m.Role == "user" && m.Kind == "text":
			label = "User"
		case m.Kind == "context":
			label, max = "Context", 2000
		case m.Kind == "tool_use":
			label, max = "Tool call", 1000
		case m.Kind == "tool_result":
			label, max = "Tool result", 2000
		case m.Kind == "result":
			label = "Final reply of the turn"
		case m.Kind == "error":
			label = "Error"
		}
		content := strings.TrimSpace(m.Content)
		if r := []rune(content); len(r) > max {
			content = string(r[:max]) + " …"
		}
		fmt.Fprintf(&b, "[%s]\n%s\n\n", label, content)
	}
	s := b.String()
	if r := []rune(s); len(r) > maxTranscriptRunes {
		head := maxTranscriptRunes / 5
		s = string(r[:head]) + "\n… (the middle of the session is left out) …\n\n" + string(r[len(r)-(maxTranscriptRunes-head):])
	}
	return s
}
