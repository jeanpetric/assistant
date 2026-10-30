package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jeanpetric/assistant/internal/slack"
	"github.com/jeanpetric/assistant/internal/store"
)

const (
	botUser = "UBOT"
	botID   = "BBOT"
)

// fakeSlack is a Slack workspace behind the Web API methods the polling uses.
type fakeSlack struct {
	mu       sync.Mutex
	channels map[string]string // id → name; the bot is in these
	others   map[string]string // channels the bot is not in
	msgs     map[string][]slack.Message
	users    map[string]string
	calls    map[string]int
	fail     map[string]string   // method → its ok:false error
	reacted  map[string][]string // "channel ts" → the bot's reactions
	next     int64               // the latest ts's seconds
}

func newFakeSlack(t *testing.T) (*fakeSlack, *slack.Client) {
	f := &fakeSlack{channels: map[string]string{"C1": "help"}, others: map[string]string{"C9": "random"},
		msgs: map[string][]slack.Message{}, users: map[string]string{"UANA": "Ana", "UIVO": "Ivo", botUser: "JeanBot"},
		calls: map[string]int{}, fail: map[string]string{}, reacted: map[string][]string{}, next: 1793351453}
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(srv.Close)
	return f, &slack.Client{Token: "xoxb-test", BaseURL: srv.URL}
}

// post adds a message to the channel, in the thread of thread unless empty,
// and returns its ts.
func (f *fakeSlack) post(channel, user, text, thread string) string {
	return f.add(channel, slack.Message{User: user, Text: text, ThreadTS: thread})
}

func (f *fakeSlack) add(channel string, m slack.Message) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.next++
	m.TS = fmt.Sprintf("%d.000100", f.next)
	if m.User == botUser {
		m.BotID = botID
	}
	f.msgs[channel] = append(f.msgs[channel], m)
	return m.TS
}

// reactions are the bot's reactions on the channel's message ts.
func (f *fakeSlack) reactions(channel, ts string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reacted[channel+" "+ts]
}

func (f *fakeSlack) count(method string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[method]
}

func (f *fakeSlack) resetCalls() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = map[string]int{}
}

func (f *fakeSlack) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r.ParseForm()
	method := strings.TrimPrefix(r.URL.Path, "/")
	f.calls[method]++
	reply := func(v map[string]any) {
		v["ok"] = true
		json.NewEncoder(w).Encode(v)
	}
	if e := f.fail[method]; e != "" {
		json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": e})
		return
	}
	ch := r.Form.Get("channel")
	oldest := r.Form.Get("oldest")
	switch method {
	case "auth.test":
		reply(map[string]any{"user_id": botUser, "bot_id": botID})
	case "users.info":
		u := r.Form.Get("user")
		reply(map[string]any{"user": map[string]any{"name": strings.ToLower(f.users[u]), "profile": map[string]any{"display_name": f.users[u]}}})
	case "conversations.info":
		if name, ok := f.channels[ch]; ok {
			reply(map[string]any{"channel": map[string]any{"id": ch, "name": name, "is_member": true}})
		} else if name, ok := f.others[ch]; ok {
			reply(map[string]any{"channel": map[string]any{"id": ch, "name": name, "is_member": false}})
		} else {
			json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "channel_not_found"})
		}
	case "conversations.history":
		var top []slack.Message
		for _, m := range f.msgs[ch] {
			if m.ThreadTS != "" && m.Subtype != "thread_broadcast" {
				continue
			}
			for _, x := range f.msgs[ch] {
				if x.ThreadTS == m.TS && x.Subtype != "thread_broadcast" {
					m.ThreadTS, m.ReplyCount, m.LatestReply = m.TS, m.ReplyCount+1, x.TS
				}
			}
			if oldest == "" || tsCmp(m.TS, oldest) > 0 {
				top = append(top, m)
			}
		}
		slices.Reverse(top) // newest first
		f.page(w, r, top, reply)
	case "conversations.replies":
		root := r.Form.Get("ts")
		var thread []slack.Message
		for _, m := range f.msgs[ch] {
			if m.TS == root {
				m.ThreadTS = root
				thread = append([]slack.Message{m}, thread...)
			} else if m.ThreadTS == root && (oldest == "" || tsCmp(m.TS, oldest) > 0) {
				thread = append(thread, m)
			}
		}
		f.page(w, r, thread, reply)
	case "chat.postMessage":
		f.next++
		m := slack.Message{TS: fmt.Sprintf("%d.000100", f.next), User: botUser, BotID: botID,
			Text: r.Form.Get("markdown_text"), ThreadTS: r.Form.Get("thread_ts")}
		f.msgs[ch] = append(f.msgs[ch], m)
		reply(map[string]any{"channel": ch, "ts": m.TS})
	case "reactions.add":
		ts, name, key := r.Form.Get("timestamp"), r.Form.Get("name"), ch+" "+r.Form.Get("timestamp")
		switch {
		case !slices.ContainsFunc(f.msgs[ch], func(m slack.Message) bool { return m.TS == ts }):
			json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "message_not_found"})
		case slices.Contains(f.reacted[key], name):
			json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "already_reacted"})
		default:
			f.reacted[key] = append(f.reacted[key], name)
			reply(map[string]any{})
		}
	default:
		http.Error(w, "unknown method", http.StatusNotFound)
	}
}

