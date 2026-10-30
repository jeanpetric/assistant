package app

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jeanpetric/assistant/internal/slack"
	"github.com/jeanpetric/assistant/internal/store"
)

// SlackSent is how a task message went to the task's Slack channel: the ts
// of its (first) Slack message and the thread it went to ("": top level).
type SlackSent struct {
	Channel  string `json:"channel"`
	TS       string `json:"ts,omitempty"`
	ThreadTS string `json:"thread_ts,omitempty"`
	Error    string `json:"error,omitempty"`
}

// SlackFromUser marks the user's messages in Slack, where the bot posts them.
const SlackFromUser = "_(iz assistenta)_"

// slackTo is where in Slack a task message goes: the channel, and the
// thread's root ts ("": top level).
type slackTo struct {
	c      store.SlackChannel
	thread string
}

// slackTarget is where in the task's Slack channel m goes (see PostOpts);
// nil if it goes nowhere: Slack is off, the task has no channel, or m came
// from Slack.
func (a *App) slackTarget(ctx context.Context, m store.TaskMessage, o PostOpts) (*slackTo, error) {
	if o.ReplyTo != 0 && o.TopLevel {
		return nil, fmt.Errorf("%w: reply_to and top_level don't go together", ErrBadMessage)
	}
	if a.Slack == nil || m.Author != "" {
		return nil, nil
	}
	c, err := a.Store.SlackChannel(ctx, m.TaskID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	to := &slackTo{c: c}
	switch {
	case o.TopLevel:
	case o.ReplyTo != 0:
		r, err := a.Store.TaskMessage(ctx, o.ReplyTo)
		var channel string
		if err == nil && r.TaskID == m.TaskID {
			channel, to.thread, err = a.Store.SlackThreadOf(ctx, r.ID)
		}
		if err == nil && (r.TaskID != m.TaskID || channel != c.ChannelID) {
			err = store.ErrNotFound
		}
		if errors.Is(err, store.ErrNotFound) {
			return nil, fmt.Errorf("%w: reply_to is not a message of the task's Slack channel", ErrBadMessage)
		} else if err != nil {
			return nil, err
		}
	case m.SessionID != nil:
		to.thread, err = a.Store.SlackLastThread(ctx, c.ChannelID, *m.SessionID)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return nil, err
		}
	}
	return to, nil
}

// postSlack posts the task message to Slack, in parts if it is long: the
// first where to says, the rest in its thread. Each part is recorded as the
// message's, so polling follows the replies and doesn't take it in. A
// failure stops there and shows in the task's Slack status.
func (a *App) postSlack(ctx context.Context, to slackTo, x store.TaskMessage) *SlackSent {
	ctx = context.WithoutCancel(ctx) // what Slack took is recorded
	out := &SlackSent{Channel: to.c.ChannelID, ThreadTS: to.thread}
	thread := to.thread
	var err error
	for _, part := range splitSlack(slackBody(x), slack.MaxMarkdown) {
		var ts string
		if ts, err = a.Slack.PostMessage(ctx, to.c.ChannelID, part, thread); err != nil {
			break
		}
		sm := store.SlackMessage{ChannelID: to.c.ChannelID, TS: ts, ThreadTS: thread, TaskMessageID: &x.ID}
		if thread == "" {
			sm.LastReplyTS = ts
		}
		if err = a.Store.AddSlackSent(ctx, sm); err != nil {
			break
		}
		if out.TS == "" {
			out.TS = ts
		}
		if thread == "" {
			thread = ts
		}
	}
	if err != nil {
		out.Error = err.Error()
	}
	a.setSlackPostStatus(ctx, to.c, err)
	return out
}

// setSlackPostStatus records how posting to the task's channel last went,
// notifying of the first failure in a row.
func (a *App) setSlackPostStatus(ctx context.Context, c store.SlackChannel, err error) {
	a.mu.Lock()
	st := a.slackStatus[c.TaskID]
	prev := st.PostError
	st.PostError = ""
	if err != nil {
		st.PostError = err.Error()
	}
	a.slackStatus[c.TaskID] = st
	a.mu.Unlock()
	if err != nil && prev == "" {
		a.Store.Notify(ctx, &c.TaskID, fmt.Sprintf("Posting to Slack #%s failed: %v", c.ChannelName, err))
	}
}

// slackBody is the task message as it shows in Slack: a question marked,
// the user's marked as coming from the assistant.
func slackBody(x store.TaskMessage) string {
	body := x.Body
	if x.Question {
		body = "❓ " + body
	}
	if x.SessionID == nil {
		sep := " "
		if strings.Contains(body, "\n") {
			sep = "\n"
		}
		body = SlackFromUser + sep + body
	}
	return body
}

// splitSlack splits text into parts of at most max characters, at a blank
// line, else a line end, else a space in the later half of a part. A code
// block cut in two is closed at the end of one part and opened again in
// the next.
func splitSlack(text string, max int) []string {
	const fence = "```"
	var parts []string
	rest := []rune(text)
	open := false // the part starts inside a code block
	for {
		prefix := ""
		if open {
			if r := string(rest); r == fence || strings.HasPrefix(r, fence+"\n") {
				rest = rest[min(len(rest), len(fence)+1):] // it ends right where the part starts
				open = false
				continue
			}
			prefix = fence + "\n"
		}
		room := max - len([]rune(prefix))
		if len(rest) == 0 && len(parts) > 0 {
			return parts
		}
		if len(rest) <= room {
			return append(parts, prefix+string(rest))
		}
		part, skip := cutSlack(rest, room)
		ends := open != (strings.Count(part, fence)%2 == 1) // inside a code block
		if ends && len([]rune(part))+len(fence)+1 > room {
			part, skip = cutSlack(rest, room-len(fence)-1)
			ends = open != (strings.Count(part, fence)%2 == 1)
		}
		rest = rest[len([]rune(part))+skip:]
		if ends {
			part += "\n" + fence
		}
		open = ends
		parts = append(parts, prefix+part)
	}
}

// cutSlack gives the first part of text that fits in room characters, cut
// where splitSlack says, and how many characters after it the cut takes.
func cutSlack(text []rune, room int) (string, int) {
	window := string(text[:room])
	for _, sep := range []string{"\n\n", "\n", " "} {
		if i := strings.LastIndex(window, sep); i > 0 && len([]rune(window[:i])) > room/2 {
			return window[:i], len([]rune(sep))
		}
	}
	return window, 0
}
