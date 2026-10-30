package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// ErrChannelTaken: the Slack channel is connected to another task.
var ErrChannelTaken = errors.New("the Slack channel is connected to another task")

// SlackChannel is a task's connection to a Slack channel, whose messages
// come to the task's thread and go to its orchestrator session. It is this
// computer's (never synced).
type SlackChannel struct {
	TaskID      int64  `json:"task_id"`
	ChannelID   string `json:"channel_id"`
	ChannelName string `json:"channel_name"`
	SessionID   *int64 `json:"session_id"` // the orchestrator; nil: none
	// Cursor is the ts of the latest top-level message seen.
	Cursor      string    `json:"cursor"`
	ConnectedAt time.Time `json:"connected_at"`
}

// SlackMessage is a message of a connected channel already taken in: on the
// task's thread as TaskMessageID (nil: deleted there, or never there, like
// the bot's own). A top-level message has no ThreadTS and LastReplyTS is its
// thread's latest reply seen; a reply's ThreadTS is its thread's root.
type SlackMessage struct {
	ChannelID     string `json:"channel_id"`
	TS            string `json:"ts"`
	ThreadTS      string `json:"thread_ts"`
	TaskMessageID *int64 `json:"task_message_id"`
	LastReplyTS   string `json:"last_reply_ts"`
}

const slackChannelCols = `task_id, channel_id, channel_name, session_id, cursor, connected_at FROM slack_channels`

func scanSlackChannel(sc interface{ Scan(...any) error }) (SlackChannel, error) {
	var c SlackChannel
	var sess sql.NullInt64
	var at int64
	err := sc.Scan(&c.TaskID, &c.ChannelID, &c.ChannelName, &sess, &c.Cursor, &at)
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrNotFound
	}
	c.SessionID, c.ConnectedAt = fromNullID(sess), fromMillis(at)
	return c, err
}

// ConnectSlack connects the task to the channel, in place of any channel it
// was connected to: a channel is connected to one task at most. Connected
// to it already, the task only takes c's name and orchestrator: polling goes
// on from where it was.
func (s *Store) ConnectSlack(ctx context.Context, c SlackChannel) (SlackChannel, error) {
	if _, err := s.Task(ctx, c.TaskID); err != nil {
		return c, err
	}
	err := s.tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE slack_channels SET channel_name = ?, session_id = ? WHERE task_id = ? AND channel_id = ?`,
			c.ChannelName, c.SessionID, c.TaskID, c.ChannelID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n > 0 {
			return nil
		}
		// Another channel's connection goes with the messages seen there.
		if _, err := tx.ExecContext(ctx, `DELETE FROM slack_channels WHERE task_id = ?`, c.TaskID); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO slack_channels (task_id, channel_id, channel_name, session_id, cursor, connected_at)
			VALUES (?, ?, ?, ?, ?, ?)`, c.TaskID, c.ChannelID, c.ChannelName, c.SessionID, c.Cursor, s.ts())
		if err != nil && uniqueFailure(err) == "slack_channels.channel_id" {
			return ErrChannelTaken
		}
		return err
	})
	if err != nil {
		return c, err
	}
	return s.SlackChannel(ctx, c.TaskID)
}

// DisconnectSlack ends the task's connection to its channel; ErrNotFound if
// it has none. The thread keeps the messages taken in.
func (s *Store) DisconnectSlack(ctx context.Context, taskID int64) error {
	return affected(s.db.ExecContext(ctx, `DELETE FROM slack_channels WHERE task_id = ?`, taskID))
}

// SlackChannel is the task's connection; ErrNotFound if it has none.
func (s *Store) SlackChannel(ctx context.Context, taskID int64) (SlackChannel, error) {
	return scanSlackChannel(s.db.QueryRowContext(ctx, `SELECT `+slackChannelCols+` WHERE task_id = ?`, taskID))
}

// SlackChannels lists the connections in the order they were made.
func (s *Store) SlackChannels(ctx context.Context) ([]SlackChannel, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+slackChannelCols+` ORDER BY connected_at, task_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []SlackChannel{}
	for rows.Next() {
		c, err := scanSlackChannel(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// SetSlackCursor moves the task's channel past the top-level messages up to ts.
func (s *Store) SetSlackCursor(ctx context.Context, taskID int64, ts string) error {
	return affected(s.db.ExecContext(ctx, `UPDATE slack_channels SET cursor = ? WHERE task_id = ?`, ts, taskID))
}

// SlackThreads gives the channel's top-level messages seen, by ts.
func (s *Store) SlackThreads(ctx context.Context, channelID string) (map[string]SlackMessage, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT channel_id, ts, thread_ts, task_message_id, last_reply_ts
		FROM slack_messages WHERE channel_id = ? AND thread_ts = ''`, channelID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]SlackMessage{}
	for rows.Next() {
		var m SlackMessage
		var tm sql.NullInt64
		if err := rows.Scan(&m.ChannelID, &m.TS, &m.ThreadTS, &tm, &m.LastReplyTS); err != nil {
			return nil, err
		}
		m.TaskMessageID = fromNullID(tm)
		out[m.TS] = m
	}
	return out, rows.Err()
}

