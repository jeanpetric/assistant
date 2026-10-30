package app

import (
	"context"
	"errors"
	"fmt"
	"log"

	"github.com/jeanpetric/assistant/internal/store"
)

// Reports a session started with report_to sends that session once a turn
// has really ended: after the skills, queued messages and moves that follow
// it, with no turn of its own left to run. Each opens with ReportHead.
const (
	ReportHead      = "Session #%d %q"
	ReportTaskHead  = " (task #%d %q)"
	ReportFinished  = "%s finished:\n\n%s"
	ReportFailed    = "%s failed:\n\n%s"
	ReportAsks      = "%s asks:\n\n%s\n\nAnswer it with send_message to session %d."
	ReportCancelled = "%s was cancelled."
)

// report tells the session's report_to how the session's turn ended, if
// turn is still its latest: one that started another (a skill, a queued
// message) leaves the report to that one. An interrupted turn reports nothing,
// nor does a session handed off: the session it handed off to reports.
// A report_to that is gone or runs on another node gets a notification instead.
func (a *App) report(ctx context.Context, sessionID, turn int64) {
	a.mu.Lock()
	latest := a.turns[sessionID] == turn && !a.stopping
	a.mu.Unlock()
	if !latest {
		return
	}
	sess, err := a.Store.Session(ctx, sessionID)
	if err != nil || sess.ReportTo == nil || sess.ChildID != nil {
		return
	}
	msg := a.reportMessage(ctx, sess)
	if msg == "" {
		return
	}
	target, err := a.Store.Session(ctx, *sess.ReportTo)
	if errors.Is(err, store.ErrNotFound) {
		err = errors.New("it was deleted")
	}
	if err == nil {
		err = own(target)
	}
	if err == nil {
		_, err = a.sendMessage(ctx, target.ID, msg, false)
	}
	if err != nil {
		log.Printf("session %d: report to %d: %v", sess.ID, *sess.ReportTo, err)
		a.notifySession(ctx, sess, fmt.Sprintf("Session %q could not report to session #%d (%v):\n\n%s", sess.Title, *sess.ReportTo, err, msg))
	}
}

// reportMessage says how the session's last turn ended; empty for none to report.
func (a *App) reportMessage(ctx context.Context, sess store.Session) string {
	head := fmt.Sprintf(ReportHead, sess.ID, sess.Title)
	if sess.TaskID != 0 {
		head += fmt.Sprintf(ReportTaskHead, sess.TaskID, sess.TaskTitle)
	}
	switch sess.Status {
	case store.StatusIdle:
		if q, err := a.Store.SessionOpenQuestion(ctx, sess.ID); err == nil {
			return fmt.Sprintf(ReportAsks, head, q.Body, sess.ID)
		}
		reply, err := a.lastReply(ctx, sess.ID)
		if err != nil {
			reply = "(no reply)"
		}
		return fmt.Sprintf(ReportFinished, head, reply)
	case store.StatusError:
		msg := sess.Error
		if msg == "" {
			msg = sess.LastMessage
		}
		return fmt.Sprintf(ReportFailed, head, msg)
	case store.StatusCancelled:
		return fmt.Sprintf(ReportCancelled, head)
	}
	return ""
}