// page answers with the messages from the cursor's on, as many as limit.
func (f *fakeSlack) page(w http.ResponseWriter, r *http.Request, msgs []slack.Message, reply func(map[string]any)) {
	from, _ := strconv.Atoi(r.Form.Get("cursor"))
	n, _ := strconv.Atoi(r.Form.Get("limit"))
	msgs = msgs[min(from, len(msgs)):]
	next := ""
	if n > 0 && len(msgs) > n {
		msgs, next = msgs[:n], strconv.Itoa(from+n)
	}
	if msgs == nil {
		msgs = []slack.Message{}
	}
	reply(map[string]any{"messages": msgs, "response_metadata": map[string]any{"next_cursor": next}})
}

type slackSetup struct {
	a     *App
	r     *fakeRunner
	f     *fakeSlack
	task  store.Task
	sess  store.Session
	clock time.Time
}

// newSlackApp has a task with an orchestrator session that has run a turn,
// connected to channel C1 at a time before the fake's first message.
func newSlackApp(t *testing.T) slackSetup {
	t.Helper()
	a, r := newApp(t)
	f, c := newFakeSlack(t)
	a.Slack = c
	clock := time.Unix(1793351400, 0)
	a.Now = func() time.Time { return clock }
	ctx := context.Background()
	task, _ := a.Store.CreateTask(ctx, store.Task{Title: "Support"})
	sess, err := a.StartSession(ctx, StartOpts{TaskID: task.ID, Prompt: "orchestrate"})
	if err != nil {
		t.Fatal(err)
	}
	a.Wait()
	if _, err := a.ConnectSlack(ctx, task.ID, "https://x.slack.com/archives/C1", &sess.ID); err != nil {
		t.Fatal(err)
	}
	return slackSetup{a: a, r: r, f: f, task: task, sess: sess, clock: clock}
}

func (s slackSetup) sync(t *testing.T) {
	t.Helper()
	if err := s.a.SyncSlack(context.Background()); err != nil {
		t.Fatal(err)
	}
	s.a.Wait()
}

