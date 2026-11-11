package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"text/template"

	"github.com/jeanpetric/assistant/internal/store"
)

// A PR review: the agent of a session started on a task with a pull_request
// field fetches the PR with gh, ranks its files and annotates their lines,
// and saves that (save_review) with the diff; the app shows it as a page.
// Questions on the page's lines go to that same session.

var (
	// ErrNoPR: a review needs the task's pull_request field.
	ErrNoPR = errors.New("task has no pull_request field")
	// ErrBadReview: what was sent for a review cannot be used.
	ErrBadReview = errors.New("bad review")
	// ErrReviewNotReady: the review has no notes yet, so no questions.
	ErrReviewNotReady = errors.New("the review is not ready: it is still being generated or its generation failed")
)

// ReviewTrigger starts a review's session. Its turns run no skills and move
// no task: they only make the review and answer its questions.
const ReviewTrigger = "review"

// MaxReviewDiff is the largest diff a review keeps.
const MaxReviewDiff = 2 << 20

// Review statuses, from the review and its session.
const (
	ReviewGenerating = "generating" // no notes yet, the session works on them
	ReviewFailed     = "failed"     // no notes and the session stopped
	ReviewReady      = "ready"
)

// ReviewHide are the patterns of the files a review groups under "Skriveno"
// unless the notes rank them: one without / matches the base name, dir/**
// a path prefix.
var ReviewHide = []string{"*.yml", "*.yaml", "*.md", "*.json", "*.plain", "spec/**", "test/**", "*_spec.rb", "*_test.go"}

// HiddenBy returns the first of ReviewHide matching the path, "" if none.
func HiddenBy(p string) string {
	base := path.Base(p)
	for _, pat := range ReviewHide {
		if pre, ok := strings.CutSuffix(pat, "/**"); ok {
			if strings.HasPrefix(p, pre+"/") || strings.Contains("/"+p, "/"+pre+"/") {
				return pat
			}
			continue
		}
		target := p
		if !strings.Contains(pat, "/") {
			target = base
		}
		if ok, _ := path.Match(pat, target); ok {
			return pat
		}
	}
	return ""
}

// ---- notes ----

// ReviewNotes is what the agent makes of a PR, as the PR review skill's
// notes.json.
type ReviewNotes struct {
	Title       string                        `json:"title"`
	URL         string                        `json:"url"`
	Repo        string                        `json:"repo,omitempty"`
	Head        string                        `json:"head"`
	Summary     string                        `json:"summary"`
	Files       []NoteFile                    `json:"files"`
	Annotations map[string][]ReviewAnnotation `json:"annotations"`
	// Concepts explains, in Croatian, the :Concepts: the PR's .plain specs
	// use, by name without colons.
	Concepts map[string]string `json:"concepts,omitempty"`
	// Specs is set by SaveReview, never by the agent: the head content of
	// the PR's .plain files and the modules they import, by path.
	Specs map[string]string `json:"specs,omitempty"`
}

// NoteFile ranks a file (1 = read first) or calls it unimportant, saying why.
type NoteFile struct {
	Path        string `json:"path"`
	Rank        int    `json:"rank,omitempty"`
	Unimportant bool   `json:"unimportant,omitempty"`
	Why         string `json:"why"`
}

// ReviewAnnotation explains a line shown in the diff: Side new (the default)
// or old, Kind class, method or line.
type ReviewAnnotation struct {
	Line int    `json:"line"`
	Side string `json:"side,omitempty"`
	Kind string `json:"kind,omitempty"`
	Text string `json:"text"`
}

func (n ReviewAnnotation) side() string {
	if n.Side == "old" {
		return "old"
	}
	return "new"
}

// ---- diff ----

// PRFile is one file of a unified diff.
type PRFile struct {
	Path    string   `json:"path"`
	OldPath string   `json:"old_path"`
	Status  string   `json:"status"` // modified, added, deleted or renamed
	Binary  bool     `json:"binary"`
	Adds    int      `json:"adds"`
	Dels    int      `json:"dels"`
	Hunks   []PRHunk `json:"hunks"`
}

// PRHunk is a hunk: its @@ line and its lines.
type PRHunk struct {
	Header string   `json:"header"`
	Lines  []PRLine `json:"lines"`
}

