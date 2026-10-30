package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jeanpetric/assistant/internal/slack"
	"github.com/jeanpetric/assistant/internal/store"
)

// fakeSlack is a workspace with channels the bot is in and one it is not;
// nothing is ever posted in them.
type fakeSlack struct {
	historyErr error
}

var slackChannels = map[string]slack.Channel{
	"C1": {ID: "C1", Name: "help", IsMember: true},
	"C2": {ID: "C2", Name: "ops", IsMember: true},
	"C9": {ID: "C9", Name: "random"},
}

func (fakeSlack) AuthTest(context.Context) (slack.Identity, error) {
	return slack.Identity{UserID: "UBOT", BotID: "BBOT", URL: "https://acme.slack.com/"}, nil
}

func (fakeSlack) ConversationInfo(_ context.Context, channel string) (slack.Channel, error) {
	c, ok := slackChannels[channel]
	if !ok {
		return c, fmt.Errorf("slack: conversations.info: %w", slack.ErrChannelNotFound)
	}
	return c, nil
}

func (f fakeSlack) History(context.Context, string, string, int) ([]slack.Message, error) {
	return nil, f.historyErr
}

func (fakeSlack) Replies(context.Context, string, string, string) ([]slack.Message, error) {
	return nil, nil
}

func (fakeSlack) PostMessage(context.Context, string, string, string) (string, error) {
	return "1793351453.000100", nil
}

func (fakeSlack) UserName(context.Context, string) (string, error) { return "Ana", nil }

type slackTaskResp struct {
	ID    int64            `json:"id"`
	Slack *store.TaskSlack `json:"slack"`
}

// foreign makes the session one another node runs.
func foreign(t *testing.T, c *client, sess store.Session) {
	t.Helper()
	row, _ := json.Marshal(map[string]any{"id": sess.ID, "task_id": sess.TaskID, "status": sess.Status, "mode": sess.Mode,
		"created_at": sess.CreatedAt.UnixMilli(), "updated_at": sess.UpdatedAt.UnixMilli(), "node_id": 2, "node_name": "office-mac"})
	if err := c.app.Store.ApplyChanges(context.Background(), []store.Change{{Table: "sessions", Key: json.RawMessage(fmt.Sprintf("[%d]", sess.ID)),
		ChangedAtMs: time.Now().Add(time.Hour).UnixMilli(), NodeID: 2, Row: row}}); err != nil {
		t.Fatal(err)
	}
}

// A task connects to a channel by id or link, its orchestrator the session
// named or its latest one here; the task's JSON shows the connection and how
// polling goes until it disconnects, which leaves the sessions alone.
func TestSlackConnectAPI(t *testing.T) {
	c := newClient(t)
	ctx := context.Background()
	var task, other store.Task
	c.do("POST", "/api/tasks", map[string]any{"title": "t"}, 201, &task)
	c.do("POST", "/api/tasks", map[string]any{"title": "other"}, 201, &other)
	path := "/api/tasks/" + itoa(task.ID) + "/slack"

	// Without a token: refused, whatever else.
	var e struct{ Error string }
	c.do("POST", path, map[string]any{"channel": "C1"}, 409, &e)
	if !strings.Contains(e.Error, "Slack is off") {
		t.Fatalf("no token: %q", e.Error)
	}
	c.do("POST", "/api/tasks/999/slack", map[string]any{"channel": "C1"}, 404, nil)
	c.do("DELETE", "/api/tasks/999/slack", nil, 404, nil)

	c.app.Slack = fakeSlack{}
	c.do("POST", path, map[string]any{"channel": " "}, 400, nil)
	// No session to take the messages yet.
	c.do("POST", path, map[string]any{"channel": "C1"}, 409, &e)
	if !strings.Contains(e.Error, "no session on this computer") {
		t.Fatalf("no session: %q", e.Error)
	}
	older, _ := c.app.Store.CreateSession(ctx, store.Session{TaskID: task.ID, Title: "older"})
	latest, _ := c.app.Store.CreateSession(ctx, store.Session{TaskID: task.ID, Title: "latest"})
	elsewhere, _ := c.app.Store.CreateSession(ctx, store.Session{TaskID: other.ID, Title: "elsewhere"})

	c.do("POST", path, map[string]any{"channel": "C9"}, 400, &e)
	if !strings.Contains(e.Error, "bot is not in #random") {
		t.Fatalf("bot not in channel: %q", e.Error)
	}
	c.do("POST", path, map[string]any{"channel": "C404"}, 400, &e)
	if !strings.Contains(e.Error, "not found") {
		t.Fatalf("no channel: %q", e.Error)
	}
	c.do("POST", path, map[string]any{"channel": "C1", "session_id": elsewhere.ID}, 400, nil)
	c.do("POST", path, map[string]any{"channel": "C1", "session_id": 999}, 400, nil)

	var got slackTaskResp
	c.do("POST", path, map[string]any{"channel": "https://acme.slack.com/archives/C1"}, 200, &got)
	if s := got.Slack; s == nil || s.ChannelID != "C1" || s.ChannelName != "help" || s.SessionID == nil || *s.SessionID != latest.ID ||
		s.URL != "https://slack.com/app_redirect?channel=C1" || s.LastSyncAt != nil || s.Error != "" {
		t.Fatalf("connected = %+v", got.Slack)
	}
	// Another task can't take the channel.
	c.app.Store.CreateSession(ctx, store.Session{TaskID: other.ID, Title: "s"})
	c.do("POST", "/api/tasks/"+itoa(other.ID)+"/slack", map[string]any{"channel": "C1"}, 409, nil)

	// A poll: the link goes to the workspace, the task shows when it went.
	if err := c.app.SyncSlack(ctx); err != nil {
		t.Fatal(err)
	}
	var page struct{ Task slackTaskResp }
	c.do("GET", "/api/tasks/"+itoa(task.ID), nil, 200, &page)
	if s := page.Task.Slack; s == nil || s.URL != "https://acme.slack.com/archives/C1" || s.LastSyncAt == nil || s.Error != "" {
		t.Fatalf("after a poll = %+v", page.Task.Slack)
	}
	c.app.Slack = fakeSlack{historyErr: errors.New("slack: conversations.history: ratelimited")}
	c.app.SyncSlack(ctx)
	var b struct{ Tasks []slackTaskResp }
	c.do("GET", "/api/board", nil, 200, &b)
	var onBoard *store.TaskSlack
	for _, x := range b.Tasks {
		if x.ID == task.ID {
			onBoard = x.Slack
		} else if x.Slack != nil {
			t.Fatalf("task %d has slack %+v", x.ID, x.Slack)
		}
	}
	if onBoard == nil || onBoard.ChannelName != "help" || !strings.Contains(onBoard.Error, "ratelimited") {
		t.Fatalf("on the board = %+v", onBoard)
	}

	// Another channel and orchestrator in its place.
	c.do("POST", path, map[string]any{"channel": "C2", "session_id": older.ID}, 200, &got)
	if s := got.Slack; s == nil || s.ChannelID != "C2" || *s.SessionID != older.ID || s.Error != "" {
		t.Fatalf("reconnected = %+v", got.Slack)
	}
	// Another computer's session can't be it.
	foreign(t, c, older)
	c.do("POST", path, map[string]any{"channel": "C2", "session_id": older.ID}, 409, &e)
	if !strings.Contains(e.Error, "office-mac") {
		t.Fatalf("foreign session: %q", e.Error)
	}

	c.do("DELETE", path, nil, 200, &got)
	if got.Slack != nil {
		t.Fatalf("disconnected = %+v", got.Slack)
	}
	if _, err := c.app.Store.SlackChannel(ctx, task.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("connection left: %v", err)
	}
	if ss, _ := c.app.Store.Sessions(ctx, task.ID, ""); len(ss) != 2 {
		t.Fatalf("sessions = %d", len(ss))
	}
	c.do("DELETE", path, nil, 200, nil) // not connected: nothing to do
}

