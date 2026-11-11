package app

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/url"
	"path"
	"strings"
)

// The page shows a PR's .plain specs as specs, not as a diff: the whole
// module at the head commit with its changes marked, the unchanged parts
// folded, and every :Concept: explained on hover. SaveReview fetches the
// head content of the PR's .plain files and of the modules they import or
// require, so their definitions are at hand too.

// MaxReviewSpecs is the most .plain modules a review fetches.
const MaxReviewSpecs = 40

// IsSpec reports whether the path is a PlainLang spec.
func IsSpec(p string) bool { return strings.HasSuffix(strings.ToLower(p), ".plain") }

// fetchSpecs returns the head content of the diff's .plain files (but
// deleted ones) and, transitively, of the modules they import or require,
// by path. What cannot be fetched is left out (and logged): the page then
// shows that file from its diff only.
func (a *App) fetchSpecs(ctx context.Context, prURL, head string, files []PRFile) map[string]string {
	var todo []string
	for _, f := range files {
		if IsSpec(f.Path) && f.Status != "deleted" {
			todo = append(todo, f.Path)
		}
	}
	if len(todo) == 0 {
		return nil
	}
	owner, repo, _, ok := ParsePRURL(prURL)
	if !ok || head == "" {
		log.Printf("review specs: no repo or head commit (%q, %q)", prURL, head)
		return nil
	}
	tree := a.specTree(ctx, owner, repo, head)
	out := map[string]string{}
	seen := map[string]bool{}
	for len(todo) > 0 && len(seen) < MaxReviewSpecs {
		p := todo[0]
		todo = todo[1:]
		if seen[p] {
			continue
		}
		seen[p] = true
		raw, err := a.gh(ctx, "api", "-H", "Accept: application/vnd.github.raw", contentsPath(owner, repo, p, head))
		if err != nil {
			log.Printf("review specs: %s: %v", p, err)
			continue
		}
		out[p] = string(raw)
		for _, ref := range SpecRefs(string(raw)) {
			if dep := resolveSpec(p, ref, tree); dep != "" && !seen[dep] {
				todo = append(todo, dep)
			}
		}
	}
	return out
}

func contentsPath(owner, repo, p, ref string) string {
	segs := strings.Split(p, "/")
	for i, s := range segs {
		segs[i] = url.PathEscape(s)
	}
	return fmt.Sprintf("repos/%s/%s/contents/%s?ref=%s", owner, repo, strings.Join(segs, "/"), url.QueryEscape(ref))
}

// specTree is the set of .plain paths in the repository at ref, nil if
// gh cannot list it.
func (a *App) specTree(ctx context.Context, owner, repo, ref string) map[string]bool {
	raw, err := a.gh(ctx, "api", fmt.Sprintf("repos/%s/%s/git/trees/%s?recursive=1", owner, repo, url.PathEscape(ref)))
	if err != nil {
		log.Printf("review specs: tree: %v", err)
		return nil
	}
	var t struct {
		Tree []struct {
			Path string `json:"path"`
			Type string `json:"type"`
		} `json:"tree"`
	}
	if err := json.Unmarshal(raw, &t); err != nil {
		log.Printf("review specs: tree: %v", err)
		return nil
	}
	set := map[string]bool{}
	for _, e := range t.Tree {
		if e.Type == "blob" && IsSpec(e.Path) {
			set[e.Path] = true
		}
	}
	return set
}

// resolveSpec finds the module a reference of the spec at from names: ref
// with .plain in from's directory or the nearest of its parents that has
// it ("" if none does).
func resolveSpec(from, ref string, tree map[string]bool) string {
	ref = strings.TrimSuffix(strings.TrimSpace(ref), ".plain") + ".plain"
	for dir := path.Dir(from); ; dir = path.Dir(dir) {
		p := path.Clean(path.Join(dir, ref))
		if tree[p] {
			return p
		}
		if dir == "." || dir == "/" {
			return ""
		}
	}
}

// SpecRefs are the modules a spec's frontmatter lists under import and
// requires, as written: block lists (- x) or flow lists ([x, y]).
func SpecRefs(text string) []string {
	lines := strings.Split(text, "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[0]) != "---" {
		return nil
	}
	var refs []string
	in := false
	unquote := func(s string) string { return strings.Trim(strings.TrimSpace(s), `"'`) }
	for _, l := range lines[1:] {
		l = strings.TrimRight(l, "\r")
		if strings.TrimSpace(l) == "---" {
			break
		}
		if l != "" && l[0] != ' ' && l[0] != '-' {
			key, val, _ := strings.Cut(l, ":")
			in = key == "import" || key == "requires"
			val = strings.TrimSpace(val)
			if in && strings.HasPrefix(val, "[") {
				for _, x := range strings.Split(strings.Trim(val, "[]"), ",") {
					if x = unquote(x); x != "" {
						refs = append(refs, x)
					}
				}
			} else if in && val != "" {
				refs = append(refs, unquote(val))
			}
			continue
		}
		if t := strings.TrimSpace(l); in && strings.HasPrefix(t, "- ") {
			refs = append(refs, unquote(t[2:]))
		}
	}
	return refs
}

// specSnippet is the head content of the spec around a line (1-based),
// for a question on a line the diff does not show.
func specSnippet(src string, line, around int) (string, bool) {
	lines := strings.Split(src, "\n")
	if line < 1 || line > len(lines) {
		return "", false
	}
	from, to := max(0, line-1-around), min(len(lines), line+around)
	out := make([]string, 0, to-from)
	for i := from; i < to; i++ {
		mark := "  "
		if i == line-1 {
			mark = "> "
		}
		out = append(out, fmt.Sprintf("%s%d: %s", mark, i+1, lines[i]))
	}
	return strings.Join(out, "\n"), true
}
