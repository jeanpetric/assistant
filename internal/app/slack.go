package app

import (
	"context"

	"github.com/jeanpetric/assistant/internal/slack"
)

// SlackClient is the part of the Slack Web API the Slack integration uses.
type SlackClient interface {
	AuthTest(ctx context.Context) (slack.Identity, error)
	ConversationInfo(ctx context.Context, channel string) (slack.Channel, error)
	History(ctx context.Context, channel, oldest string, limit int) ([]slack.Message, error)
	Replies(ctx context.Context, channel, threadTS, oldest string) ([]slack.Message, error)
	PostMessage(ctx context.Context, channel, markdown, threadTS string) (string, error)
	UserName(ctx context.Context, userID string) (string, error)
}

var _ SlackClient = (*slack.Client)(nil)
