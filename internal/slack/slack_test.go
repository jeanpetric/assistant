package slack

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// fake is a Slack API server: answer gives the JSON body for a method and its
// form; every call is recorded as "method form".
type fake struct {
	t      *testing.T
	mu     sync.Mutex
	calls  []string
	answer func(method string, form url.Values) string
}

func (f *fake) start() *Client {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if a := r.Header.Get("Authorization"); a != "Bearer xoxb-secret" {
			f.t.Errorf("Authorization = %q", a)
		}
		if r.Method != http.MethodPost || r.URL.RawQuery != "" {
			f.t.Errorf("request %s %s", r.Method, r.URL)
		}
		r.ParseForm()
		method := strings.TrimPrefix(r.URL.Path, "/api/")
		f.mu.Lock()
		f.calls = append(f.calls, method+" "+r.PostForm.Encode())
		f.mu.Unlock()
		w.Write([]byte(f.answer(method, r.PostForm)))
	}))
	f.t.Cleanup(srv.Close)
	return &Client{Token: "xoxb-secret", BaseURL: srv.URL + "/api"}
}

func TestChannelID(t *testing.T) {
	for in, want := range map[string]string{
		"https://theproductivecompany.slack.com/archives/C0CDK1XJUL9":                   "C0CDK1XJUL9",
		"https://theproductivecompany.slack.com/archives/C0CDK1XJUL9/p1761820000123456": "C0CDK1XJUL9",
		"theproductivecompany.slack.com/archives/G01ABC?thread_ts=1.2":                  "G01ABC",
		" C0CDK1XJUL9 ": "C0CDK1XJUL9",
	} {
		if got := ChannelID(in); got != want {
			t.Errorf("ChannelID(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestAuthTestAndConversationInfo(t *testing.T) {
	f := &fake{t: t, answer: func(method string, _ url.Values) string {
		if method == "auth.test" {
			return `{"ok":true,"user_id":"U0CDK1MUPR7","bot_id":"B0CEBFM6XFS","user":"jeanbot","team":"The Productive Company"}`
		}
		return `{"ok":true,"channel":{"id":"C1","name":"general","is_member":true}}`
	}}
	c := f.start()
	ctx := context.Background()
	id, err := c.AuthTest(ctx)
	if err != nil || id != (Identity{UserID: "U0CDK1MUPR7", BotID: "B0CEBFM6XFS", User: "jeanbot", Team: "The Productive Company"}) {
		t.Fatalf("AuthTest = %+v, %v", id, err)
	}
	ch, err := c.ConversationInfo(ctx, "https://x.slack.com/archives/C1/p123")
	if err != nil || ch != (Channel{ID: "C1", Name: "general", IsMember: true}) {
		t.Fatalf("ConversationInfo = %+v, %v", ch, err)
	}
	if want := []string{"auth.test ", "conversations.info channel=C1"}; strings.Join(f.calls, "|") != strings.Join(want, "|") {
		t.Fatalf("calls = %q", f.calls)
	}
}

func TestHistoryFollowsCursorUpToLimit(t *testing.T) {
	pages := map[string]string{
		"":   `{"ok":true,"messages":[{"ts":"5.0","user":"U1","text":"hi","thread_ts":"5.0","reply_count":2,"latest_reply":"7.0"},{"ts":"4.0","bot_id":"B1","text":"bot"}],"has_more":true,"response_metadata":{"next_cursor":"c2"}}`,
		"c2": `{"ok":true,"messages":[{"ts":"3.0","user":"U2","subtype":"channel_join","text":"joined"}],"has_more":true,"response_metadata":{"next_cursor":"c3"}}`,
		"c3": `{"ok":true,"messages":[{"ts":"2.0","user":"U1","text":"old"},{"ts":"1.5","user":"U1","text":"older"}],"response_metadata":{"next_cursor":""}}`,
	}
	f := &fake{t: t, answer: func(_ string, form url.Values) string { return pages[form.Get("cursor")] }}
	c := f.start()

	msgs, err := c.History(context.Background(), "C1", "1.0", 0)
	if err != nil || len(msgs) != 5 {
		t.Fatalf("History = %+v, %v", msgs, err)
	}
	want := Message{TS: "5.0", User: "U1", Text: "hi", ThreadTS: "5.0", ReplyCount: 2, LatestReply: "7.0"}
	if msgs[0] != want || msgs[1].BotID != "B1" || msgs[2].Subtype != "channel_join" || msgs[4].Text != "older" {
		t.Fatalf("messages = %+v", msgs)
	}
	if want := "conversations.history channel=C1&limit=200&oldest=1.0|conversations.history channel=C1&cursor=c2&limit=200&oldest=1.0|conversations.history channel=C1&cursor=c3&limit=200&oldest=1.0"; strings.Join(f.calls, "|") != want {
		t.Fatalf("calls = %q", f.calls)
	}

	// The limit caps the whole result: no page is asked for more than is left.
	f.calls = nil
	msgs, err = c.History(context.Background(), "C1", "", 4)
	if err != nil || len(msgs) != 4 || msgs[3].TS != "2.0" {
		t.Fatalf("History limit 4 = %+v, %v", msgs, err)
	}
	if want := "conversations.history channel=C1&limit=4|conversations.history channel=C1&cursor=c2&limit=2|conversations.history channel=C1&cursor=c3&limit=1"; strings.Join(f.calls, "|") != want {
		t.Fatalf("calls = %q", f.calls)
	}
}

func TestReplies(t *testing.T) {
	f := &fake{t: t, answer: func(_ string, form url.Values) string {
		if form.Get("cursor") == "" {
			return `{"ok":true,"messages":[{"ts":"5.0","thread_ts":"5.0","reply_count":2,"text":"root"},{"ts":"6.0","thread_ts":"5.0","user":"U1","text":"a"}],"response_metadata":{"next_cursor":"n"}}`
		}
		return `{"ok":true,"messages":[{"ts":"7.0","thread_ts":"5.0","user":"U2","text":"b"}]}`
	}}
	c := f.start()
	msgs, err := c.Replies(context.Background(), "C1", "5.0", "5.5")
	if err != nil || len(msgs) != 3 || msgs[1].ThreadTS != "5.0" || msgs[2].Text != "b" {
		t.Fatalf("Replies = %+v, %v", msgs, err)
	}
	if want := "conversations.replies channel=C1&limit=200&oldest=5.5&ts=5.0|conversations.replies channel=C1&cursor=n&limit=200&oldest=5.5&ts=5.0"; strings.Join(f.calls, "|") != want {
		t.Fatalf("calls = %q", f.calls)
	}
}

func TestPostMessage(t *testing.T) {
	f := &fake{t: t, answer: func(_ string, form url.Values) string {
		return `{"ok":true,"channel":"C1","ts":"9.0` + form.Get("thread_ts") + `"}`
	}}
	c := f.start()
	ctx := context.Background()
	if ts, err := c.PostMessage(ctx, "C1", "hello & bye", ""); err != nil || ts != "9.0" {
		t.Fatalf("PostMessage = %q, %v", ts, err)
	}
	if ts, err := c.PostMessage(ctx, "C1", "in thread", "5.0"); err != nil || ts != "9.05.0" {
		t.Fatalf("PostMessage in thread = %q, %v", ts, err)
	}
	if want := "chat.postMessage channel=C1&markdown_text=hello+%26+bye|chat.postMessage channel=C1&markdown_text=in+thread&thread_ts=5.0"; strings.Join(f.calls, "|") != want {
		t.Fatalf("calls = %q", f.calls)
	}
}

func TestUserNameIsCached(t *testing.T) {
	f := &fake{t: t, answer: func(_ string, form url.Values) string {
		switch form.Get("user") {
		case "U1":
			return `{"ok":true,"user":{"name":"jean","real_name":"Jean Petric","profile":{"display_name":"Jean"}}}`
		case "U2":
			return `{"ok":true,"user":{"name":"ana","real_name":"Ana K","profile":{"display_name":""}}}`
		}
		return `{"ok":false,"error":"user_not_found"}`
	}}
	c := f.start()
	ctx := context.Background()
	for _, want := range [][2]string{{"U1", "Jean"}, {"U2", "Ana K"}, {"U1", "Jean"}, {"U2", "Ana K"}} {
		if got, err := c.UserName(ctx, want[0]); err != nil || got != want[1] {
			t.Fatalf("UserName(%s) = %q, %v", want[0], got, err)
		}
	}
	if _, err := c.UserName(ctx, "U3"); err == nil || !strings.Contains(err.Error(), "user_not_found") {
		t.Fatalf("unknown user: %v", err)
	}
	if len(f.calls) != 3 {
		t.Fatalf("calls = %q, want one per user", f.calls)
	}
}

func TestOkFalseErrors(t *testing.T) {
	for code, want := range map[string]error{
		"not_in_channel":    ErrNotInChannel,
		"channel_not_found": ErrChannelNotFound,
		"invalid_auth":      ErrAuth,
		"token_revoked":     ErrAuth,
		"not_authed":        ErrAuth,
		"account_inactive":  ErrAuth,
	} {
		f := &fake{t: t, answer: func(string, url.Values) string { return `{"ok":false,"error":"` + code + `"}` }}
		_, err := f.start().History(context.Background(), "C1", "", 0)
		if !errors.Is(err, want) || strings.Contains(err.Error(), "secret") {
			t.Errorf("%s: err = %v", code, err)
		}
	}
	f := &fake{t: t, answer: func(string, url.Values) string { return `{"ok":false,"error":"missing_scope"}` }}
	_, err := f.start().PostMessage(context.Background(), "C1", "x", "")
	if err == nil || err.Error() != "slack: chat.postMessage: missing_scope" {
		t.Fatalf("err = %v", err)
	}
}

func TestTokenStaysOutOfErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Even a server that echoes the request back can't put the token in the error.
		http.Error(w, r.Header.Get("Authorization"), http.StatusInternalServerError)
	}))
	defer srv.Close()
	c := &Client{Token: "xoxb-secret", BaseURL: srv.URL}
	_, err := c.AuthTest(context.Background())
	if err == nil || !strings.Contains(err.Error(), "500") || strings.Contains(err.Error(), "secret") {
		t.Fatalf("err = %v", err)
	}
	srv.Close() // connection refused: the URL is in the error, the token is not
	_, err = c.ConversationInfo(context.Background(), "C1")
	if err == nil || strings.Contains(err.Error(), "secret") {
		t.Fatalf("err = %v", err)
	}
}

