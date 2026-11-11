package server

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/jeanpetric/assistant/internal/app"
	"github.com/jeanpetric/assistant/internal/store"
)

// ---- PR reviews ----

// reviewRef is a review in a list: no notes, no diff, with its status.
type reviewRef struct {
	store.Review
	Status string `json:"status"`
}

func (s *Server) reviewRefs(ctx context.Context, taskID int64) ([]reviewRef, error) {
	xs, err := s.app.Store.Reviews(ctx, taskID)
	if err != nil {
		return nil, err
	}
	out := make([]reviewRef, len(xs))
	for i, x := range xs {
		out[i] = reviewRef{x, s.app.ReviewStatusOf(ctx, x)}
	}
	return out, nil
}

// generateReview adds a review of the task's PR and starts its session.
func (s *Server) generateReview(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, err)
		return
	}
	rev, sess, err := s.app.GenerateReview(r.Context(), id)
	if err != nil && sess.ID == 0 {
		writeErr(w, err)
		return
	}
	writeJSON(w, 201, map[string]int64{"review_id": rev.ID, "session_id": sess.ID})
}

// listReviews lists the task's reviews, newest first.
func (s *Server) listReviews(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, err)
		return
	}
	if _, err := s.app.Store.Task(r.Context(), id); err != nil {
		writeErr(w, err)
		return
	}
	xs, err := s.reviewRefs(r.Context(), id)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, xs)
}

// getReview is a review as its page shows it: the PR, the summary, the
// files in reading order with their diffs and notes, the questions and the
// session that answers them.
func (s *Server) getReview(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, err := pathID(r)
	if err != nil {
		writeErr(w, err)
		return
	}
	rev, err := s.app.Store.Review(ctx, id)
	if err != nil {
		writeErr(w, err)
		return
	}
	var notes app.ReviewNotes
	files := []app.ReviewFile{}
	if rev.Notes != "" {
		if err := json.Unmarshal([]byte(rev.Notes), &notes); err != nil {
			writeErr(w, err)
			return
		}
		files = app.ReviewFiles(rev.Diff, notes)
	}
	questions, err := s.app.Store.ReviewQuestions(ctx, id)
	if err != nil {
		writeErr(w, err)
		return
	}
	var session any
	if rev.SessionID != nil {
		if sess, err := s.app.Store.Session(ctx, *rev.SessionID); err == nil {
			session = map[string]any{"id": sess.ID, "title": sess.Title, "status": sess.Status, "owned": sess.Owned, "node_name": sess.Node()}
		}
	}
	writeJSON(w, 200, map[string]any{
		"review": reviewRef{rev, s.app.ReviewStatusOf(ctx, rev)}, "summary": notes.Summary, "files": files,
		"questions": questions, "session": session,
	})
}

func (s *Server) deleteReview(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, err)
		return
	}
	if err := s.app.Store.DeleteReview(r.Context(), id); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(204)
}

// saveReviewNotes keeps the agent's notes with the diff at diff_path and
// answers with the warnings it is to fix.
func (s *Server) saveReviewNotes(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, err)
		return
	}
	var in struct {
		Notes    json.RawMessage `json:"notes"`
		DiffPath string          `json:"diff_path"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, err)
		return
	}
	warnings, err := s.app.SaveReview(r.Context(), id, in.Notes, in.DiffPath)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"saved": true, "warnings": warnings})
}

// askReview sends a question on a line or file of the review to its session.
func (s *Server) askReview(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, err)
		return
	}
	var in struct {
		Path     string `json:"path"`
		Side     string `json:"side"`
		Line     int    `json:"line"`
		Question string `json:"question"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, err)
		return
	}
	q, err := s.app.AskReview(r.Context(), id, store.ReviewQuestion{Path: in.Path, Side: in.Side, Line: in.Line, Question: in.Question})
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 201, q)
}

// submitReview posts the user's verdict on the review's PR to GitHub:
// event approve, comment or request_changes, with body as its message.
func (s *Server) submitReview(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, err)
		return
	}
	var in struct {
		Event string `json:"event"`
		Body  string `json:"body"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, err)
		return
	}
	if err := s.app.SubmitReview(r.Context(), id, in.Event, in.Body); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"submitted": true, "event": in.Event})
}

// reviewWithoutDiff keeps get_review small: the files without their hunks.
func reviewWithoutDiff(raw []byte) ([]byte, error) {
	var out map[string]json.RawMessage
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	var files []map[string]json.RawMessage
	if err := json.Unmarshal(out["files"], &files); err != nil {
		return nil, err
	}
	for _, f := range files {
		delete(f, "hunks")
	}
	b, err := json.Marshal(files)
	if err != nil {
		return nil, err
	}
	out["files"] = b
	return json.Marshal(out)
}