// The MCP tools connect and disconnect as the API does, errors as tool errors.
func TestSlackConnectMCP(t *testing.T) {
	c := newClient(t)
	ctx := context.Background()
	var task store.Task
	c.do("POST", "/api/tasks", map[string]any{"title": "t"}, 201, &task)

	if text, isErr := c.tool("connect_slack_channel", map[string]any{"task_id": task.ID, "channel": "C1"}); !isErr || !strings.Contains(text, "Slack is off") {
		t.Fatalf("no token: %v %s", isErr, text)
	}
	c.app.Slack = fakeSlack{}
	if text, isErr := c.tool("connect_slack_channel", map[string]any{"task_id": 999, "channel": "C1"}); !isErr {
		t.Fatalf("no task: %s", text)
	}
	if text, isErr := c.tool("connect_slack_channel", map[string]any{"task_id": task.ID, "channel": "C1"}); !isErr || !strings.Contains(text, "start one first") {
		t.Fatalf("no session: %v %s", isErr, text)
	}
	sess, _ := c.app.Store.CreateSession(ctx, store.Session{TaskID: task.ID, Title: "orchestrator"})
	if text, isErr := c.tool("connect_slack_channel", map[string]any{"task_id": task.ID, "channel": "C9"}); !isErr || !strings.Contains(text, "invite it") {
		t.Fatalf("bot not in channel: %v %s", isErr, text)
	}
	text, isErr := c.tool("connect_slack_channel", map[string]any{"task_id": task.ID, "channel": "C1", "session_id": sess.ID})
	var got slackTaskResp
	if isErr || json.Unmarshal([]byte(text), &got) != nil || got.Slack == nil || got.Slack.ChannelName != "help" || *got.Slack.SessionID != sess.ID {
		t.Fatalf("connect: %v %s", isErr, text)
	}
	text, _ = c.tool("get_task", map[string]any{"task_id": task.ID})
	if !strings.Contains(text, `"channel_name":"help"`) {
		t.Fatalf("get_task: %s", text)
	}
	text, isErr = c.tool("disconnect_slack_channel", map[string]any{"task_id": task.ID})
	if isErr || json.Unmarshal([]byte(text), &got) != nil || got.Slack != nil {
		t.Fatalf("disconnect: %v %s", isErr, text)
	}
	if _, isErr := c.tool("disconnect_slack_channel", map[string]any{"task_id": 999}); !isErr {
		t.Fatal("disconnect of no task")
	}
}
