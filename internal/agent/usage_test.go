package agent

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestUsageClient(t *testing.T) {
	var auth, beta string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth, beta = r.Header.Get("Authorization"), r.Header.Get("anthropic-beta")
		w.Write([]byte(`{"five_hour":{"utilization":18.0,"resets_at":"2026-10-20T11:59:59.852253+00:00"},
			"seven_day":{"utilization":4.5,"resets_at":"2026-10-26T07:59:59+00:00"},"seven_day_opus":null,"extra":{}}`))
	}))
	defer srv.Close()
	c := UsageClient{URL: srv.URL, Token: func() (string, error) { return "tok", nil }}
	got, err := c.Usage(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if auth != "Bearer tok" || beta == "" {
		t.Errorf("headers: %q %q", auth, beta)
	}
	if got.FiveHour == nil || got.FiveHour.UsedPercent != 18 || !got.FiveHour.ResetsAt.Equal(time.Date(2026, 10, 20, 11, 59, 59, 852253000, time.UTC)) {
		t.Errorf("five_hour = %+v", got.FiveHour)
	}
	if got.SevenDay == nil || got.SevenDay.UsedPercent != 4.5 {
		t.Errorf("seven_day = %+v", got.SevenDay)
	}
}

func TestUsageClientErrors(t *testing.T) {
	for name, tc := range map[string]struct {
		status int
		body   string
		want   string
	}{
		"unauthorized": {401, `{}`, "unauthorized"},
		"server":       {500, `oops`, "500"},
		"bad json":     {200, `{`, "usage"},
		"no windows":   {200, `{"five_hour":null,"seven_day":{"utilization":null}}`, "no five_hour"},
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(tc.status)
			w.Write([]byte(tc.body))
		}))
		c := UsageClient{URL: srv.URL, Token: func() (string, error) { return "tok", nil }}
		if _, err := c.Usage(context.Background()); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v", name, err)
		}
		srv.Close()
	}
	c := UsageClient{URL: "http://127.0.0.1:1", Token: func() (string, error) { return "", errors.New("no login") }}
	if _, err := c.Usage(context.Background()); err == nil || err.Error() != "no login" {
		t.Errorf("token error: %v", err)
	}
}

func TestClaudeOAuthTokenFromFile(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	os.WriteFile(filepath.Join(dir, ".credentials.json"), []byte(`{"claudeAiOauth":{"accessToken":"abc","refreshToken":"r"}}`), 0o600)
	if tok, err := ClaudeOAuthToken(); err != nil || tok != "abc" {
		t.Fatalf("token = %q, %v", tok, err)
	}
	os.WriteFile(filepath.Join(dir, ".credentials.json"), []byte(`{"mcpOAuth":{}}`), 0o600)
	if _, err := ClaudeOAuthToken(); err == nil {
		t.Fatal("expected an error without an OAuth login")
	}
}
