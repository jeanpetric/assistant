package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
)

// ErrGitHub: gh refused or failed; the error carries what it said.
var ErrGitHub = errors.New("github")

// Review verdicts, as the page and the API name them, with gh's flag.
var reviewEvents = map[string]string{
	"approve":         "--approve",
	"comment":         "--comment",
	"request_changes": "--request-changes",
}

var prPartsRe = regexp.MustCompile(`^https?://github\.com/([^/\s]+)/([^/\s]+)/pull/(\d+)(?:[/?#].*)?$`)

// ParsePRURL takes owner, repo and number from a GitHub pull request URL,
// with or without a trailing /changes, /files or /commits.
func ParsePRURL(u string) (owner, repo, number string, ok bool) {
	m := prPartsRe.FindStringSubmatch(strings.TrimSpace(u))
	if m == nil {
		return "", "", "", false
	}
	return m[1], m[2], m[3], true
}

// gh runs the GitHub CLI: a.GH if set (tests), else gh on the PATH. The
// error carries gh's stderr.
func (a *App) gh(ctx context.Context, args ...string) ([]byte, error) {
	if a.GH != nil {
		return a.GH(ctx, args...)
	}
	cmd := exec.CommandContext(ctx, "gh", args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return out, errors.New(msg)
		}
	}
	return out, err
}

// SubmitReview posts the user's review of the review's PR on GitHub: event
// approve, comment or request_changes, with body as its message (required
// but for approve), through gh pr review as the user gh is logged in as.
func (a *App) SubmitReview(ctx context.Context, reviewID int64, event, body string) error {
	r, err := a.Store.Review(ctx, reviewID)
	if err != nil {
		return err
	}
	flag, ok := reviewEvents[event]
	if !ok {
		return fmt.Errorf("%w: event must be approve, comment or request_changes", ErrBadReview)
	}
	body = strings.TrimSpace(body)
	if body == "" && event != "approve" {
		return fmt.Errorf("%w: a comment or a change request needs a message", ErrBadReview)
	}
	owner, repo, n, ok := ParsePRURL(r.PRURL)
	if !ok {
		return fmt.Errorf("%w: %q is not a GitHub pull request URL", ErrBadReview, r.PRURL)
	}
	args := []string{"pr", "review", n, "-R", owner + "/" + repo, flag}
	if body != "" {
		args = append(args, "--body", body)
	}
	if _, err := a.gh(ctx, args...); err != nil {
		return fmt.Errorf("%w: gh pr review: %v", ErrGitHub, err)
	}
	return nil
}
