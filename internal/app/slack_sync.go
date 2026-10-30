package app

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jeanpetric/assistant/internal/slack"
	"github.com/jeanpetric/assistant/internal/store"
)

var (
	ErrSlackOff = errors.New("Slack is off: no bot token")
	// ErrNotOnTask: the orchestrator named is not one of the task's sessions.
	ErrNotOnTask = errors.New("the session is not one of the task's")
)

// SlackStatus is how the last poll of a task's channel went; in memory only.
type SlackStatus struct {
	LastSyncAt *time.Time `json:"last_sync_at"`
	Error      string     `json:"error"`
	// Undelivered: why the latest messages could not go to the orchestrator.
	Undelivered string `json:"undelivered,omitempty"`
	// PostError: why the latest task message could not go to the channel.
	PostError string `json:"post_error,omitempty"`
}

// SlackStatus returns the status of the last poll of the task's channel.
func (a *App) SlackStatus(taskID int64) SlackStatus {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.slackStatus[taskID]
}

// KickSlack asks the polling loop for a poll now instead of at its next tick.
func (a *App) KickSlack() {
	select {
	case a.slackKick <- struct{}{}:
	default:
	}
}

// ConnectSlack connects the task to a Slack channel (its id or archive URL),
// whose messages from now on come to the task's thread and go to the
// orchestrator session (nil: none yet; it must be one of the task's). The
// bot must be in the channel. The channel's earlier messages are not taken in.
func (a *App) ConnectSlack(ctx context.Context, taskID int64, channel string, sessionID *int64) (store.SlackChannel, error) {
	if a.Slack == nil {
		return store.SlackChannel{}, ErrSlackOff
	}
	if _, err := a.Store.Task(ctx, taskID); err != nil {
		return store.SlackChannel{}, err
	}
	if sessionID != nil {
		sess, err := a.Store.Session(ctx, *sessionID)
		if err != nil {
			return store.SlackChannel{}, err
		}
		if sess.TaskID != taskID {
			return store.SlackChannel{}, ErrNotOnTask
		}
	}
	id := slack.ChannelID(channel)
	info, err := a.Slack.ConversationInfo(ctx, id)
	switch {
	case errors.Is(err, slack.ErrChannelNotFound):
		return store.SlackChannel{}, fmt.Errorf("Slack channel %s not found, or the bot can't see it: %w", id, err)
	case err != nil:
		return store.SlackChannel{}, err
	case !info.IsMember:
		return store.SlackChannel{}, fmt.Errorf("the bot is not in #%s: invite it to the channel first: %w", info.Name, slack.ErrNotInChannel)
	}
	a.slackMu.Lock() // not in the middle of a poll of the channel it replaces
	defer a.slackMu.Unlock()
	c, err := a.Store.ConnectSlack(ctx, store.SlackChannel{TaskID: taskID, ChannelID: id, ChannelName: info.Name,
		SessionID: sessionID, Cursor: slackTS(a.Now())})
	if err != nil {
		return c, err
	}
	a.mu.Lock()
	delete(a.slackStatus, taskID)
	a.mu.Unlock()
	a.KickSlack()
	return c, nil
}

// DisconnectSlack ends the task's connection to its channel: the channel is
// no longer polled. The orchestrator session and the task's thread stay.
func (a *App) DisconnectSlack(ctx context.Context, taskID int64) error {
	a.slackMu.Lock() // not in the middle of a poll of it
	defer a.slackMu.Unlock()
	if err := a.Store.DisconnectSlack(ctx, taskID); err != nil {
		return err
	}
	a.mu.Lock()
	delete(a.slackStatus, taskID)
	a.mu.Unlock()
	return nil
}

// slackHistory is how many of a channel's latest top-level messages a poll
// reads: the new ones, and the latest reply of their threads.
const slackHistory = 100

// slackThreadsPerPoll caps the threads whose new replies one poll of a
// channel reads (Slack allows some 50 calls a minute); the rest wait for
// the next.
const slackThreadsPerPoll = 10

// SyncSlack polls every connected channel: their new messages, top-level and
// in threads, come to the task's thread, and all of a channel's go to its
// orchestrator in one message. It does nothing without a client, and on a
// hub. A channel's failure doesn't stop the others; only the first failure
// in a row of a channel notifies.
func (a *App) SyncSlack(ctx context.Context) error {
	if a.Slack == nil || a.SyncRole == RoleHub {
		return nil
	}
	a.slackMu.Lock()
	defer a.slackMu.Unlock()
	channels, err := a.Store.SlackChannels(ctx)
	if err != nil || len(channels) == 0 {
		return err
	}
	if a.slackMe == nil {
		me, err := a.Slack.AuthTest(ctx)
		if err == nil {
			a.slackMe = &me
		}
		for _, c := range channels {
			a.setSlackStatus(ctx, c, err, nil)
		}
		if err != nil {
			return err
		}
	}
	var errs []error
	for _, c := range channels {
		delivery, err := a.pollSlack(ctx, c)
		if err != nil {
			errs = append(errs, fmt.Errorf("#%s: %w", c.ChannelName, err))
		}
		a.setSlackStatus(ctx, c, err, delivery)
	}
	return errors.Join(errs...)
}

