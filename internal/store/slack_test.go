package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func slackRows(t *testing.T, s *Store) []string {
	t.Helper()
	rows, err := s.db.Query(`SELECT channel_id, ts, thread_ts, COALESCE(task_message_id, 0), last_reply_ts FROM slack_messages ORDER BY channel_id, ts`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var ch, ts, thread, last string
		var tm int64
		if err := rows.Scan(&ch, &ts, &thread, &tm, &last); err != nil {
			t.Fatal(err)
		}
		out = append(out, fmt.Sprintf("%s %s %s %d %s", ch, ts, thread, tm, last))
	}
	return out
}

func TestSlackChannels(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	task, _ := s.CreateTask(ctx, Task{Title: "t"})
	other, _ := s.CreateTask(ctx, Task{Title: "other"})
	sess, _ := s.CreateSession(ctx, Session{TaskID: task.ID, Title: "Orchestrator", Mode: "plan"})

	if _, err := s.SlackChannel(ctx, task.ID); err != ErrNotFound {
		t.Fatalf("channel before connecting: %v", err)
	}
	if _, err := s.ConnectSlack(ctx, SlackChannel{TaskID: 999, ChannelID: "C1", Cursor: "1.0"}); err != ErrNotFound {
		t.Fatalf("connect a missing task: %v", err)
	}
	c, err := s.ConnectSlack(ctx, SlackChannel{TaskID: task.ID, ChannelID: "C1", ChannelName: "help", SessionID: &sess.ID, Cursor: "100.000001"})
	if err != nil || c.ChannelName != "help" || *c.SessionID != sess.ID || c.Cursor != "100.000001" || c.ConnectedAt.IsZero() {
		t.Fatalf("connected = %+v, %v", c, err)
	}
	if _, err := s.ConnectSlack(ctx, SlackChannel{TaskID: other.ID, ChannelID: "C1", Cursor: "1.0"}); err != ErrChannelTaken {
		t.Fatalf("connect a taken channel: %v", err)
	}
	if err := s.SetSlackCursor(ctx, task.ID, "200.000002"); err != nil {
		t.Fatal(err)
	}
	if cs, err := s.SlackChannels(ctx); err != nil || len(cs) != 1 || cs[0].Cursor != "200.000002" {
		t.Fatalf("channels = %+v, %v", cs, err)
	}

	// Messages seen are recorded once; a message added only the first time.
	root := SlackMessage{ChannelID: "C1", TS: "300.000001", LastReplyTS: "300.000001"}
	x, err := s.AddSlackMessage(ctx, root, &TaskMessage{TaskID: task.ID, Author: "Ana", Body: "help?"})
	if err != nil || x == nil || x.Author != "Ana" || x.SessionID != nil || x.Body != "help?" {
		t.Fatalf("added = %+v, %v", x, err)
	}
	if again, err := s.AddSlackMessage(ctx, root, &TaskMessage{TaskID: task.ID, Author: "Ana", Body: "help?"}); err != nil || again != nil {
		t.Fatalf("added again = %+v, %v", again, err)
	}
	if own, err := s.AddSlackMessage(ctx, SlackMessage{ChannelID: "C1", TS: "301.000001"}, nil); err != nil || own != nil {
		t.Fatalf("own = %+v, %v", own, err)
	}
	reply, _ := s.AddSlackMessage(ctx, SlackMessage{ChannelID: "C1", TS: "302.000001", ThreadTS: root.TS}, &TaskMessage{TaskID: task.ID, Author: "Ivo", Body: "same"})
	if err := s.SetSlackLastReply(ctx, "C1", root.TS, "302.000001"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetSlackLastReply(ctx, "C1", "50.000001", "305.000001"); err != nil { // an old thread
		t.Fatal(err)
	}
	if xs, _ := s.TaskMessages(ctx, task.ID); len(xs) != 2 {
		t.Fatalf("thread = %+v", xs)
	}
	threads, err := s.SlackThreads(ctx, "C1")
	if err != nil || len(threads) != 3 || threads[root.TS].LastReplyTS != "302.000001" || *threads[root.TS].TaskMessageID != x.ID ||
		threads["301.000001"].TaskMessageID != nil || threads["50.000001"].LastReplyTS != "305.000001" {
		t.Fatalf("threads = %+v, %v", threads, err)
	}

	// A deleted thread message drops its reply's row and leaves its root's
	// thread followed.
	exec(t, s, fmt.Sprintf(`DELETE FROM task_messages WHERE id IN (%d, %d)`, x.ID, reply.ID))
	want := []string{"C1 300.000001  0 302.000001", "C1 301.000001  0 ", "C1 50.000001  0 305.000001"}
	if got := slackRows(t, s); !slices.Equal(got, want) {
		t.Fatalf("rows = %v, want %v", got, want)
	}

	// The orchestrator's deletion leaves the channel without one.
	exec(t, s, fmt.Sprintf(`DELETE FROM sessions WHERE id = %d`, sess.ID))
	if c, _ := s.SlackChannel(ctx, task.ID); c.SessionID != nil {
		t.Fatalf("channel = %+v", c)
	}

	// Disconnecting drops the messages seen; the thread keeps its messages.
	s.AddSlackMessage(ctx, SlackMessage{ChannelID: "C1", TS: "400.000001"}, &TaskMessage{TaskID: task.ID, Body: "kept"})
	if err := s.DisconnectSlack(ctx, task.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.DisconnectSlack(ctx, task.ID); err != ErrNotFound {
		t.Fatalf("disconnect again: %v", err)
	}
	if got := slackRows(t, s); len(got) != 0 {
		t.Fatalf("rows = %v", got)
	}
	if xs, _ := s.TaskMessages(ctx, task.ID); len(xs) != 1 || xs[0].Body != "kept" {
		t.Fatalf("thread = %+v", xs)
	}
	// Now free for another task.
	if _, err := s.ConnectSlack(ctx, SlackChannel{TaskID: other.ID, ChannelID: "C1", Cursor: "1.0"}); err != nil {
		t.Fatal(err)
	}
}

// Reconnecting a task to its channel only changes the name and orchestrator;
// to another channel it forgets the first one's messages.
func TestSlackReconnect(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	task, _ := s.CreateTask(ctx, Task{Title: "t"})
	sess, _ := s.CreateSession(ctx, Session{TaskID: task.ID, Title: "Orchestrator", Mode: "plan"})
	s.ConnectSlack(ctx, SlackChannel{TaskID: task.ID, ChannelID: "C1", Cursor: "1.0"})
	s.AddSlackMessage(ctx, SlackMessage{ChannelID: "C1", TS: "2.0"}, nil)
	s.SetSlackCursor(ctx, task.ID, "2.0")
	c, err := s.ConnectSlack(ctx, SlackChannel{TaskID: task.ID, ChannelID: "C1", ChannelName: "renamed", SessionID: &sess.ID, Cursor: "9.0"})
	if err != nil || c.Cursor != "2.0" || c.ChannelName != "renamed" || *c.SessionID != sess.ID || len(slackRows(t, s)) != 1 {
		t.Fatalf("reconnected = %+v, %v, rows %v", c, err, slackRows(t, s))
	}
	if c, err := s.ConnectSlack(ctx, SlackChannel{TaskID: task.ID, ChannelID: "C2", Cursor: "3.0"}); err != nil || c.ChannelID != "C2" {
		t.Fatalf("reconnected = %+v, %v", c, err)
	}
	if got := slackRows(t, s); len(got) != 0 {
		t.Fatalf("rows = %v", got)
	}
}

// Deleting a task drops its connection and messages seen, however it goes:
// also when the delete comes through sync.
func TestSlackGoesWithTheTask(t *testing.T) {
	for _, synced := range []bool{false, true} {
		t.Run(fmt.Sprint("synced=", synced), func(t *testing.T) {
			s := open(t)
			ctx := context.Background()
			task, _ := s.CreateTask(ctx, Task{Title: "t"})
			keep, _ := s.CreateTask(ctx, Task{Title: "keep"})
			s.ConnectSlack(ctx, SlackChannel{TaskID: task.ID, ChannelID: "C1", Cursor: "1.0"})
			s.ConnectSlack(ctx, SlackChannel{TaskID: keep.ID, ChannelID: "C2", Cursor: "1.0"})
			s.AddSlackMessage(ctx, SlackMessage{ChannelID: "C1", TS: "2.0"}, &TaskMessage{TaskID: task.ID, Body: "a"})
			s.AddSlackMessage(ctx, SlackMessage{ChannelID: "C2", TS: "2.0"}, &TaskMessage{TaskID: keep.ID, Body: "b"})
			if synced {
				err := s.ApplyChanges(ctx, []Change{{Table: "tasks", Key: json.RawMessage(fmt.Sprint("[", task.ID, "]")),
					ChangedAtMs: 1 << 50, NodeID: 2, Deleted: true}})
				if err != nil {
					t.Fatal(err)
				}
			} else if err := s.DeleteTask(ctx, task.ID); err != nil {
				t.Fatal(err)
			}
			if cs, _ := s.SlackChannels(ctx); len(cs) != 1 || cs[0].TaskID != keep.ID {
				t.Fatalf("channels = %+v", cs)
			}
			if got := slackRows(t, s); len(got) != 1 || !strings.HasPrefix(got[0], "C2 ") {
				t.Fatalf("rows = %v", got)
			}
		})
	}
}

// A thread message's author is synced with it, and the Slack tables are not.
func TestSlackSync(t *testing.T) {
	s := openSynced(t)
	ctx := context.Background()
	task, _ := s.CreateTask(ctx, Task{Title: "t"})
	s.ConnectSlack(ctx, SlackChannel{TaskID: task.ID, ChannelID: "C1", Cursor: "1.0"})
	x, _ := s.AddSlackMessage(ctx, SlackMessage{ChannelID: "C1", TS: "2.0"}, &TaskMessage{TaskID: task.ID, Author: "Ana", Body: "hi"})
	changes, err := s.ExportChanges(ctx, 0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, c := range changes {
		if strings.HasPrefix(c.Table, "slack_") {
			t.Fatalf("synced %s", c.Table)
		}
		if c.Table == "task_messages" {
			found = strings.Contains(string(c.Row), `"author":"Ana"`)
		}
	}
	if !found {
		t.Fatalf("changes = %+v", changes)
	}

	other := openSynced(t) // the task comes with the changes
	if err := other.ApplyChanges(ctx, changes); err != nil {
		t.Fatal(err)
	}
	if got, err := other.TaskMessage(ctx, x.ID); err != nil || got.Author != "Ana" {
		t.Fatalf("synced message = %+v, %v", got, err)
	}
}

// Upgrading keeps the threads, every message without an author, and starts
// with no channel connected.
func TestSlackMigrationKeepsData(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	n := migrationWith(t, "CREATE TABLE slack_channels")
	for _, m := range migrations[:n] {
		if _, err := db.Exec(m); err != nil {
			t.Fatal(err)
		}
	}
	for _, q := range []string{
		fmt.Sprintf(`PRAGMA user_version = %d`, n),
		`INSERT INTO tasks (id, title, lane_id, position, project_id, created_at, updated_at) VALUES (5, 'Ship it', 1, 0, 1, 0, 0)`,
		`INSERT INTO sessions (id, task_id, title, status, mode, created_at, updated_at) VALUES (7, 5, 'Guard', 'idle', 'plan', 0, 0)`,
		`INSERT INTO task_messages (id, task_id, session_id, body, question, created_at) VALUES (11, 5, 7, 'Done.', 0, 0)`,
		`INSERT INTO task_messages (id, task_id, session_id, body, question, created_at) VALUES (12, 5, NULL, 'Thanks', 0, 0)`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(q, err)
		}
	}
	db.Close()

	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	xs, err := s.TaskMessages(ctx, 5)
	if err != nil || len(xs) != 2 || xs[0].Body != "Done." || xs[0].SessionTitle != "Guard" || xs[1].Body != "Thanks" ||
		xs[0].Author != "" || xs[1].Author != "" {
		t.Fatalf("thread = %+v, %v", xs, err)
	}
	if cs, err := s.SlackChannels(ctx); err != nil || len(cs) != 0 {
		t.Fatalf("channels = %+v, %v", cs, err)
	}
	sid := int64(7)
	if _, err := s.ConnectSlack(ctx, SlackChannel{TaskID: 5, ChannelID: "C1", SessionID: &sid, Cursor: "1.0"}); err != nil {
		t.Fatal(err)
	}
}

// Upgrading keeps the Slack messages seen, none of them delivered to a
// session yet; then a delivered message gives its session's thread, until
// the session goes.
func TestSlackDeliveredMigrationKeepsData(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	n := migrationWith(t, "ADD COLUMN delivered_to")
	for _, m := range migrations[:n] {
		if _, err := db.Exec(m); err != nil {
			t.Fatal(err)
		}
	}
	for _, q := range []string{
		fmt.Sprintf(`PRAGMA user_version = %d`, n),
		`INSERT INTO tasks (id, title, lane_id, position, project_id, created_at, updated_at) VALUES (5, 'Support', 1, 0, 1, 0, 0)`,
		`INSERT INTO sessions (id, task_id, title, status, mode, created_at, updated_at) VALUES (7, 5, 'Orchestrator', 'idle', 'plan', 0, 0)`,
		`INSERT INTO task_messages (id, task_id, session_id, author, body, question, created_at) VALUES (11, 5, NULL, 'Ana', 'help', 0, 0)`,
		`INSERT INTO slack_channels (task_id, channel_id, channel_name, session_id, cursor, connected_at) VALUES (5, 'C1', 'help', 7, '10.0', 0)`,
		`INSERT INTO slack_messages (channel_id, ts, thread_ts, task_message_id, last_reply_ts) VALUES ('C1', '10.0', '', 11, '12.0')`,
		`INSERT INTO slack_messages (channel_id, ts, thread_ts, task_message_id, last_reply_ts) VALUES ('C1', '12.0', '10.0', NULL, '')`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(q, err)
		}
	}
	db.Close()

	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	if th, err := s.SlackThreads(ctx, "C1"); err != nil || len(th) != 1 || th["10.0"].LastReplyTS != "12.0" || *th["10.0"].TaskMessageID != 11 {
		t.Fatalf("threads = %+v, %v", th, err)
	}
	if ch, thread, err := s.SlackThreadOf(ctx, 11); err != nil || ch != "C1" || thread != "10.0" {
		t.Fatalf("thread of 11 = %q %q, %v", ch, thread, err)
	}
	if _, err := s.SlackLastThread(ctx, "C1", 7); !errors.Is(err, ErrNotFound) {
		t.Fatalf("last thread: %v", err)
	}
	if err := s.SetSlackDelivered(ctx, "C1", []string{"10.0", "12.0"}, 7); err != nil {
		t.Fatal(err)
	}
	if thread, err := s.SlackLastThread(ctx, "C1", 7); err != nil || thread != "10.0" {
		t.Fatalf("last thread = %q, %v", thread, err)
	}
	exec(t, s, `DELETE FROM sessions WHERE id = 7`)
	if _, err := s.SlackLastThread(ctx, "C1", 7); !errors.Is(err, ErrNotFound) {
		t.Fatalf("last thread after the session went: %v", err)
	}
}

// A message posted to Slack is recorded once, whether polling saw it first
// or not.
func TestAddSlackSent(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	task, _ := s.CreateTask(ctx, Task{Title: "Support"})
	if _, err := s.ConnectSlack(ctx, SlackChannel{TaskID: task.ID, ChannelID: "C1", Cursor: "1.0"}); err != nil {
		t.Fatal(err)
	}
	x, _ := s.AddTaskMessage(ctx, TaskMessage{TaskID: task.ID, Body: "news"})
	// Polling took the bot's message as seen, with no task message.
	if _, err := s.AddSlackMessage(ctx, SlackMessage{ChannelID: "C1", TS: "5.0", LastReplyTS: "5.0"}, nil); err != nil {
		t.Fatal(err)
	}
	for _, ts := range []string{"5.0", "6.0"} {
		if err := s.AddSlackSent(ctx, SlackMessage{ChannelID: "C1", TS: ts, TaskMessageID: &x.ID, LastReplyTS: ts}); err != nil {
			t.Fatal(err)
		}
	}
	th, err := s.SlackThreads(ctx, "C1")
	if err != nil || len(th) != 2 || th["5.0"].TaskMessageID == nil || *th["5.0"].TaskMessageID != x.ID || th["5.0"].LastReplyTS != "5.0" {
		t.Fatalf("threads = %+v, %v", th, err)
	}
	if _, thread, err := s.SlackThreadOf(ctx, x.ID); err != nil || thread != "5.0" {
		t.Fatalf("thread = %q, %v", thread, err)
	}
}