func TestRateLimitRetriesOnce(t *testing.T) {
	var n int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n++
		if n == 1 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.Write([]byte(`{"ok":true,"user_id":"U1"}`))
	}))
	defer srv.Close()
	c := &Client{Token: "xoxb-secret", BaseURL: srv.URL}
	if id, err := c.AuthTest(context.Background()); err != nil || id.UserID != "U1" || n != 2 {
		t.Fatalf("AuthTest = %+v, %v after %d calls", id, err, n)
	}
}

func TestRateLimitGivesUpAfterOneRetry(t *testing.T) {
	var n int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n++
		w.Header().Set("Retry-After", "0")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()
	c := &Client{BaseURL: srv.URL}
	if _, err := c.AuthTest(context.Background()); !errors.Is(err, ErrRateLimited) || n != 2 {
		t.Fatalf("err = %v after %d calls", err, n)
	}
}

func TestRateLimitWaitEndsWithContext(t *testing.T) {
	var n int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n++
		w.Header().Set("Retry-After", "60")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()
	c := &Client{BaseURL: srv.URL}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := c.AuthTest(ctx)
	if !errors.Is(err, context.DeadlineExceeded) || n != 1 || time.Since(start) > 5*time.Second {
		t.Fatalf("err = %v after %d calls, %v", err, n, time.Since(start))
	}
}
