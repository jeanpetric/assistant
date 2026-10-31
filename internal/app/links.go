package app

import (
	"context"
	"log"
	"regexp"
	"strings"

	"github.com/jeanpetric/assistant/internal/agent"
)

// linkKinds are the links the app recognises, by the key of the field that
// keeps them: a task's link of each kind is filled in from its description,
// and pull_request also from the PR an agent opens.
var linkKinds = []struct {
	key string
	re  *regexp.Regexp
}{
	{"pull_request", regexp.MustCompile(`^https://github\.com/[^/]+/[^/]+/pull/\d+`)},
	{"productive", regexp.MustCompile(`^https://app\.productive\.io/.*/tasks?/`)},
	{"slack", regexp.MustCompile(`^https://[^/]+\.slack\.com/archives/`)},
}

// LinkFields returns the link fields the text fills: per kind of link, the
// first such URL in it, for a url field of that key that is defined and not
// among have (the task's fields, or keys a caller set or cleared on purpose).
func (a *App) LinkFields(ctx context.Context, text string, have map[string]string) map[string]string {
	defs, err := a.Store.FieldDefs(ctx)
	if err != nil {
		log.Printf("link fields: %v", err)
		return nil
	}
	urlField := map[string]bool{}
	for _, d := range defs {
		urlField[d.Key] = d.Type == "url"
	}
	out := map[string]string{}
	for _, u := range urlRe.FindAllString(text, -1) {
		u = strings.TrimRight(u, ".,;:!?*_`")
		for _, k := range linkKinds {
			if _, ok := have[k.key]; ok || !urlField[k.key] || out[k.key] != "" || !k.re.MatchString(u) {
				continue
			}
			out[k.key] = u
		}
	}
	return out
}

// WithLinkFields adds to fields the link fields the description fills.
func (a *App) WithLinkFields(ctx context.Context, description string, fields map[string]string) map[string]string {
	add := a.LinkFields(ctx, description, fields)
	if len(add) == 0 {
		return fields
	}
	if fields == nil {
		fields = map[string]string{}
	}
	for k, v := range add {
		fields[k] = v
	}
	return fields
}

var prURLRe = regexp.MustCompile(`https://github\.com/[^/\s]+/[^/\s]+/pull/\d+`)

// openedPRs watches a turn's events for pull requests the agent opens: the PR
// URL in the result of a tool call that creates one (gh pr create, or a
// create_pull_request tool).
type openedPRs struct {
	creating bool
	urls     []string
}

func (o *openedPRs) see(ev agent.Event) {
	switch ev.Kind {
	case "tool_use":
		o.creating = strings.Contains(ev.Content, "gh pr create") || strings.Contains(ev.Content, "create_pull_request")
	case "tool_result":
		if o.creating {
			o.urls = append(o.urls, prURLRe.FindAllString(ev.Content, -1)...)
		}
		o.creating = false
	}
}

// recordPR keeps the first PR a turn opened in its task's pull_request field,
// if that is empty.
func (a *App) recordPR(ctx context.Context, taskID int64, prs openedPRs) {
	if taskID == 0 || len(prs.urls) == 0 {
		return
	}
	t, err := a.Store.Task(ctx, taskID)
	if err != nil {
		return
	}
	add := a.LinkFields(ctx, prs.urls[0], t.Fields)
	if add["pull_request"] == "" {
		return
	}
	t.Fields = map[string]string{"pull_request": add["pull_request"]}
	if err := a.Store.UpdateTask(ctx, t); err != nil {
		log.Printf("task %d: record PR: %v", taskID, err)
	}
}
