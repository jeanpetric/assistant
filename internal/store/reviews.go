package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// Review is a task's pull request made readable: the agent of its session
// ranks the files and annotates their lines (Notes, a JSON object, ” until
// it saves them), Diff is the unified diff they are about. The same session
// answers the user's questions on the review's lines.
type Review struct {
	ID        int64     `json:"id"`
	TaskID    int64     `json:"task_id"`
	SessionID *int64    `json:"session_id"`
	PRURL     string    `json:"pr_url"`
	Title     string    `json:"title"`
	HeadSHA   string    `json:"head_sha"`
	Notes     string    `json:"-"`
	Diff      string    `json:"-"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Question statuses: waiting for the session's turn, answered, or the turn
// failed.
const (
	QuestionPending  = "pending"
	QuestionAnswered = "answered"
	QuestionFailed   = "failed"
)

// ReviewQuestion is the user's question on a line of a review's diff (Side
// new or old, Line its number there) or on a whole file (Side ”, Line 0).
type ReviewQuestion struct {
	ID          int64      `json:"id"`
	ReviewID    int64      `json:"review_id"`
	Path        string     `json:"path"`
	Side        string     `json:"side"`
	Line        int        `json:"line"`
	Question    string     `json:"question"`
	ShortAnswer string     `json:"short_answer"`
	FullAnswer  string     `json:"full_answer"`
	Status      string     `json:"status"`
	Error       string     `json:"error"`
	CreatedAt   time.Time  `json:"created_at"`
	AnsweredAt  *time.Time `json:"answered_at"`
}

// CreateReview adds a review of a task's pull request, without notes yet.
func (s *Store) CreateReview(ctx context.Context, x Review) (Review, error) {
	now := s.ts()
	id, err := s.insert(ctx, "reviews", `INSERT INTO reviews (id, task_id, session_id, pr_url, title, head_sha, notes, diff, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, x.TaskID, x.SessionID, x.PRURL, x.Title, x.HeadSHA, x.Notes, x.Diff, now, now)
	if err != nil {
		return x, err
	}
	x.ID, x.CreatedAt, x.UpdatedAt = id, fromMillis(now), fromMillis(now)
	return x, nil
}

// SetReviewSession names the session that makes the review and answers its
// questions.
func (s *Store) SetReviewSession(ctx context.Context, id, sessionID int64) error {
	return affected(s.db.ExecContext(ctx, `UPDATE reviews SET session_id = ?, updated_at = ? WHERE id = ?`, sessionID, s.ts(), id))
}

// SaveReviewNotes keeps what the agent made of the pull request; a later save
// replaces it.
func (s *Store) SaveReviewNotes(ctx context.Context, id int64, prURL, title, headSHA, notes, diff string) error {
	return affected(s.db.ExecContext(ctx, `UPDATE reviews SET pr_url = ?, title = ?, head_sha = ?, notes = ?, diff = ?, updated_at = ?
		WHERE id = ?`, prURL, title, headSHA, notes, diff, s.ts(), id))
}

const reviewCols = `id, task_id, session_id, pr_url, title, head_sha, notes, created_at, updated_at`

func scanReview(row interface{ Scan(...any) error }, diff *string) (Review, error) {
	var x Review
	var sess sql.NullInt64
	var created, updated int64
	dest := []any{&x.ID, &x.TaskID, &sess, &x.PRURL, &x.Title, &x.HeadSHA, &x.Notes, &created, &updated}
	if diff != nil {
		dest = append(dest, diff)
	}
	if err := row.Scan(dest...); err != nil {
		return x, err
	}
	x.SessionID, x.CreatedAt, x.UpdatedAt = fromNullID(sess), fromMillis(created), fromMillis(updated)
	return x, nil
}

// Review is one review with its diff.
func (s *Store) Review(ctx context.Context, id int64) (Review, error) {
	var diff string
	x, err := scanReview(s.db.QueryRowContext(ctx, `SELECT `+reviewCols+`, diff FROM reviews WHERE id = ?`, id), &diff)
	if errors.Is(err, sql.ErrNoRows) {
		return x, ErrNotFound
	}
	x.Diff = diff
	return x, err
}

// Reviews lists a task's reviews, newest first, without their diffs.
func (s *Store) Reviews(ctx context.Context, taskID int64) ([]Review, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+reviewCols+` FROM reviews WHERE task_id = ? ORDER BY id DESC`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Review{}
	for rows.Next() {
		x, err := scanReview(rows, nil)
		if err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

// DeleteReview deletes a review and its questions.
func (s *Store) DeleteReview(ctx context.Context, id int64) error {
	return affected(s.db.ExecContext(ctx, `DELETE FROM reviews WHERE id = ?`, id))
}

// AddReviewQuestion adds a pending question to a review.
func (s *Store) AddReviewQuestion(ctx context.Context, q ReviewQuestion) (ReviewQuestion, error) {
	now := s.ts()
	q.Status = QuestionPending
	id, err := s.insert(ctx, "review_questions", `INSERT INTO review_questions (id, review_id, path, side, line, question, status, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, q.ReviewID, q.Path, q.Side, q.Line, q.Question, q.Status, now)
	if err != nil {
		return q, err
	}
	q.ID, q.CreatedAt = id, fromMillis(now)
	return q, nil
}

// DeleteReviewQuestion deletes one question.
func (s *Store) DeleteReviewQuestion(ctx context.Context, id int64) error {
	return affected(s.db.ExecContext(ctx, `DELETE FROM review_questions WHERE id = ?`, id))
}

// ReviewQuestions lists a review's questions, oldest first.
func (s *Store) ReviewQuestions(ctx context.Context, reviewID int64) ([]ReviewQuestion, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, review_id, path, side, line, question, short_answer, full_answer, status, error, created_at, answered_at
		FROM review_questions WHERE review_id = ? ORDER BY id`, reviewID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ReviewQuestion{}
	for rows.Next() {
		var q ReviewQuestion
		var created int64
		var answered sql.NullInt64
		if err := rows.Scan(&q.ID, &q.ReviewID, &q.Path, &q.Side, &q.Line, &q.Question, &q.ShortAnswer, &q.FullAnswer,
			&q.Status, &q.Error, &created, &answered); err != nil {
			return nil, err
		}
		q.CreatedAt, q.AnsweredAt = fromMillis(created), fromNullMillis(answered)
		out = append(out, q)
	}
	return out, rows.Err()
}

// AnswerReviewQuestion settles the oldest pending question of the reviews
// the session answers: status answered with the answers, or failed with
// errMsg. It returns the question's id, 0 if none was pending.
func (s *Store) AnswerReviewQuestion(ctx context.Context, sessionID int64, status, short, full, errMsg string) (int64, error) {
	var id int64
	err := s.tx(ctx, func(tx *sql.Tx) error {
		err := tx.QueryRowContext(ctx, `SELECT q.id FROM review_questions q JOIN reviews r ON r.id = q.review_id
			WHERE r.session_id = ? AND q.status = ? ORDER BY q.id LIMIT 1`, sessionID, QuestionPending).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE review_questions SET status = ?, short_answer = ?, full_answer = ?, error = ?, answered_at = ?
			WHERE id = ?`, status, short, full, errMsg, s.ts(), id)
		return err
	})
	return id, err
}
