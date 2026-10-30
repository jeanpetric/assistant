package app

import (
	"context"
	"strings"

	"github.com/jeanpetric/assistant/internal/slack"
	"github.com/jeanpetric/assistant/internal/store"
)

// SlackClient is the part of the Slack Web API the Slack integration uses.
type SlackClient interface {
	AuthTest(ctx context.Context) (slack.Identity, error)
	ConversationInfo(ctx context.Context, channel string) (slack.Channel, error)
	History(ctx context.Context, channel, oldest string, limit int) ([]slack.Message, error)
	Replies(ctx context.Context, channel, threadTS, oldest string) ([]slack.Message, error)
	PostMessage(ctx context.Context, channel, markdown, threadTS string) (string, error)
	AddReaction(ctx context.Context, channel, ts, name string) error
	UserName(ctx context.Context, userID string) (string, error)
}

var _ SlackClient = (*slack.Client)(nil)

// WithSlack fills in how the last poll of the task's channel went, if the
// task is connected to one, and the channel's link: in the workspace once
// known, else through Slack's redirect.
func (a *App) WithSlack(t *store.Task) {
	if t.Slack == nil {
		return
	}
	a.mu.Lock()
	st, base := a.slackStatus[t.ID], a.slackURL
	a.mu.Unlock()
	t.Slack.LastSyncAt = st.LastSyncAt
	var errs []string
	for _, e := range []string{st.Error, st.PostError, st.Undelivered} {
		if e != "" {
			errs = append(errs, e)
		}
	}
	t.Slack.Error = strings.Join(errs, "; ")
	if base != "" {
		t.Slack.URL = strings.TrimSuffix(base, "/") + "/archives/" + t.Slack.ChannelID
	} else {
		t.Slack.URL = "https://slack.com/app_redirect?channel=" + t.Slack.ChannelID
	}
}