// setSlackStatus records how the channel's poll went and how passing its
// messages to the orchestrator did (delivery nil: none came; "": they went),
// notifying of the first failure of either in a row.
func (a *App) setSlackStatus(ctx context.Context, c store.SlackChannel, err error, delivery *string) {
	now := a.Now()
	st := SlackStatus{LastSyncAt: &now}
	if err != nil {
		st.Error = err.Error()
	}
	a.mu.Lock()
	prev := a.slackStatus[c.TaskID]
	st.Undelivered, st.PostError = prev.Undelivered, prev.PostError
	if delivery != nil {
		st.Undelivered = *delivery
	}
	undelivered := st.Undelivered
	a.slackStatus[c.TaskID] = st
	a.mu.Unlock()
	if ctx.Err() != nil {
		return
	}
	if err != nil && prev.Error == "" {
		a.Store.Notify(ctx, &c.TaskID, fmt.Sprintf("Slack polling of #%s failed: %v", c.ChannelName, err))
	}
	if undelivered != "" && prev.Undelivered == "" {
		a.Store.Notify(ctx, &c.TaskID, fmt.Sprintf("Messages from Slack #%s wait on the task's thread: %s", c.ChannelName, undelivered))
	}
}

// slackIn is a message new to a poll: a top-level one has no thread.
type slackIn struct {
	msg    slack.Message
	thread string
}

// pollSlack takes the channel's new messages in and passes them to its
// orchestrator. It returns how that went: nil if none came, else why they
// could not go there ("": they did).
func (a *App) pollSlack(ctx context.Context, c store.SlackChannel) (*string, error) {
	roots, err := a.Slack.History(ctx, c.ChannelID, "", slackHistory)
	if err != nil {
		return nil, err
	}
	// More new messages than one read takes, after a pause: all of them.
	if len(roots) >= slackHistory && slices.IndexFunc(roots, func(m slack.Message) bool { return !tsAfter(m.TS, c.Cursor) }) < 0 {
		if roots, err = a.Slack.History(ctx, c.ChannelID, c.Cursor, 0); err != nil {
			return nil, err
		}
	}
	slices.SortFunc(roots, func(x, y slack.Message) int { return tsCmp(x.TS, y.TS) })
	seen, err := a.Store.SlackThreads(ctx, c.ChannelID)
	if err != nil {
		return nil, err
	}
	// Replies of a thread not seen are new from the time of connecting.
	connected := slackTS(c.ConnectedAt)
	var in []slackIn
	lastReply := map[string]string{} // root ts → its thread's latest reply now seen
	cursor := c.Cursor
	var errs []error
	threads := 0
	for _, m := range roots {
		if m.ThreadTS != "" && m.ThreadTS != m.TS {
			continue // a reply also sent to the channel: it comes with its thread
		}
		isNew := tsAfter(m.TS, c.Cursor)
		if isNew && tsAfter(m.TS, cursor) {
			cursor = m.TS
		}
		root, known := seen[m.TS]
		last := root.LastReplyTS
		switch {
		case !known && isNew:
			in = append(in, slackIn{msg: m})
			last = m.TS
		case !known:
			last = connected
		}
		if m.LatestReply == "" || !tsAfter(m.LatestReply, last) {
			continue
		}
		if threads++; threads > slackThreadsPerPoll {
			continue
		}
		replies, err := a.Slack.Replies(ctx, c.ChannelID, m.TS, last)
		if err != nil {
			errs = append(errs, err) // the thread is read again next time
			continue
		}
		newest := last
		for _, r := range replies {
			if r.TS == m.TS || !tsAfter(r.TS, last) {
				continue
			}
			in = append(in, slackIn{msg: r, thread: m.TS})
			if tsAfter(r.TS, newest) {
				newest = r.TS
			}
		}
		lastReply[m.TS] = newest
	}

	// Recorded before the thread's latest reply and the cursor move past
	// them, so a poll cut short takes nothing in twice and loses nothing.
	slices.SortStableFunc(in, func(x, y slackIn) int { return tsCmp(x.msg.TS, y.msg.TS) })
	var lines, taken []string
	for _, x := range in {
		sm := store.SlackMessage{ChannelID: c.ChannelID, TS: x.msg.TS, ThreadTS: x.thread}
		if x.thread == "" {
			sm.LastReplyTS = x.msg.TS
		}
		var tm *store.TaskMessage
		if a.slackTakesIn(x.msg) {
			tm = &store.TaskMessage{TaskID: c.TaskID, Author: a.slackAuthor(ctx, x.msg), Body: a.slackText(ctx, x.msg.Text)}
		} else if x.thread != "" {
			continue // nothing to keep of it
		}
		added, err := a.Store.AddSlackMessage(ctx, sm, tm)
		if err != nil {
			return nil, err
		}
		if added != nil {
			thread := x.thread
			if thread == "" {
				thread = x.msg.TS
			}
			taken = append(taken, x.msg.TS)
			lines = append(lines, fmt.Sprintf("[Slack #%s] %s — task_message %d, ts %s, thread %s: %s",
				c.ChannelName, added.Author, added.ID, x.msg.TS, thread, added.Body))
		}
	}
	for ts, last := range lastReply {
		if err := a.Store.SetSlackLastReply(ctx, c.ChannelID, ts, last); err != nil {
			return nil, err
		}
	}
	if cursor != c.Cursor {
		if err := a.Store.SetSlackCursor(ctx, c.TaskID, cursor); err != nil {
			return nil, err
		}
	}
	if len(lines) == 0 {
		return nil, errors.Join(errs...)
	}
	why := a.deliverSlack(ctx, c, strings.Join(lines, "\n"))
	if why == "" {
		// The orchestrator's next messages go to the latest one's thread.
		if err := a.Store.SetSlackDelivered(ctx, c.ChannelID, taken, *c.SessionID); err != nil {
			errs = append(errs, err)
		}
	}
	return &why, errors.Join(errs...)
}

