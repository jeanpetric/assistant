package app

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jeanpetric/assistant/internal/store"
)

// ErrBadMessage: a task message the thread cannot take, e.g. an answer to
// something that is not an open question of the task.
var ErrBadMessage = errors.New("bad task message")

// ActivityNote follows the session prompt on every turn of a session on a
// task: how its agent keeps the task's activity thread.
const ActivityNote = `This task has an activity thread: a short chat between its agent sessions and the user, which get_task returns as activity; read it before you start. ` +
	`At the end of every turn, and whenever you hand work over to another session, post one short message on it with post_task_message (task_id %d, session_id %d): ` +
	`what you did or passed on, in 1-3 sentences like on Slack; the details stay in this session. ` +
	`When you need the user's decision, post it as a question (question: true) and end your turn instead of guessing: their answer comes as your next message.`

// AnswerPrompt continues the session that asked a question on the thread
// with the user's answer.
const AnswerPrompt = "The user answered your question on the task's activity thread (%q):\n\n%s"

// PostedMessage is a message added to a task's thread and, for the user's,
// the question it answered and whether the answer waits in the asking
// session's queue.
type PostedMessage struct {
	store.TaskMessage
	AnsweredID *int64 `json:"answered_id"`
	Queued     bool   `json:"queued"`
}

// PostTaskMessage adds a message to the task's activity thread.
//
// From a session (SessionID set), a question flags the session as waiting
// for the user and notifies them. From the user, the message answers the
// question answerTo (0: the task's latest open one, if any): it goes to the
// session that asked as its next message, queued while it works, and the
// question is answered. With no open question it only stays on the thread.
func (a *App) PostTaskMessage(ctx context.Context, m store.TaskMessage, answerTo int64) (PostedMessage, error) {
	m.Body = strings.TrimSpace(m.Body)
	if m.Body == "" {
		return PostedMessage{}, fmt.Errorf("%w: body is required", ErrBadMessage)
	}
	if _, err := a.Store.Task(ctx, m.TaskID); err != nil {
		return PostedMessage{}, err
	}
	if m.SessionID != nil {
		if answerTo != 0 {
			return PostedMessage{}, fmt.Errorf("%w: only the user answers questions", ErrBadMessage)
		}
		sess, err := a.Store.Session(ctx, *m.SessionID)
		if errors.Is(err, store.ErrNotFound) {
			return PostedMessage{}, fmt.Errorf("%w: unknown session_id", ErrBadMessage)
		} else if err != nil {
			return PostedMessage{}, err
		}
		x, err := a.Store.AddTaskMessage(ctx, m)
		if err != nil || !m.Question {
			return PostedMessage{TaskMessage: x}, err
		}
		if err := a.Store.SetSessionNeedsInput(ctx, sess.ID, "asked a question on the task"); err != nil {
			return PostedMessage{TaskMessage: x}, err
		}
		a.notifySession(ctx, sess, fmt.Sprintf("Question from %q: %s", sess.Title, m.Body))
		return PostedMessage{TaskMessage: x}, nil
	}

	m.Question = false // only agents ask
	var q store.TaskMessage
	var err error
	if answerTo != 0 {
		q, err = a.Store.TaskMessage(ctx, answerTo)
		if err == nil && (q.TaskID != m.TaskID || !q.Question || q.AnsweredAt != nil) {
			err = store.ErrNotFound
		}
		if errors.Is(err, store.ErrNotFound) {
			return PostedMessage{}, fmt.Errorf("%w: answer_to is not an open question of the task", ErrBadMessage)
		}
	} else if q, err = a.Store.OpenQuestion(ctx, m.TaskID); errors.Is(err, store.ErrNotFound) {
		x, err := a.Store.AddTaskMessage(ctx, m)
		return PostedMessage{TaskMessage: x}, err
	}
	if err != nil {
		return PostedMessage{}, err
	}
	var asker *store.Session
	if q.SessionID != nil {
		sess, err := a.Store.Session(ctx, *q.SessionID)
		if err == nil {
			err = own(sess)
		}
		if err != nil {
			return PostedMessage{}, err
		}
		asker = &sess
	}
	// Answered before the session gets it, so its turn ends with nothing open;
	// a session that cannot take the answer leaves the question open again
	// and the thread as it was.
	if err := a.Store.SetQuestionAnswered(ctx, q.ID, true); err != nil {
		return PostedMessage{}, err
	}
	out := PostedMessage{AnsweredID: &q.ID}
	if asker != nil {
		sess := *asker
		err := a.Store.SetSessionNeedsInput(ctx, sess.ID, "")
		if err == nil {
			if out.Queued, err = a.SendMessage(ctx, sess.ID, fmt.Sprintf(AnswerPrompt, q.Body, m.Body)); err != nil {
				a.Store.SetSessionNeedsInput(ctx, sess.ID, sess.NeedsInput)
			}
		}
		if err != nil {
			a.Store.SetQuestionAnswered(ctx, q.ID, false)
			return PostedMessage{}, err
		}
	}
	out.TaskMessage, err = a.Store.AddTaskMessage(ctx, m)
	return out, err
}

// waitsForAnswer reports whether the session asked a question on its task's
// thread that the user has yet to answer.
func (a *App) waitsForAnswer(ctx context.Context, sessionID int64) bool {
	open, err := a.Store.HasOpenQuestion(ctx, sessionID)
	return err == nil && open
}