// thread lists the task's thread as "author: body".
func (s slackSetup) thread(t *testing.T) []string {
	t.Helper()
	xs, err := s.a.Store.TaskMessages(context.Background(), s.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	out := []string{}
	for _, x := range xs {
		out = append(out, x.Author+": "+x.Body)
	}
	return out
}

// prompts are what the orchestrator got after its first prompt.
func (s slackSetup) prompts() []string {
	var out []string
	for _, q := range s.r.requests()[1:] {
		out = append(out, q.Prompt)
	}
	return out
}

func notifications(t *testing.T, a *App) []string {
	t.Helper()
	ns, err := a.Store.Notifications(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, n := range ns {
		out = append(out, n.Message)
	}
	sort.Strings(out)
	return out
}

// A new top-level message comes to the task's thread under its author and
// goes to the orchestrator naming its task message, ts and thread; the
// channel's history from before connecting does not.
func TestSlackNewMessage(t *testing.T) {
	a, r := newApp(t)
	f, c := newFakeSlack(t)
	a.Slack = c
	ctx := context.Background()
	f.post("C1", "UANA", "before connecting", "")
	a.Now = func() time.Time { return time.Unix(f.next, 500_000) } // just after it
	task, _ := a.Store.CreateTask(ctx, store.Task{Title: "Support"})
	sess, _ := a.StartSession(ctx, StartOpts{TaskID: task.ID, Prompt: "orchestrate"})
	a.Wait()
	ch, err := a.ConnectSlack(ctx, task.ID, "C1", &sess.ID)
	if err != nil || ch.ChannelName != "help" || ch.Cursor != fmt.Sprintf("%d.000500", f.next) {
		t.Fatalf("connected = %+v, %v", ch, err)
	}
	s := slackSetup{a: a, r: r, f: f, task: task, sess: sess}

	ts := f.post("C1", "UANA", "hey <@UIVO>, the export &amp; import fail", "")
	s.sync(t)
	xs, _ := a.Store.TaskMessages(ctx, task.ID)
	if len(xs) != 1 || xs[0].Author != "Ana" || xs[0].Body != "hey @Ivo, the export & import fail" || xs[0].SessionID != nil {
		t.Fatalf("thread = %+v", xs)
	}
	want := fmt.Sprintf("[Slack #help] Ana — task_message %d, ts %s, thread %s: hey @Ivo, the export & import fail", xs[0].ID, ts, ts)
	if got := s.prompts(); !slices.Equal(got, []string{want}) {
		t.Fatalf("prompts = %q, want %q", got, want)
	}
	if st := a.SlackStatus(task.ID); st.LastSyncAt == nil || st.Error != "" || st.Undelivered != "" {
		t.Fatalf("status = %+v", st)
	}

	// Nothing new: nothing more, and no thread read.
	f.resetCalls()
	s.sync(t)
	if n := len(s.thread(t)); n != 1 || len(s.prompts()) != 1 || f.count("conversations.replies") != 0 || f.count("conversations.history") != 1 {
		t.Fatalf("thread %d, prompts %d, calls %v", n, len(s.prompts()), f.calls)
	}
}

// A reply in a thread seen before comes in; only threads with new replies
// are read.
func TestSlackReplyInThread(t *testing.T) {
	s := newSlackApp(t)
	f := s.f
	root := f.post("C1", "UANA", "export fails", "")
	quiet := f.post("C1", "UIVO", "unrelated", "")
	f.post("C1", "UIVO", "old reply", quiet)
	s.sync(t)
	if got := s.thread(t); !slices.Equal(got, []string{"Ana: export fails", "Ivo: unrelated", "Ivo: old reply"}) {
		t.Fatalf("thread = %v", got)
	}

	f.resetCalls()
	reply := f.post("C1", "UIVO", "same here", root)
	s.sync(t)
	if got := s.thread(t); len(got) != 4 || got[3] != "Ivo: same here" {
		t.Fatalf("thread = %v", got)
	}
	if n := f.count("conversations.replies"); n != 1 {
		t.Fatalf("%d replies calls, want only the changed thread's", n)
	}
	xs, _ := s.a.Store.TaskMessages(context.Background(), s.task.ID)
	want := fmt.Sprintf("[Slack #help] Ivo — task_message %d, ts %s, thread %s: same here", xs[3].ID, reply, root)
	if got := s.prompts(); len(got) != 2 || got[1] != want {
		t.Fatalf("prompts = %q, want last %q", got, want)
	}
	f.resetCalls()
	s.sync(t)
	if n := f.count("conversations.replies"); n != 0 || len(s.thread(t)) != 4 {
		t.Fatalf("replies calls %d, thread %v", n, s.thread(t))
	}
}

// All of a poll's new messages go to the orchestrator as one message, in
// the order they were written.
func TestSlackOneMessagePerPoll(t *testing.T) {
	s := newSlackApp(t)
	f := s.f
	root := f.post("C1", "UANA", "one", "")
	f.post("C1", "UIVO", "two", root)
	f.post("C1", "UIVO", "three", "")
	s.sync(t)
	got := s.prompts()
	if len(got) != 1 {
		t.Fatalf("prompts = %q", got)
	}
	lines := strings.Split(got[0], "\n")
	if len(lines) != 3 || !strings.HasSuffix(lines[0], ": one") || !strings.HasSuffix(lines[1], ": two") || !strings.HasSuffix(lines[2], ": three") ||
		!strings.Contains(lines[1], "thread "+root+":") {
		t.Fatalf("lines = %q", lines)
	}
}

// Each message handed to the orchestrator gets the bot's :eyes:, top-level
// and thread replies alike; one that only waits on the task's thread gets
// none.
func TestSlackReactsToDelivered(t *testing.T) {
	s := newSlackApp(t)
	f := s.f
	root := f.post("C1", "UANA", "export fails", "")
	reply := f.post("C1", "UIVO", "same here", root)
	s.sync(t)
	for _, ts := range []string{root, reply} {
		if got := f.reactions("C1", ts); !slices.Equal(got, []string{"eyes"}) {
			t.Fatalf("reactions on %s = %q", ts, got)
		}
	}
	if n := f.count("reactions.add"); n != 2 {
		t.Fatalf("%d reactions.add calls", n)
	}

	if _, err := s.a.ConnectSlack(context.Background(), s.task.ID, "C1", nil); err != nil {
		t.Fatal(err)
	}
	waiting := f.post("C1", "UANA", "anyone?", "")
	s.sync(t)
	if got := f.reactions("C1", waiting); got != nil {
		t.Fatalf("reactions on an undelivered message = %q", got)
	}
}

// Reactions failing (an app without reactions:write) don't stop the
// messages, the sync or its status; a reaction already there is no error.
func TestSlackReactionFailureIsIgnored(t *testing.T) {
	s := newSlackApp(t)
	f := s.f
	f.fail["reactions.add"] = "missing_scope"
	f.post("C1", "UANA", "one", "")
	f.post("C1", "UIVO", "two", "")
	s.sync(t)
	if len(s.prompts()) != 1 || len(s.thread(t)) != 2 {
		t.Fatalf("prompts = %q, thread = %v", s.prompts(), s.thread(t))
	}
	if n := f.count("reactions.add"); n != 1 {
		t.Fatalf("%d reactions.add calls, want one a poll", n)
	}
	if st := s.a.SlackStatus(s.task.ID); st.Error != "" || st.Undelivered != "" {
		t.Fatalf("status = %+v", st)
	}
	if got := notifications(t, s.a); len(got) != 0 {
		t.Fatalf("notifications = %q", got)
	}

	delete(f.fail, "reactions.add")
	ts := f.post("C1", "UANA", "three", "")
	f.mu.Lock()
	f.reacted["C1 "+ts] = []string{"eyes"}
	f.mu.Unlock()
	if err := s.a.Slack.AddReaction(context.Background(), "C1", ts, "eyes"); err != nil {
		t.Fatalf("already reacted: %v", err)
	}
}

// While the orchestrator works, the messages wait in its queue and start its
// next turn.
func TestSlackBusyOrchestratorQueues(t *testing.T) {
	s := newSlackApp(t)
	ctx := context.Background()
	s.r.block = make(chan struct{})
	if queued, err := s.a.SendMessage(ctx, s.sess.ID, "busy"); err != nil || queued {
		t.Fatalf("send: %v %v", queued, err)
	}
	s.f.post("C1", "UANA", "while busy", "")
	if err := s.a.SyncSlack(ctx); err != nil {
		t.Fatal(err)
	}
	sess, _ := s.a.Store.Session(ctx, s.sess.ID)
	if len(sess.Queued) != 1 || !strings.HasSuffix(sess.Queued[0].Content, ": while busy") {
		t.Fatalf("queue = %+v", sess.Queued)
	}
	close(s.r.block)
	s.a.Wait()
	if got := s.prompts(); len(got) != 2 || got[0] != "busy" || !strings.HasSuffix(got[1], ": while busy") {
		t.Fatalf("prompts = %q", got)
	}
}

// The bot's own messages and the channel's system messages stay out; a
// reply in a thread the bot started comes in.
func TestSlackSkipsOwnAndSystemMessages(t *testing.T) {
	s := newSlackApp(t)
	f := s.f
	own := f.post("C1", botUser, "posted from the task", "")
	f.add("C1", slack.Message{User: "UIVO", Subtype: "channel_join", Text: "<@UIVO> has joined the channel"})
	f.post("C1", botUser, "my own reply", own)
	f.post("C1", "UANA", "thanks", own)
	f.add("C1", slack.Message{User: "UANA", Subtype: "thread_broadcast", Text: "also to the channel", ThreadTS: own})
	s.sync(t)
	if got := s.thread(t); !slices.Equal(got, []string{"Ana: thanks", "Ana: also to the channel"}) {
		t.Fatalf("thread = %v", got)
	}
	if got := s.prompts(); len(got) != 1 || strings.Contains(got[0], "posted from") || strings.Contains(got[0], "joined") {
		t.Fatalf("prompts = %q", got)
	}
	// Nothing but the bot's own: nothing to pass on.
	f.post("C1", botUser, "more of mine", "")
	s.sync(t)
	if len(s.prompts()) != 1 || len(s.thread(t)) != 2 {
		t.Fatalf("prompts = %q, thread = %v", s.prompts(), s.thread(t))
	}
}

// After a pause, polling goes on from where it was and takes in all that
// came meanwhile, once.
func TestSlackResumesAfterPause(t *testing.T) {
	s := newSlackApp(t)
	f := s.f
	root := f.post("C1", "UANA", "first", "")
	s.sync(t)

	// The app is down: a new app on the same database polls next.
	restart := func() *App {
		a := New(s.a.Store, s.r, "/default/dir", "bypassPermissions")
		a.Slack, a.Now = s.a.Slack, s.a.Now
		return a
	}
	poll := func(a *App) {
		t.Helper()
		if err := a.SyncSlack(context.Background()); err != nil {
			t.Fatal(err)
		}
		a.Wait()
	}
	a2 := restart()
	f.post("C1", "UIVO", "reply meanwhile", root)
	f.post("C1", "UANA", "new meanwhile", "")
	poll(a2)
	if got := s.thread(t); !slices.Equal(got, []string{"Ana: first", "Ivo: reply meanwhile", "Ana: new meanwhile"}) {
		t.Fatalf("thread = %v", got)
	}
	poll(a2)
	if n := len(s.thread(t)); n != 3 {
		t.Fatalf("thread has %d after polling again", n)
	}

	// More new messages than one read of the history holds.
	a3 := restart()
	var many string
	for i := range slackHistory + 20 {
		ts := f.post("C1", "UANA", fmt.Sprint("m", i), "")
		if i == 5 {
			many = ts
		}
	}
	f.post("C1", "UIVO", "reply to m5", many)
	poll(a3)
	got := s.thread(t)
	if len(got) != 3+slackHistory+21 || got[3] != "Ana: m0" || got[len(got)-1] != "Ivo: reply to m5" {
		t.Fatalf("thread has %d: %v", len(got), got[3:6])
	}
	if p := s.prompts(); len(p) != 3 || strings.Count(p[2], "\n") != slackHistory+20 {
		t.Fatalf("%d prompts", len(p))
	}
	poll(a3)
	if n := len(s.thread(t)); n != 3+slackHistory+21 {
		t.Fatalf("thread has %d after polling again", n)
	}
}

// A disconnected channel is no longer polled; the session and thread stay.
func TestSlackDisconnect(t *testing.T) {
	s := newSlackApp(t)
	ctx := context.Background()
	s.f.post("C1", "UANA", "first", "")
	s.sync(t)
	if err := s.a.DisconnectSlack(ctx, s.task.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.a.DisconnectSlack(ctx, s.task.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("disconnect again: %v", err)
	}
	s.f.resetCalls()
	s.f.post("C1", "UANA", "after", "")
	s.sync(t)
	if s.f.count("conversations.history") != 0 || len(s.thread(t)) != 1 || len(s.prompts()) != 1 {
		t.Fatalf("calls %v, thread %v", s.f.calls, s.thread(t))
	}
	if _, err := s.a.Store.Session(ctx, s.sess.ID); err != nil {
		t.Fatal(err)
	}
}

// A deleted task's channel is no longer polled.
func TestSlackDeletedTask(t *testing.T) {
	s := newSlackApp(t)
	s.f.post("C1", "UANA", "first", "")
	s.sync(t)
	if err := s.a.Store.DeleteTask(context.Background(), s.task.ID); err != nil {
		t.Fatal(err)
	}
	s.f.resetCalls()
	s.f.post("C1", "UANA", "after", "")
	s.sync(t)
	if s.f.count("conversations.history") != 0 {
		t.Fatalf("calls %v", s.f.calls)
	}
	if cs, _ := s.a.Store.SlackChannels(context.Background()); len(cs) != 0 {
		t.Fatalf("channels = %+v", cs)
	}
}

// Without an orchestrator that can take them, the messages wait on the
// thread and the user hears of it once.
func TestSlackUndelivered(t *testing.T) {
	s := newSlackApp(t)
	ctx := context.Background()
	if _, err := s.a.ConnectSlack(ctx, s.task.ID, "C1", nil); err != nil {
		t.Fatal(err)
	}
	s.f.post("C1", "UANA", "anyone?", "")
	s.sync(t)
	s.f.post("C1", "UANA", "hello?", "")
	s.sync(t)
	if got := s.thread(t); len(got) != 2 || len(s.prompts()) != 0 {
		t.Fatalf("thread = %v, prompts = %q", got, s.prompts())
	}
	want := []string{"Messages from Slack #help wait on the task's thread: the task has no orchestrator session"}
	if got := notifications(t, s.a); !slices.Equal(got, want) {
		t.Fatalf("notifications = %q", got)
	}
	if st := s.a.SlackStatus(s.task.ID); st.Undelivered == "" {
		t.Fatalf("status = %+v", st)
	}

	// Another node's session can't take them either.
	makeForeign(t, s.a.Store, s.sess)
	if _, err := s.a.ConnectSlack(ctx, s.task.ID, "C1", &s.sess.ID); err != nil {
		t.Fatal(err)
	}
	s.f.post("C1", "UANA", "still there?", "")
	s.sync(t)
	got := notifications(t, s.a)
	if len(got) != 2 || !strings.Contains(strings.Join(got, "|"), "session 1 can't take them: session runs on office-mac") || len(s.thread(t)) != 3 || len(s.prompts()) != 0 {
		t.Fatalf("notifications = %q, thread = %v", got, s.thread(t))
	}
}

// A failing channel notifies once in a row, and polling the others goes on.
func TestSlackFailureNotifiesOnce(t *testing.T) {
	s := newSlackApp(t)
	ctx := context.Background()
	s.f.fail["conversations.history"] = "internal_error"
	for range 2 {
		if err := s.a.SyncSlack(ctx); err == nil || !strings.Contains(err.Error(), "internal_error") {
			t.Fatalf("sync: %v", err)
		}
	}
	if got := notifications(t, s.a); len(got) != 1 || !strings.HasPrefix(got[0], "Slack polling of #help failed") {
		t.Fatalf("notifications = %q", got)
	}
	if st := s.a.SlackStatus(s.task.ID); !strings.Contains(st.Error, "internal_error") {
		t.Fatalf("status = %+v", st)
	}
	delete(s.f.fail, "conversations.history")
	s.f.post("C1", "UANA", "back", "")
	s.sync(t)
	if st := s.a.SlackStatus(s.task.ID); st.Error != "" || len(s.thread(t)) != 1 {
		t.Fatalf("status = %+v, thread = %v", st, s.thread(t))
	}
}

func TestConnectSlackChecksTheChannel(t *testing.T) {
	s := newSlackApp(t)
	ctx := context.Background()
	other, _ := s.a.Store.CreateTask(ctx, store.Task{Title: "other"})
	if _, err := s.a.ConnectSlack(ctx, other.ID, "C9", nil); !errors.Is(err, slack.ErrNotInChannel) || !strings.Contains(err.Error(), "#random") {
		t.Fatalf("connect a channel without the bot: %v", err)
	}
	if _, err := s.a.ConnectSlack(ctx, other.ID, "C404", nil); !errors.Is(err, slack.ErrChannelNotFound) {
		t.Fatalf("connect a missing channel: %v", err)
	}
	if _, err := s.a.ConnectSlack(ctx, other.ID, "C1", nil); !errors.Is(err, store.ErrChannelTaken) {
		t.Fatalf("connect a taken channel: %v", err)
	}
	if _, err := s.a.ConnectSlack(ctx, other.ID, "C9", &s.sess.ID); !errors.Is(err, ErrNotOnTask) {
		t.Fatalf("connect with another task's session: %v", err)
	}
	s.a.Slack = nil
	if _, err := s.a.ConnectSlack(ctx, other.ID, "C1", nil); !errors.Is(err, ErrSlackOff) {
		t.Fatalf("connect with Slack off: %v", err)
	}
}

// A hub only shares the database: it polls nothing.
func TestSlackHubDoesNotPoll(t *testing.T) {
	s := newSlackApp(t)
	s.a.SyncRole = RoleHub
	s.f.resetCalls()
	s.f.post("C1", "UANA", "hi", "")
	s.sync(t)
	if len(s.f.calls) != 0 || len(s.thread(t)) != 0 {
		t.Fatalf("calls %v", s.f.calls)
	}
}

func TestSlackTimestampsAndText(t *testing.T) {
	for _, c := range []struct {
		x, y string
		want int
	}{{"999.900000", "1000.000001", -1}, {"1000.5", "1000.400000", 1}, {"1000.000100", "1000.0001", 0}} {
		if got := tsCmp(c.x, c.y); got != c.want {
			t.Errorf("tsCmp(%s, %s) = %d", c.x, c.y, got)
		}
	}
	if got := slackTS(time.Unix(1793351453, 822019_000)); got != "1793351453.822019" {
		t.Errorf("slackTS = %s", got)
	}
	a, _ := newApp(t)
	_, a.Slack = newFakeSlack(t)
	got := a.slackText(context.Background(), "<@UANA> see <#C1|help>, <https://x.io/a?b=1&amp;c=2|the doc> or <https://y.io> &lt;now&gt; <!here>")
	if want := "@Ana see #help, the doc (https://x.io/a?b=1&c=2) or https://y.io <now> @here"; got != want {
		t.Errorf("slackText = %q, want %q", got, want)
	}
}

// Only the orchestrator of a connected task's channel is told how to
// orchestrate, after the ActivityNote: not another session of the task, nor
// it once the task is disconnected.
func TestOrchestratorNote(t *testing.T) {
	s := newSlackApp(t)
	ctx := context.Background()
	other, err := s.a.StartSession(ctx, StartOpts{TaskID: s.task.ID, Prompt: "something else"})
	if err != nil {
		t.Fatal(err)
	}
	s.a.Wait()
	s.f.post("C1", "UANA", "export fails", "")
	s.sync(t)
	s.a.DisconnectSlack(ctx, s.task.ID)
	s.a.SendMessage(ctx, s.sess.ID, "and now?")
	s.a.Wait()

	activity := func(id int64) string { return fmt.Sprintf(ActivityNote, s.task.ID, id) }
	note := fmt.Sprintf(OrchestratorNote, "help", s.task.ProjectID, s.task.ID, s.sess.ID)
	reqs := s.r.requests()
	want := []string{activity(s.sess.ID), activity(other.ID), activity(s.sess.ID) + "\n\n" + note, activity(s.sess.ID)}
	if len(reqs) != len(want) {
		t.Fatalf("requests = %+v", reqs)
	}
	for i, w := range want {
		if !strings.HasSuffix(reqs[i].SystemPrompt, w) || strings.Count(reqs[i].SystemPrompt, "orchestrator of this task's Slack channel") != strings.Count(w, "orchestrator of this task's Slack channel") {
			t.Errorf("turn %d system prompt = %q, want it to end in %q", i, reqs[i].SystemPrompt, w)
		}
	}
	if !strings.Contains(note, "#help") || !strings.Contains(note, fmt.Sprintf("report_to %d", s.sess.ID)) || !strings.Contains(note, fmt.Sprintf("context_from [%d]", s.task.ID)) {
		t.Errorf("note = %q", note)
	}
}

// A task connected without a session gets an orchestrator, started with the
// OrchestratorStart prompt, which takes the channel's messages; a channel it
// can't have starts none.
func TestConnectSlackStarting(t *testing.T) {
	a, r := newApp(t)
	f, c := newFakeSlack(t)
	a.Slack = c
	a.Now = func() time.Time { return time.Unix(1793351400, 0) }
	ctx := context.Background()
	task, _ := a.Store.CreateTask(ctx, store.Task{Title: "Support"})

	if _, _, err := a.ConnectSlackStarting(ctx, task.ID, "C9"); !errors.Is(err, slack.ErrNotInChannel) {
		t.Fatalf("not in channel: %v", err)
	}
	if ss, _ := a.Store.Sessions(ctx, task.ID, ""); len(ss) != 0 {
		t.Fatalf("sessions = %+v", ss)
	}

	ch, sess, err := a.ConnectSlackStarting(ctx, task.ID, "C1")
	if err != nil || ch.SessionID == nil || *ch.SessionID != sess.ID || sess.TaskID != task.ID || sess.Title != "Slack #help" {
		t.Fatalf("connected = %+v, %+v, %v", ch, sess, err)
	}
	a.Wait()
	f.post("C1", "UANA", "export fails", "")
	if err := a.SyncSlack(ctx); err != nil {
		t.Fatal(err)
	}
	a.Wait()
	reqs := r.requests()
	if len(reqs) != 2 || reqs[0].Prompt != fmt.Sprintf(OrchestratorStart, "help") || !strings.Contains(reqs[1].Prompt, "export fails") ||
		!strings.HasSuffix(reqs[1].SystemPrompt, fmt.Sprintf(OrchestratorNote, "help", task.ProjectID, task.ID, sess.ID)) {
		t.Fatalf("requests = %+v", reqs)
	}
}
