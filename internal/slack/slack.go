// Package slack is a minimal client for the Slack Web API: who the bot is, a
// channel's info, history and thread replies, posting a message, and user names.
package slack

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

var (
	// ErrNotInChannel: the bot is not a member of the channel.
	ErrNotInChannel = errors.New("bot is not in the channel")
	// ErrChannelNotFound: the channel does not exist, or the token can't see it.
	ErrChannelNotFound = errors.New("channel not found")
	// ErrAuth: the token is invalid, revoked or expired.
	ErrAuth = errors.New("slack token not accepted")
	// ErrRateLimited: Slack answered 429 again after the one retry.
	ErrRateLimited = errors.New("rate limited")
)

// authErrors are the ok:false error codes that mean the token is no good.
var authErrors = map[string]bool{
	"not_authed": true, "invalid_auth": true, "account_inactive": true,
	"token_revoked": true, "token_expired": true, "no_permission": true,
}

type Client struct {
	Token   string
	BaseURL string // default https://slack.com/api
	HTTP    *http.Client

	mu    sync.Mutex
	names map[string]string // user id → name, see UserName
}

// Identity is who the token belongs to (auth.test).
type Identity struct {
	UserID string `json:"user_id"`
	BotID  string `json:"bot_id"`
	User   string `json:"user"`
	Team   string `json:"team"`
}

type Channel struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	IsMember bool   `json:"is_member"`
}

// Message is a message of a channel or a thread. A thread's root has
// ThreadTS == TS, ReplyCount and LatestReply; a reply has ThreadTS of its root.
type Message struct {
	TS          string `json:"ts"`
	User        string `json:"user"`
	BotID       string `json:"bot_id"`
	Subtype     string `json:"subtype"`
	Text        string `json:"text"`
	ThreadTS    string `json:"thread_ts"`
	ReplyCount  int    `json:"reply_count"`
	LatestReply string `json:"latest_reply"`
}

// archiveURL matches https://<workspace>.slack.com/archives/<channel>[/p…].
var archiveURL = regexp.MustCompile(`slack\.com/archives/([A-Za-z0-9]+)`)

// ChannelID takes a channel's id or archive URL and returns its id.
func ChannelID(s string) string {
	s = strings.TrimSpace(s)
	if m := archiveURL.FindStringSubmatch(s); m != nil {
		return m[1]
	}
	return s
}

// AuthTest returns who the token belongs to.
func (c *Client) AuthTest(ctx context.Context) (Identity, error) {
	var out Identity
	err := c.call(ctx, "auth.test", url.Values{}, &out)
	return out, err
}

// ConversationInfo returns the channel's name and whether the bot is in it.
func (c *Client) ConversationInfo(ctx context.Context, channel string) (Channel, error) {
	var out struct {
		Channel Channel `json:"channel"`
	}
	err := c.call(ctx, "conversations.info", url.Values{"channel": {ChannelID(channel)}}, &out)
	return out.Channel, err
}

// History returns the channel's top-level messages after oldest (a ts; empty:
// from the start), newest first as Slack gives them, following the cursor
// until there are no more or limit messages are in (limit <= 0: no cap).
func (c *Client) History(ctx context.Context, channel, oldest string, limit int) ([]Message, error) {
	q := url.Values{"channel": {ChannelID(channel)}}
	if oldest != "" {
		q.Set("oldest", oldest)
	}
	return c.pages(ctx, "conversations.history", q, limit)
}

// Replies returns the thread's messages after oldest (empty: all), oldest
// first; Slack always puts the thread's root first.
func (c *Client) Replies(ctx context.Context, channel, threadTS, oldest string) ([]Message, error) {
	q := url.Values{"channel": {ChannelID(channel)}, "ts": {threadTS}}
	if oldest != "" {
		q.Set("oldest", oldest)
	}
	return c.pages(ctx, "conversations.replies", q, 0)
}

// pageSize is what one history or replies call asks for; Slack's maximum is 1000
// but it recommends no more than 200.
const pageSize = 200

