package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// UsageURL is the endpoint Claude Code's /usage command reads. It is not a
// documented API and costs no quota.
const UsageURL = "https://api.anthropic.com/api/oauth/usage"

// UsageClient reads the subscription's usage windows with Claude Code's own
// OAuth login. It only reads the token and never refreshes it: refreshing
// would rotate the token under Claude Code. An expired token fails until the
// claude CLI next runs and refreshes it.
type UsageClient struct {
	URL    string                 // defaults to UsageURL
	Token  func() (string, error) // defaults to ClaudeOAuthToken
	Client *http.Client           // defaults to a client with a 20s timeout
}

func (c UsageClient) Usage(ctx context.Context) (RateLimits, error) {
	url, tokenFn, client := c.URL, c.Token, c.Client
	if url == "" {
		url = UsageURL
	}
	if tokenFn == nil {
		tokenFn = ClaudeOAuthToken
	}
	if client == nil {
		client = &http.Client{Timeout: 20 * time.Second}
	}
	token, err := tokenFn()
	if err != nil {
		return RateLimits{}, err
	}
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return RateLimits{}, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("anthropic-beta", "oauth-2025-04-20")
	resp, err := client.Do(req)
	if err != nil {
		return RateLimits{}, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return RateLimits{}, err
	}
	if resp.StatusCode == http.StatusUnauthorized {
		return RateLimits{}, errors.New("usage: unauthorized (Claude Code login expired? run claude once)")
	}
	if resp.StatusCode != http.StatusOK {
		return RateLimits{}, fmt.Errorf("usage: %s: %s", resp.Status, strings.TrimSpace(string(body[:min(len(body), 200)])))
	}
	return parseUsage(body)
}

type usageWindow struct {
	Utilization *float64  `json:"utilization"` // 0–100
	ResetsAt    time.Time `json:"resets_at"`
}

func (w *usageWindow) window() *LimitWindow {
	if w == nil || w.Utilization == nil || w.ResetsAt.IsZero() {
		return nil
	}
	return &LimitWindow{UsedPercent: *w.Utilization, ResetsAt: w.ResetsAt}
}

func parseUsage(body []byte) (RateLimits, error) {
	var u struct {
		FiveHour *usageWindow `json:"five_hour"`
		SevenDay *usageWindow `json:"seven_day"`
	}
	if err := json.Unmarshal(body, &u); err != nil {
		return RateLimits{}, fmt.Errorf("usage: %w", err)
	}
	lim := RateLimits{FiveHour: u.FiveHour.window(), SevenDay: u.SevenDay.window()}
	if lim.FiveHour == nil && lim.SevenDay == nil {
		return lim, errors.New("usage: response has no five_hour or seven_day window")
	}
	return lim, nil
}

// ClaudeOAuthToken returns the access token Claude Code logged in with: from
// .credentials.json in the Claude config dir, else (macOS) the login keychain.
func ClaudeOAuthToken() (string, error) {
	dir := os.Getenv("CLAUDE_CONFIG_DIR")
	if dir == "" {
		home, _ := os.UserHomeDir()
		dir = filepath.Join(home, ".claude")
	}
	raw, err := os.ReadFile(filepath.Join(dir, ".credentials.json"))
	if errors.Is(err, os.ErrNotExist) && runtime.GOOS == "darwin" {
		raw, err = exec.Command("security", "find-generic-password", "-s", "Claude Code-credentials", "-w").Output()
		if err != nil {
			return "", fmt.Errorf("claude credentials not found in keychain: %w", err)
		}
	}
	if err != nil {
		return "", fmt.Errorf("claude credentials: %w", err)
	}
	return oauthToken(raw)
}

func oauthToken(raw []byte) (string, error) {
	var c struct {
		ClaudeAiOauth struct {
			AccessToken string `json:"accessToken"`
		} `json:"claudeAiOauth"`
	}
	if err := json.Unmarshal(raw, &c); err != nil {
		return "", fmt.Errorf("claude credentials: %w", err)
	}
	if c.ClaudeAiOauth.AccessToken == "" {
		return "", errors.New("claude credentials: no OAuth login (API key users have no subscription usage)")
	}
	return c.ClaudeAiOauth.AccessToken, nil
}
