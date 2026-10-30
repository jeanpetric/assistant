package app

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/jeanpetric/assistant/internal/slack"
	"github.com/jeanpetric/assistant/internal/store"
)

// sent are the bot's messages in the fake channel as "thread|text", a
// top-level one with no thread.
func (f *fakeSlack) sent() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, m := range f.msgs["C1"] {
		if m.User == botUser {
			out = append(out, m.ThreadTS+"|"+m.Text)
		}
	}
	return out
}

func (s slackSetup) post(t *testing.T, m store.TaskMessage, o PostOpts) PostedMessage {
	t.Helper()
	m.TaskID = s.task.ID
	x, err := s.a.PostTaskMessage(context.Background(), m, o)
	if err != nil {
		t.Fatal(err)
	}
	return x
}

// A session's message goes to the channel, at the top before any Slack
// message went to the session. Polling doesn't take it back in, and follows
// its thread: a reply comes in, and the session's next message goes there.
func TestSlackPostSessionMessage(t *testing.T) {
	s := newSlackApp(t)
	x := s.post(t, store.TaskMessage{SessionID: &s.sess.ID, Body: "Release is **out**: see [notes](https://x.io)."}, PostOpts{})
	sent := s.f.sent()
	if !slices.Equal(sent, []string{"|Release is **out**: see [notes](https://x.io)."}) {
		t.Fatalf("sent = %q", sent)
	}
	if x.Slack == nil || x.Slack.Error != "" || x.Slack.TS == "" || x.Slack.ThreadTS != "" || x.Slack.Channel != "C1" {
		t.Fatalf("slack = %+v", x.Slack)
	}
	s.sync(t)
	if got := s.thread(t); len(got) != 1 || len(s.prompts()) != 0 {
		t.Fatalf("thread = %v, prompts = %q", got, s.prompts())
	}

	s.f.post("C1", "UANA", "thanks, what about the API?", x.Slack.TS)
	s.sync(t)
	if got := s.thread(t); len(got) != 2 || got[1] != "Ana: thanks, what about the API?" || len(s.prompts()) != 1 {
		t.Fatalf("thread = %v, prompts = %q", got, s.prompts())
	}
	s.post(t, store.TaskMessage{SessionID: &s.sess.ID, Body: "Same release."}, PostOpts{})
	if sent := s.f.sent(); len(sent) != 2 || sent[1] != x.Slack.TS+"|Same release." {
		t.Fatalf("sent = %q", sent)
	}
	s.sync(t)
	if got := s.thread(t); len(got) != 3 || len(s.prompts()) != 1 {
		t.Fatalf("thread = %v, prompts = %q", got, s.prompts())
	}
	if st := s.a.SlackStatus(s.task.ID); st.PostError != "" || st.Error != "" {
		t.Fatalf("status = %+v", st)
	}
}