func (c *Client) pages(ctx context.Context, method string, q url.Values, limit int) ([]Message, error) {
	var all []Message
	for {
		n := pageSize
		if limit > 0 {
			n = min(n, limit-len(all))
		}
		q.Set("limit", strconv.Itoa(n))
		var out struct {
			Messages []Message `json:"messages"`
			Meta     struct {
				NextCursor string `json:"next_cursor"`
			} `json:"response_metadata"`
		}
		if err := c.call(ctx, method, q, &out); err != nil {
			return all, err
		}
		all = append(all, out.Messages...)
		if limit > 0 && len(all) >= limit {
			return all[:limit], nil
		}
		if out.Meta.NextCursor == "" {
			return all, nil
		}
		q.Set("cursor", out.Meta.NextCursor)
	}
}

// MaxMarkdown is the most characters PostMessage takes in one message.
const MaxMarkdown = 12000

// PostMessage posts markdown to the channel, in the thread of threadTS unless
// it is empty, and returns the new message's ts. Slack renders the Markdown
// itself (markdown_text), up to MaxMarkdown characters.
func (c *Client) PostMessage(ctx context.Context, channel, markdown, threadTS string) (string, error) {
	q := url.Values{"channel": {ChannelID(channel)}, "markdown_text": {markdown}}
	if threadTS != "" {
		q.Set("thread_ts", threadTS)
	}
	var out struct {
		TS string `json:"ts"`
	}
	err := c.call(ctx, "chat.postMessage", q, &out)
	return out.TS, err
}

// UserName returns the name the user shows as: display name, else real name,
// else user name. Names are kept in memory once looked up.
func (c *Client) UserName(ctx context.Context, userID string) (string, error) {
	c.mu.Lock()
	name, ok := c.names[userID]
	c.mu.Unlock()
	if ok {
		return name, nil
	}
	var out struct {
		User struct {
			Name     string `json:"name"`
			RealName string `json:"real_name"`
			Profile  struct {
				DisplayName string `json:"display_name"`
			} `json:"profile"`
		} `json:"user"`
	}
	if err := c.call(ctx, "users.info", url.Values{"user": {userID}}, &out); err != nil {
		return "", err
	}
	u := out.User
	name = cmp.Or(u.Profile.DisplayName, u.RealName, u.Name, userID)
	c.mu.Lock()
	if c.names == nil {
		c.names = map[string]string{}
	}
	c.names[userID] = name
	c.mu.Unlock()
	return name, nil
}

// call POSTs the form to the method and decodes the answer into out. On 429 it
// waits Retry-After and tries once more, unless ctx ends first.
func (c *Client) call(ctx context.Context, method string, form url.Values, out any) error {
	res, err := c.post(ctx, method, form)
	if err == nil && res.StatusCode == http.StatusTooManyRequests {
		res.Body.Close()
		wait, aerr := strconv.Atoi(res.Header.Get("Retry-After"))
		if aerr != nil || wait < 0 {
			wait = 1
		}
		t := time.NewTimer(time.Duration(wait) * time.Second)
		select {
		case <-ctx.Done():
			t.Stop()
			return fmt.Errorf("slack: %s: %w", method, ctx.Err())
		case <-t.C:
		}
		res, err = c.post(ctx, method, form)
	}
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode == http.StatusTooManyRequests {
		return fmt.Errorf("slack: %s: %w", method, ErrRateLimited)
	}
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("slack: %s: %s", method, res.Status)
	}
	raw := json.RawMessage{}
	if err := json.NewDecoder(res.Body).Decode(&raw); err != nil {
		return fmt.Errorf("slack: %s: %w", method, err)
	}
	var status struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(raw, &status); err != nil {
		return fmt.Errorf("slack: %s: %w", method, err)
	}
	if !status.OK {
		switch {
		case status.Error == "not_in_channel":
			return fmt.Errorf("slack: %s: %w", method, ErrNotInChannel)
		case status.Error == "channel_not_found":
			return fmt.Errorf("slack: %s: %w", method, ErrChannelNotFound)
		case authErrors[status.Error]:
			return fmt.Errorf("slack: %s: %w (%s)", method, ErrAuth, status.Error)
		}
		return fmt.Errorf("slack: %s: %s", method, status.Error)
	}
	return json.Unmarshal(raw, out)
}

func (c *Client) post(ctx context.Context, method string, form url.Values) (*http.Response, error) {
	base := c.BaseURL
	if base == "" {
		base = "https://slack.com/api"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/"+method, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	// In a header rather than the form or URL, so it stays out of error messages.
	req.Header.Set("Authorization", "Bearer "+c.Token)
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	return hc.Do(req)
}