// deliverSlack passes a poll's new messages to the channel's orchestrator as
// one message, queued while it works. It returns why they could not go ("":
// they did).
func (a *App) deliverSlack(ctx context.Context, c store.SlackChannel, text string) string {
	if c.SessionID == nil {
		return "the task has no orchestrator session"
	}
	if _, err := a.SendMessage(ctx, *c.SessionID, text); err != nil {
		return fmt.Sprintf("session %d can't take them: %v", *c.SessionID, err)
	}
	return ""
}

// slackWritten are the subtypes of messages someone wrote; the others (a
// member joining, a topic changed …) are the channel's own.
var slackWritten = map[string]bool{"": true, "thread_broadcast": true, "file_share": true, "me_message": true, "bot_message": true}

// slackTakesIn reports whether the message is someone's, not the channel's
// own or the bot's (the bot writes what the task's thread already has).
func (a *App) slackTakesIn(m slack.Message) bool {
	if !slackWritten[m.Subtype] {
		return false
	}
	return a.slackMe == nil || (m.User == "" || m.User != a.slackMe.UserID) && (m.BotID == "" || m.BotID != a.slackMe.BotID)
}

func (a *App) slackAuthor(ctx context.Context, m slack.Message) string {
	if m.User == "" {
		return "bot " + m.BotID
	}
	name, err := a.Slack.UserName(ctx, m.User)
	if err != nil {
		log.Printf("slack: user %s: %v", m.User, err)
		return m.User
	}
	return name
}

var slackRef = regexp.MustCompile(`<([@#!])?([^<>|]*)(?:\|([^<>]*))?>`)

// slackText is Slack's message markup as plain text: <@U…> as @Name, a
// channel as #name, a link as its URL after any label, and &amp; &lt; &gt; as
// the characters.
func (a *App) slackText(ctx context.Context, text string) string {
	text = slackRef.ReplaceAllStringFunc(text, func(ref string) string {
		m := slackRef.FindStringSubmatch(ref)
		kind, id, label := m[1], m[2], m[3]
		switch kind {
		case "@":
			if name, err := a.Slack.UserName(ctx, id); err == nil {
				return "@" + name
			}
			return "@" + id
		case "#":
			return "#" + cmp.Or(label, id)
		case "!":
			return "@" + cmp.Or(label, strings.TrimPrefix(id, "subteam^"))
		}
		if label == "" || label == id {
			return id
		}
		return label + " (" + id + ")"
	})
	return strings.NewReplacer("&lt;", "<", "&gt;", ">", "&amp;", "&").Replace(text)
}

// slackTS is t as a Slack ts.
func slackTS(t time.Time) string {
	return fmt.Sprintf("%d.%06d", t.Unix(), t.Nanosecond()/1000)
}

// tsCmp orders Slack timestamps ("seconds.micros") by time.
func tsCmp(x, y string) int {
	xs, xf, _ := strings.Cut(x, ".")
	ys, yf, _ := strings.Cut(y, ".")
	xi, _ := strconv.ParseInt(xs, 10, 64)
	yi, _ := strconv.ParseInt(ys, 10, 64)
	if xi != yi {
		if xi < yi {
			return -1
		}
		return 1
	}
	xf, yf = (xf + "000000")[:6], (yf + "000000")[:6]
	return strings.Compare(xf, yf)
}

// tsAfter reports whether ts is later than since (empty: always).
func tsAfter(ts, since string) bool { return since == "" || tsCmp(ts, since) > 0 }

// RunSlackSync polls every interval, and when kicked, until ctx is done.
func (a *App) RunSlackSync(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		if err := a.SyncSlack(ctx); err != nil && ctx.Err() == nil {
			log.Printf("slack: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-a.slackKick:
		}
	}
}