// By default a session answers in the thread of the latest Slack message
// that went to it; top_level and reply_to choose otherwise.
func TestSlackPostWhere(t *testing.T) {
	s := newSlackApp(t)
	ctx := context.Background()
	first := s.f.post("C1", "UANA", "export fails", "")
	s.sync(t)
	second := s.f.post("C1", "UIVO", "import too", "")
	s.f.post("C1", "UANA", "on staging", first)
	s.sync(t)
	xs, _ := s.a.Store.TaskMessages(ctx, s.task.ID)
	if len(xs) != 3 {
		t.Fatalf("thread = %v", s.thread(t))
	}

	// The latest delivered is the reply in the first thread.
	s.post(t, store.TaskMessage{SessionID: &s.sess.ID, Body: "Looking."}, PostOpts{})
	s.post(t, store.TaskMessage{SessionID: &s.sess.ID, Body: "Import: fixed."}, PostOpts{ReplyTo: xs[1].ID})
	s.post(t, store.TaskMessage{SessionID: &s.sess.ID, Body: "Staging: fixed."}, PostOpts{ReplyTo: xs[2].ID})
	s.post(t, store.TaskMessage{SessionID: &s.sess.ID, Body: "All fixed, everyone."}, PostOpts{TopLevel: true})
	want := []string{first + "|Looking.", second + "|Import: fixed.", first + "|Staging: fixed.", "|All fixed, everyone."}
	if sent := s.f.sent(); !slices.Equal(sent, want) {
		t.Fatalf("sent = %q, want %q", sent, want)
	}

	// Another session got no Slack message: the top.
	other, _ := s.a.Store.CreateSession(ctx, store.Session{TaskID: s.task.ID, Title: "Helper", Status: store.StatusIdle})
	s.post(t, store.TaskMessage{SessionID: &other.ID, Body: "Helper here."}, PostOpts{})
	if sent := s.f.sent(); sent[len(sent)-1] != "|Helper here." {
		t.Fatalf("sent = %q", sent)
	}

	// reply_to must be a Slack message of the task; nothing is posted otherwise.
	mine := s.post(t, store.TaskMessage{SessionID: &s.sess.ID, Body: "note"}, PostOpts{TopLevel: true})
	n := len(s.thread(t))
	other2, _ := s.a.Store.CreateTask(ctx, store.Task{Title: "Other"})
	foreign, _ := s.a.Store.AddTaskMessage(ctx, store.TaskMessage{TaskID: other2.ID, Body: "elsewhere"})
	bot, _ := s.a.Store.AddTaskMessage(ctx, store.TaskMessage{TaskID: s.task.ID, Body: "not in Slack"})
	n++
	for _, o := range []PostOpts{{ReplyTo: foreign.ID}, {ReplyTo: bot.ID}, {ReplyTo: 99999}, {ReplyTo: xs[0].ID, TopLevel: true}} {
		if _, err := s.a.PostTaskMessage(ctx, store.TaskMessage{TaskID: s.task.ID, SessionID: &s.sess.ID, Body: "x"}, o); !errors.Is(err, ErrBadMessage) {
			t.Errorf("%+v: err = %v", o, err)
		}
	}
	if len(s.thread(t)) != n {
		t.Fatalf("thread = %v", s.thread(t))
	}
	// A message the bot posted is one to reply to too.
	s.post(t, store.TaskMessage{SessionID: &s.sess.ID, Body: "more"}, PostOpts{ReplyTo: mine.ID})
	if sent := s.f.sent(); sent[len(sent)-1] != mine.Slack.TS+"|more" {
		t.Fatalf("sent = %q", sent)
	}
}

// The user's message from the assistant goes to the channel marked as
// theirs, at the top unless reply_to says otherwise.
func TestSlackPostUserMessage(t *testing.T) {
	s := newSlackApp(t)
	root := s.f.post("C1", "UANA", "status?", "")
	s.sync(t)
	xs, _ := s.a.Store.TaskMessages(context.Background(), s.task.ID)
	s.post(t, store.TaskMessage{Body: "On it."}, PostOpts{})
	s.post(t, store.TaskMessage{Body: "Done:\n- one\n- two"}, PostOpts{ReplyTo: xs[0].ID})
	want := []string{"|" + SlackFromUser + " On it.", root + "|" + SlackFromUser + "\nDone:\n- one\n- two"}
	if sent := s.f.sent(); !slices.Equal(sent, want) {
		t.Fatalf("sent = %q, want %q", sent, want)
	}
	s.sync(t)
	if got := s.thread(t); len(got) != 3 {
		t.Fatalf("thread = %v", got)
	}
}

// A message from Slack goes back neither when polled nor when posted with
// its author; a task with no channel posts nothing.
func TestSlackPostNotBack(t *testing.T) {
	s := newSlackApp(t)
	s.f.post("C1", "UANA", "hi", "")
	s.sync(t)
	x := s.post(t, store.TaskMessage{Author: "Ana", Body: "from Slack"}, PostOpts{})
	if s.f.count("chat.postMessage") != 0 || x.Slack != nil {
		t.Fatalf("calls = %v, slack = %+v", s.f.calls, x.Slack)
	}

	other, _ := s.a.Store.CreateTask(context.Background(), store.Task{Title: "Quiet"})
	x, err := s.a.PostTaskMessage(context.Background(), store.TaskMessage{TaskID: other.ID, Body: "local", Author: ""}, PostOpts{ReplyTo: 12345})
	if err != nil || x.Slack != nil || s.f.count("chat.postMessage") != 0 {
		t.Fatalf("x = %+v, %v; calls %v", x, err, s.f.calls)
	}
}

// A question shows as one in Slack.
func TestSlackPostQuestion(t *testing.T) {
	s := newSlackApp(t)
	s.post(t, store.TaskMessage{SessionID: &s.sess.ID, Body: "Roll back?", Question: true}, PostOpts{})
	if sent := s.f.sent(); !slices.Equal(sent, []string{"|❓ Roll back?"}) {
		t.Fatalf("sent = %q", sent)
	}
}