// PRLine is a line of a hunk: T is +, -, space or \; Old and New its
// numbers on each side (0: not on that side).
type PRLine struct {
	T    string `json:"t"`
	Old  int    `json:"old"`
	New  int    `json:"new"`
	Text string `json:"text"`
}

var hunkRe = regexp.MustCompile(`^@@ -(\d+)(?:,\d+)? \+(\d+)(?:,\d+)? @@`)
var diffGitRe = regexp.MustCompile(`^diff --git a/(.*) b/(.*)$`)

// ParseDiff splits `gh pr diff` output into its files.
func ParseDiff(text string) []PRFile {
	var files []PRFile
	var cur *PRFile
	var hunk *PRHunk
	oldNo, newNo := 0, 0
	for _, raw := range strings.Split(text, "\n") {
		raw = strings.TrimSuffix(raw, "\r")
		if strings.HasPrefix(raw, "diff --git ") {
			a, b := raw[11:], raw[11:]
			if m := diffGitRe.FindStringSubmatch(raw); m != nil {
				a, b = m[1], m[2]
			}
			files = append(files, PRFile{Path: b, OldPath: a, Status: "modified", Hunks: []PRHunk{}})
			cur, hunk = &files[len(files)-1], nil
			continue
		}
		if cur == nil {
			continue
		}
		if hunk == nil {
			switch {
			case strings.HasPrefix(raw, "new file mode"):
				cur.Status = "added"
			case strings.HasPrefix(raw, "deleted file mode"):
				cur.Status = "deleted"
			case strings.HasPrefix(raw, "rename from "):
				cur.OldPath, cur.Status = raw[12:], "renamed"
			case strings.HasPrefix(raw, "rename to "):
				cur.Path = raw[10:]
			case strings.HasPrefix(raw, "Binary files"):
				cur.Binary = true
			case strings.HasPrefix(raw, "+++ ") && raw[4:] != "/dev/null":
				cur.Path = strings.TrimPrefix(raw[4:], "b/")
			}
		}
		if m := hunkRe.FindStringSubmatch(raw); m != nil {
			oldNo, _ = strconv.Atoi(m[1])
			newNo, _ = strconv.Atoi(m[2])
			cur.Hunks = append(cur.Hunks, PRHunk{Header: raw, Lines: []PRLine{}})
			hunk = &cur.Hunks[len(cur.Hunks)-1]
			continue
		}
		if hunk == nil || raw == "" {
			continue
		}
		t, body := raw[:1], raw[1:]
		switch t {
		case "+":
			hunk.Lines = append(hunk.Lines, PRLine{T: t, New: newNo, Text: body})
			newNo++
			cur.Adds++
		case "-":
			hunk.Lines = append(hunk.Lines, PRLine{T: t, Old: oldNo, Text: body})
			oldNo++
			cur.Dels++
		case " ":
			hunk.Lines = append(hunk.Lines, PRLine{T: t, Old: oldNo, New: newNo, Text: body})
			oldNo++
			newNo++
		case `\`:
			hunk.Lines = append(hunk.Lines, PRLine{T: t, Text: body})
		}
	}
	return files
}

// shown reports whether the line is one a question or note on side/line
// points at: a + or context line for new, a - line for old.
func (l PRLine) shown(side string, line int) bool {
	if side == "old" {
		return l.T == "-" && l.Old == line
	}
	return (l.T == "+" || l.T == " ") && l.New == line
}

func (f PRFile) has(side string, line int) bool {
	for _, h := range f.Hunks {
		for _, l := range h.Lines {
			if l.shown(side, line) {
				return true
			}
		}
	}
	return false
}

func findFile(files []PRFile, p string) (PRFile, bool) {
	for _, f := range files {
		if f.Path == p || f.OldPath == p {
			return f, true
		}
	}
	return PRFile{}, false
}

// ---- generating ----

// ReviewStatus says how far the review is: ready with notes, else
// generating while its session runs, else failed.
func ReviewStatus(r store.Review, sess *store.Session) string {
	switch {
	case r.Notes != "":
		return ReviewReady
	case sess != nil && sess.Status == store.StatusRunning:
		return ReviewGenerating
	}
	return ReviewFailed
}

// ReviewStatusOf is ReviewStatus with the review's session looked up.
func (a *App) ReviewStatusOf(ctx context.Context, r store.Review) string {
	if r.SessionID == nil {
		return ReviewStatus(r, nil)
	}
	sess, err := a.Store.Session(ctx, *r.SessionID)
	if err != nil {
		return ReviewStatus(r, nil)
	}
	return ReviewStatus(r, &sess)
}

// GenerateReview adds a review of the task's pull request and starts the
// session that makes it, which also answers the review's questions.
func (a *App) GenerateReview(ctx context.Context, taskID int64) (store.Review, store.Session, error) {
	task, err := a.Store.Task(ctx, taskID)
	if err != nil {
		return store.Review{}, store.Session{}, err
	}
	pr := strings.TrimSpace(task.Fields["pull_request"])
	if pr == "" {
		return store.Review{}, store.Session{}, ErrNoPR
	}
	r, err := a.Store.CreateReview(ctx, store.Review{TaskID: taskID, PRURL: pr})
	if err != nil {
		return r, store.Session{}, err
	}
	prompt, err := ReviewPrompt(task.ID, r.ID, pr)
	if err != nil {
		_ = a.Store.DeleteReview(ctx, r.ID)
		return r, store.Session{}, err
	}
	sess, err := a.StartSession(ctx, StartOpts{TaskID: taskID, Prompt: prompt, Title: "PR review", Trigger: ReviewTrigger})
	if sess.ID == 0 {
		_ = a.Store.DeleteReview(ctx, r.ID)
		return r, sess, err
	}
	if serr := a.Store.SetReviewSession(ctx, r.ID, sess.ID); serr != nil && err == nil {
		err = serr
	}
	r.SessionID = &sess.ID
	return r, sess, err
}

var reviewPromptTmpl = template.Must(template.New("review").Parse(`Make a review of the pull request of task #{{.TaskID}} for the user, who reads it as review {{.ReviewID}} in the assistant app: important files first with a reason, noise last, and short Croatian notes beside classes, methods and key lines. Your part is fetching the PR, deciding the ranking and writing the notes; the app renders them. Read only: never comment on, review or change anything on GitHub.

## 1. The PR

The task's PR: {{.PRURL}}. Strip a trailing ` + "`/changes`, `/files` or `/commits`" + ` from the URL; take ` + "`<owner>`, `<repo>` and `<n>`" + ` from it.

` + "```bash" + `
W=$(mktemp -d -t pr-review)
gh pr view <n> -R <owner>/<repo> --json number,title,url,state,headRefOid,baseRefName,body,files > $W/pr.json
gh pr diff <n> -R <owner>/<repo> > $W/pr.diff
` + "```" + `

If gh cannot read the PR (no access, no such PR), do not save the review: reply with gh's error in one sentence and stop. A closed or merged PR is reviewed as any other.

## 2. Filter

These patterns put a file under "Skriveno" at the bottom of the page unless you rank it (you only need to know which files you can skip reading closely): {{.Hide}}. A pattern without ` + "`/`" + ` matches the base name, ` + "`dir/**`" + ` a path prefix.

## 3. Read and decide

Read the PR body and the whole diff of every file the filter does not hide (skim the hidden ones, including tests and docs, only for what they tell you about intent). Then write the notes:

` + "```json" + `
{
  "title": "<repo>#<n>: <PR title>",
  "url": "<PR url>", "repo": "<owner>/<repo>#<n>", "head": "<headRefOid>",
  "summary": "2-4 rečenice: što PR mijenja i zašto, za čitatelja koji ne zna kod.",
  "files": [
    {"path": "app/x.rb", "rank": 1, "why": "Jedna rečenica: zašto je bitan za review."},
    {"path": "app/y.rb", "unimportant": true, "why": "Jedna rečenica: zašto ga ne treba čitati."}
  ],
  "annotations": {
    "app/x.rb": [
      {"line": 12, "kind": "class", "text": "Što klasa radi."},
      {"line": 30, "kind": "method", "text": "Što metoda radi i zašto."},
      {"line": 41, "kind": "line", "text": "Zašto je ova linija bitna."},
      {"line": 7, "side": "old", "text": "Što je maknuto i zašto."}
    ]
  }
}
` + "```" + `

- **Ranking:** every file the filter does not hide gets an entry: either ` + "`rank`" + ` (1 = most important; the core logic first, then its callers, then wiring) with ` + "`why`" + `, or ` + "`unimportant: true`" + ` with ` + "`why`" + ` for a trivial change (a rename, an import, a one-line wiring the summary already explains). Don't mark a file unimportant just because it is small if a reviewer would want to see it.
- **Annotations:** on every ranked file. One per new or changed class and method, on the line of its ` + "`class`/`module`/`def`/`func`" + ` (side new; a removed one on the old file's line with ` + "`\"side\": \"old\"`" + `). Add ` + "`line`" + ` notes only where a line is non-obvious: a rounding rule, a guard, an ordering, a side effect, a query. Not every line. Each text is 1-2 sentences in Croatian, natural language, saying what it does and why, not restating the code. Write identifiers in backticks.
- ` + "`line`" + ` must be a line shown in the diff (a + or context line for side new, a - line for side old). Count from the hunk headers (` + "`@@ -a,b +c,d @@`" + `: the first new-side line is c), or open the file at the head commit to check.
- Hidden files get no annotations and need no entry.
- **Concepts:** when the PR changes ` + "`.plain`" + ` specs (PlainLang), add ` + "`\"concepts\": {\"Period\": \"…\"}`" + `: for every ` + "`:Concept:`" + ` their changed parts use, by name without the colons, 1-2 Croatian sentences on what it is in the product, for a reader who does not know the code (the page shows them on hover next to the concept's definition). Read the modules they import for the definitions.
- Croatian for summary, why and notes; code stays as written. Paraphrase, don't judge: a real bug you notice goes in one clause at the end of the summary.

## 4. Save

Call the assistant MCP tool ` + "`save_review`" + ` with review_id {{.ReviewID}}, notes (the object above, not a string) and diff_path (the absolute path of $W/pr.diff on this computer). Every warning it returns (a path or line not in the diff) is a mistake in your notes: fix them and call it again until there are none; each call replaces the review's notes. A diff over 2 MB is refused: then say so in your reply.

## Reply

In Croatian, 2-4 sentences: what the PR does and how many files are worth reading. The user's questions on lines of this review come later in this session, one per turn; answer each as it asks.
`))

// ReviewPrompt is the first prompt of a review's session.
func ReviewPrompt(taskID, reviewID int64, prURL string) (string, error) {
	var b bytes.Buffer
	err := reviewPromptTmpl.Execute(&b, map[string]any{
		"TaskID": taskID, "ReviewID": reviewID, "PRURL": prURL, "Hide": "`" + strings.Join(ReviewHide, "` `") + "`",
	})
	return b.String(), err
}

// SaveReview keeps the agent's notes on a review with the diff read from
// diffPath, replacing earlier ones. The notes' paths and annotated lines
// must be in the diff; the ones that are not come back as warnings (the
// review is saved all the same) for the agent to fix.
func (a *App) SaveReview(ctx context.Context, reviewID int64, notes json.RawMessage, diffPath string) ([]string, error) {
	rev, err := a.Store.Review(ctx, reviewID)
	if err != nil {
		return nil, err
	}
	var n ReviewNotes
	if err := json.Unmarshal(notes, &n); err != nil || len(bytes.TrimSpace(notes)) == 0 || bytes.TrimSpace(notes)[0] != '{' {
		return nil, fmt.Errorf("%w: notes must be a JSON object like notes.json", ErrBadReview)
	}
	if !path.IsAbs(diffPath) {
		return nil, fmt.Errorf("%w: diff_path must be an absolute path", ErrBadReview)
	}
	st, err := os.Stat(diffPath)
	if err != nil {
		return nil, fmt.Errorf("%w: diff_path: %v", ErrBadReview, err)
	}
	if st.Size() > MaxReviewDiff {
		return nil, fmt.Errorf("%w: the diff is %d bytes, over the 2 MB a review keeps", ErrBadReview, st.Size())
	}
	raw, err := os.ReadFile(diffPath)
	if err != nil {
		return nil, fmt.Errorf("%w: diff_path: %v", ErrBadReview, err)
	}
	diff := string(raw)
	files := ParseDiff(diff)
	if len(files) == 0 {
		return nil, fmt.Errorf("%w: diff_path has no file diffs (gh pr diff output expected)", ErrBadReview)
	}
	warnings := []string{}
	for _, f := range n.Files {
		if _, ok := findFile(files, f.Path); !ok {
			warnings = append(warnings, "notes name a file not in the diff: "+f.Path)
		}
	}
	paths := make([]string, 0, len(n.Annotations))
	for p := range n.Annotations {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		f, ok := findFile(files, p)
		if !ok {
			warnings = append(warnings, "annotations name a file not in the diff: "+p)
			continue
		}
		for _, an := range n.Annotations[p] {
			if !f.has(an.side(), an.Line) {
				warnings = append(warnings, fmt.Sprintf("%s: note on %s line %d is not in the diff", p, an.side(), an.Line))
			}
		}
	}
	if n.Files == nil {
		n.Files = []NoteFile{}
	}
	if n.Annotations == nil {
		n.Annotations = map[string][]ReviewAnnotation{}
	}
	prURL := n.URL
	if prURL == "" {
		prURL = rev.PRURL
	}
	n.Specs = a.fetchSpecs(ctx, prURL, n.Head, files)
	kept, err := json.Marshal(n)
	if err != nil {
		return nil, err
	}
	return warnings, a.Store.SaveReviewNotes(ctx, reviewID, n.URL, n.Title, n.Head, string(kept), diff)
}

// ---- the page ----

// ReviewFile is a file of the review as the page shows it, in Group
// ranked, unimportant, hidden (by HiddenBy) or other.
type ReviewFile struct {
	PRFile
	Group       string             `json:"group"`
	Rank        int                `json:"rank,omitempty"`
	Why         string             `json:"why,omitempty"`
	HiddenBy    string             `json:"hidden_by,omitempty"`
	Annotations []ReviewAnnotation `json:"annotations"`
}

var groupOrder = map[string]int{"ranked": 0, "unimportant": 1, "hidden": 2, "other": 3}

// ReviewFiles orders the diff's files for the page: ranked by rank, then
// unimportant, then hidden, then the rest by path. A ranked or unimportant
// file stays so even if a hide pattern matches it.
func ReviewFiles(diff string, n ReviewNotes) []ReviewFile {
	info := map[string]NoteFile{}
	for _, f := range n.Files {
		if _, ok := info[f.Path]; !ok {
			info[f.Path] = f
		}
	}
	out := []ReviewFile{}
	for _, f := range ParseDiff(diff) {
		rf := ReviewFile{PRFile: f, Group: "other", Annotations: []ReviewAnnotation{}}
		in, ok := info[f.Path]
		if !ok {
			in, ok = info[f.OldPath]
		}
		switch {
		case ok && in.Rank > 0:
			rf.Group, rf.Rank, rf.Why = "ranked", in.Rank, in.Why
		case ok && in.Unimportant:
			rf.Group, rf.Why = "unimportant", in.Why
		case HiddenBy(f.Path) != "":
			rf.Group, rf.HiddenBy, rf.Why = "hidden", HiddenBy(f.Path), in.Why
		}
		if anns, ok := n.Annotations[f.Path]; ok {
			rf.Annotations = anns
		} else if anns, ok := n.Annotations[f.OldPath]; ok {
			rf.Annotations = anns
		}
		for i := range rf.Annotations {
			rf.Annotations[i].Side = rf.Annotations[i].side()
		}
		out = append(out, rf)
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if groupOrder[a.Group] != groupOrder[b.Group] {
			return groupOrder[a.Group] < groupOrder[b.Group]
		}
		switch a.Group {
		case "ranked":
			return a.Rank < b.Rank
		case "other":
			return a.Path < b.Path
		}
		return false
	})
	return out
}

// ---- questions ----

// QuestionPrefix opens every question's prompt: a turn started by one
// answers the review's oldest pending question.
const QuestionPrefix = "Pitanje iz reviewa PR-a "

var questionTmpl = template.Must(template.New("question").Parse(QuestionPrefix + `{{.PRURL}} ("{{.Title}}", head {{.Head}}).
File: {{.Path}}{{if .Line}}, linija {{.Line}} ({{.Side}} strana){{end}}
Isječak diffa:
` + "```diff" + `
{{.Snippet}}
` + "```" + `
{{if .Note}}Postojeća bilješka: {{.Note}}
{{end}}
Pitanje: {{.Question}}

Odgovori na hrvatskom. Na kraju odgovora dodaj <kratko>2-4 rečenice jednostavnim jezikom: odgovor na pitanje</kratko>.
Kod PR-a čitaj s ` + "`gh`/`git`" + ` na head commitu; ništa ne mijenjaj ni ne komentiraj na GitHubu.`))

// QuestionPrompt is the prompt of a question on a review's line (side and
// line set) or file: the PR, the place, the diff around it, the review's
// note there and the question.
func QuestionPrompt(r store.Review, n ReviewNotes, q store.ReviewQuestion) (string, error) {
	f, ok := findFile(ParseDiff(r.Diff), q.Path)
	if !ok {
		return "", fmt.Errorf("%w: %s is not in the review's diff", ErrBadReview, q.Path)
	}
	snippet, note := "", []string{}
	if q.Line == 0 {
		snippet = fileSnippet(f, 200)
	} else {
		var ok bool
		if snippet, ok = lineSnippet(f, q.Side, q.Line, 20); !ok && q.Side == "new" && n.Specs[f.Path] != "" {
			// A spec's line the diff does not show: the page shows the whole module.
			snippet, ok = specSnippet(n.Specs[f.Path], q.Line, 20)
			if ok {
				snippet = "(nepromijenjeni dio specifikacije na head commitu, linija označena s >)\n" + snippet
			}
		}
		if !ok {
			return "", fmt.Errorf("%w: %s has no %s line %d in the diff", ErrBadReview, q.Path, q.Side, q.Line)
		}
		anns := n.Annotations[f.Path]
		if _, ok := n.Annotations[f.Path]; !ok {
			anns = n.Annotations[f.OldPath]
		}
		for _, an := range anns {
			if an.side() == q.Side && an.Line == q.Line {
				note = append(note, an.Text)
			}
		}
	}
	title, prURL, head := r.Title, r.PRURL, r.HeadSHA
	var b bytes.Buffer
	err := questionTmpl.Execute(&b, map[string]any{
		"PRURL": prURL, "Title": title, "Head": head, "Path": q.Path, "Line": q.Line, "Side": q.Side,
		"Snippet": snippet, "Note": strings.Join(note, " "), "Question": q.Question,
	})
	return b.String(), err
}

func (l PRLine) String() string { return l.T + l.Text }

// lineSnippet is the hunk with the line, at most around lines before and
// after it.
func lineSnippet(f PRFile, side string, line, around int) (string, bool) {
	for _, h := range f.Hunks {
		for i, l := range h.Lines {
			if !l.shown(side, line) {
				continue
			}
			from, to := max(0, i-around), min(len(h.Lines), i+around+1)
			out := []string{}
			if from == 0 {
				out = append(out, h.Header)
			}
			for _, l := range h.Lines[from:to] {
				out = append(out, l.String())
			}
			return strings.Join(out, "\n"), true
		}
	}
	return "", false
}

// fileSnippet is the file's diff, its first limit lines.
func fileSnippet(f PRFile, limit int) string {
	out := []string{}
	for _, h := range f.Hunks {
		out = append(out, h.Header)
		for _, l := range h.Lines {
			out = append(out, l.String())
		}
	}
	if f.Binary && len(out) == 0 {
		return "(binarni file)"
	}
	if len(out) > limit {
		out = append(out[:limit], fmt.Sprintf("… (još %d linija)", len(out)-limit))
	}
	return strings.Join(out, "\n")
}

// AskReview sends a question on a line (side new or old) or a file (side
// ”, line 0) of a ready review to its session, queued while that works.
func (a *App) AskReview(ctx context.Context, reviewID int64, q store.ReviewQuestion) (store.ReviewQuestion, error) {
	r, err := a.Store.Review(ctx, reviewID)
	if err != nil {
		return q, err
	}
	q.ReviewID, q.Question, q.Path = reviewID, strings.TrimSpace(q.Question), strings.TrimSpace(q.Path)
	switch {
	case q.Question == "":
		return q, fmt.Errorf("%w: question is required", ErrBadReview)
	case q.Path == "":
		return q, fmt.Errorf("%w: path is required", ErrBadReview)
	case q.Line < 0 || (q.Line == 0) != (q.Side == "") || (q.Side != "" && q.Side != "new" && q.Side != "old"):
		return q, fmt.Errorf("%w: a line question has side new or old and a line; a file question neither", ErrBadReview)
	}
	if a.ReviewStatusOf(ctx, r) != ReviewReady || r.SessionID == nil {
		return q, ErrReviewNotReady
	}
	sess, err := a.Store.Session(ctx, *r.SessionID)
	if err != nil {
		return q, err
	}
	if err := own(sess); err != nil {
		return q, err
	}
	var n ReviewNotes
	if err := json.Unmarshal([]byte(r.Notes), &n); err != nil {
		return q, err
	}
	prompt, err := QuestionPrompt(r, n, q)
	if err != nil {
		return q, err
	}
	if q, err = a.Store.AddReviewQuestion(ctx, q); err != nil {
		return q, err
	}
	if _, err := a.SendMessage(ctx, sess.ID, prompt); err != nil {
		_ = a.Store.DeleteReviewQuestion(ctx, q.ID)
		return q, err
	}
	return q, nil
}

var shortRe = regexp.MustCompile(`(?s)<kratko>(.*?)</kratko>`)

// ShortAnswerLen is about how much of a reply without a <kratko> block
// stands for its short answer.
const ShortAnswerLen = 300

// SplitAnswer takes the short answer out of the agent's reply: its last
// <kratko> block, or else about its first ShortAnswerLen characters. The
// full answer is the reply without the blocks.
func SplitAnswer(reply string) (short, full string) {
	blocks := shortRe.FindAllStringSubmatch(reply, -1)
	full = strings.TrimSpace(shortRe.ReplaceAllString(reply, ""))
	if len(blocks) > 0 {
		if short = strings.TrimSpace(blocks[len(blocks)-1][1]); short != "" {
			return short, full
		}
	}
	if r := []rune(strings.Join(strings.Fields(full), " ")); len(r) > ShortAnswerLen {
		return strings.TrimSpace(string(r[:ShortAnswerLen])) + "…", full
	} else {
		return string(r), full
	}
}

// answerQuestion settles the review question a turn of a review session
// was for with how the turn ended: its reply, or its error. An interrupted
// turn leaves it pending for the resumed turn.
func (a *App) answerQuestion(ctx context.Context, sess store.Session, status, errMsg string) {
	var id int64
	var err error
	switch status {
	case store.StatusIdle:
		reply, rerr := a.lastReply(ctx, sess.ID)
		if rerr != nil {
			id, err = a.Store.AnswerReviewQuestion(ctx, sess.ID, store.QuestionFailed, "", "", "Agent nije napisao odgovor.")
			break
		}
		short, full := SplitAnswer(reply)
		id, err = a.Store.AnswerReviewQuestion(ctx, sess.ID, store.QuestionAnswered, short, full, "")
	case store.StatusError:
		if errMsg == "" {
			errMsg = a.turnError(ctx, sess.ID)
		}
		id, err = a.Store.AnswerReviewQuestion(ctx, sess.ID, store.QuestionFailed, "", "", errMsg)
	case store.StatusCancelled:
		id, err = a.Store.AnswerReviewQuestion(ctx, sess.ID, store.QuestionFailed, "", "", "Prekinuto.")
	default:
		return
	}
	if err != nil {
		log.Printf("session %d: review question %d: %v", sess.ID, id, err)
	}
}

// turnError is the last error the session's latest turn recorded.
func (a *App) turnError(ctx context.Context, sessionID int64) string {
	msgs, err := a.Store.Messages(ctx, sessionID, 0)
	if err == nil {
		for i := len(msgs) - 1; i >= 0 && msgs[i].Role != "user"; i-- {
			if msgs[i].Kind == "error" && strings.TrimSpace(msgs[i].Content) != "" {
				return msgs[i].Content
			}
		}
	}
	return "Agent turn failed."
}

// answersQuestion reports whether a turn of the session with this prompt
// answers a review question: one a question started, or its resumption.
func answersQuestion(sess store.Session, prompt string) bool {
	return sess.Trigger == ReviewTrigger && (strings.HasPrefix(prompt, QuestionPrefix) || prompt == ResumePrompt)
}