// AddSlackMessage records m as seen and, with x, adds x to its task's thread
// as m's message. It returns the message added, or nil when m was seen
// already (x is not added then) or there is no x.
func (s *Store) AddSlackMessage(ctx context.Context, m SlackMessage, x *TaskMessage) (*TaskMessage, error) {
	var id int64
	err := s.tx(ctx, func(tx *sql.Tx) error {
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM slack_messages WHERE channel_id = ? AND ts = ?`,
			m.ChannelID, m.TS).Scan(&n); err != nil || n > 0 {
			return err
		}
		var tm *int64
		if x != nil {
			var err error
			id, err = s.insertTx(ctx, tx, "task_messages", `INSERT INTO task_messages (id, task_id, session_id, author, body, question, created_at)
				VALUES (?, ?, ?, ?, ?, ?, ?)`, x.TaskID, x.SessionID, x.Author, x.Body, boolInt(x.Question), s.ts())
			if err != nil {
				return err
			}
			tm = &id
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO slack_messages (channel_id, ts, thread_ts, task_message_id, last_reply_ts) VALUES (?, ?, ?, ?, ?)`,
			m.ChannelID, m.TS, m.ThreadTS, tm, m.LastReplyTS)
		return err
	})
	if err != nil || id == 0 {
		return nil, err
	}
	added, err := s.TaskMessage(ctx, id)
	return &added, err
}

// SetSlackLastReply records the replies of the channel's thread rooted at ts
// seen up to lastReply, the root seen too if it was not.
func (s *Store) SetSlackLastReply(ctx context.Context, channelID, ts, lastReply string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO slack_messages (channel_id, ts, last_reply_ts) VALUES (?, ?, ?)
		ON CONFLICT (channel_id, ts) DO UPDATE SET last_reply_ts = excluded.last_reply_ts`, channelID, ts, lastReply)
	return err
}

// AddSlackSent records a message posted to Slack for the task message
// m.TaskMessageID, so polling follows its replies and doesn't take it in.
// Polling may have recorded it already, as the bot's own message.
func (s *Store) AddSlackSent(ctx context.Context, m SlackMessage) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO slack_messages (channel_id, ts, thread_ts, task_message_id, last_reply_ts) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (channel_id, ts) DO UPDATE SET thread_ts = excluded.thread_ts, task_message_id = excluded.task_message_id`,
		m.ChannelID, m.TS, m.ThreadTS, m.TaskMessageID, m.LastReplyTS)
	return err
}

// SlackThreadOf gives the channel and Slack thread (its root's ts) of the
// task message's first Slack message; ErrNotFound if it has none.
func (s *Store) SlackThreadOf(ctx context.Context, taskMessageID int64) (channelID, thread string, err error) {
	err = s.db.QueryRowContext(ctx, `SELECT channel_id, CASE thread_ts WHEN '' THEN ts ELSE thread_ts END FROM slack_messages
		WHERE task_message_id = ? ORDER BY ts LIMIT 1`, taskMessageID).Scan(&channelID, &thread)
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrNotFound
	}
	return channelID, thread, err
}

// SetSlackDelivered records that the channel's messages ts went to the session.
func (s *Store) SetSlackDelivered(ctx context.Context, channelID string, ts []string, sessionID int64) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		for _, t := range ts {
			if _, err := tx.ExecContext(ctx, `UPDATE slack_messages SET delivered_to = ? WHERE channel_id = ? AND ts = ?`,
				sessionID, channelID, t); err != nil {
				return err
			}
		}
		return nil
	})
}

// SlackLastThread gives the Slack thread (its root's ts) of the latest of
// the channel's messages that went to the session; ErrNotFound if none did.
func (s *Store) SlackLastThread(ctx context.Context, channelID string, sessionID int64) (string, error) {
	var thread string
	err := s.db.QueryRowContext(ctx, `SELECT CASE thread_ts WHEN '' THEN ts ELSE thread_ts END FROM slack_messages
		WHERE channel_id = ? AND delivered_to = ? ORDER BY ts DESC LIMIT 1`, channelID, sessionID).Scan(&thread)
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrNotFound
	}
	return thread, err
}