// A long message goes in parts: the first where it should, the rest in its
// thread, none of them taken back in.
func TestSlackPostLongMessage(t *testing.T) {
	s := newSlackApp(t)
	para := strings.Repeat("word ", 1000) // 5000 characters
	body := strings.TrimSpace(strings.Repeat(para+"\n\n", 6))
	x := s.post(t, store.TaskMessage{SessionID: &s.sess.ID, Body: body}, PostOpts{})
	sent := s.f.sent()
	if len(sent) != 3 {
		t.Fatalf("%d parts", len(sent))
	}
	var got []string
	for i, m := range sent {
		thread, text, _ := strings.Cut(m, "|")
		if len([]rune(text)) > slack.MaxMarkdown || (i == 0) != (thread == "") || (i > 0 && thread != x.Slack.TS) {
			t.Fatalf("part %d: thread %q, %d characters", i, thread, len(text))
		}
		got = append(got, text)
	}
	if strings.Join(got, "\n\n") != body {
		t.Fatal("the parts are not the message")
	}
	s.sync(t)
	if got := s.thread(t); len(got) != 1 || len(s.prompts()) != 0 {
		t.Fatalf("thread = %d, prompts = %q", len(got), s.prompts())
	}
}

func TestSplitSlack(t *testing.T) {
	for _, c := range []struct {
		text string
		max  int
		want []string
	}{
		{"short", 20, []string{"short"}},
		{"aaaa bbbb cccc dddd", 12, []string{"aaaa bbbb", "cccc dddd"}},
		{"one two\nthree four\n\nfive six", 22, []string{"one two\nthree four", "five six"}},
		{"abcdefghijklmnopqrstuvwxyz", 10, []string{"abcdefghij", "klmnopqrst", "uvwxyz"}},
		{"see:\n```\nline one\nline two\nline three\n```\nend", 30, []string{
			"see:\n```\nline one\nline two\n```", "```\nline three\n```\nend"}},
	} {
		got := splitSlack(c.text, c.max)
		if !slices.Equal(got, c.want) {
			t.Errorf("splitSlack(%q, %d) = %q, want %q", c.text, c.max, got, c.want)
		}
		for _, p := range got {
			if len([]rune(p)) > c.max {
				t.Errorf("part %q longer than %d", p, c.max)
			}
		}
	}
}

// A failed post leaves the message on the thread and shows in the task's
// Slack status; the user hears of the first failure in a row only.
func TestSlackPostFails(t *testing.T) {
	s := newSlackApp(t)
	s.f.fail["chat.postMessage"] = "is_archived"
	for i := range 2 {
		x := s.post(t, store.TaskMessage{SessionID: &s.sess.ID, Body: "news"}, PostOpts{})
		if x.ID == 0 || x.Slack == nil || !strings.Contains(x.Slack.Error, "is_archived") {
			t.Fatalf("%d: x = %+v, slack = %+v", i, x, x.Slack)
		}
	}
	if got := s.thread(t); len(got) != 2 {
		t.Fatalf("thread = %v", got)
	}
	if st := s.a.SlackStatus(s.task.ID); !strings.Contains(st.PostError, "is_archived") {
		t.Fatalf("status = %+v", st)
	}
	s.sync(t) // a poll keeps the post's error
	if st := s.a.SlackStatus(s.task.ID); st.PostError == "" {
		t.Fatalf("status = %+v", st)
	}
	want := []string{"Posting to Slack #help failed: slack: chat.postMessage: is_archived"}
	if got := notifications(t, s.a); !slices.Equal(got, want) {
		t.Fatalf("notifications = %q", got)
	}
	if n := s.f.count("chat.postMessage"); n != 2 {
		t.Fatalf("%d posts, want no retries", n)
	}

	delete(s.f.fail, "chat.postMessage")
	s.post(t, store.TaskMessage{SessionID: &s.sess.ID, Body: "back"}, PostOpts{})
	if st := s.a.SlackStatus(s.task.ID); st.PostError != "" {
		t.Fatalf("status = %+v", st)
	}
	s.f.fail["chat.postMessage"] = "is_archived"
	s.post(t, store.TaskMessage{SessionID: &s.sess.ID, Body: "again"}, PostOpts{})
	if got := notifications(t, s.a); len(got) != 2 {
		t.Fatalf("notifications = %q", got)
	}
}
