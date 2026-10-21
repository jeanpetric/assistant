// Package agent runs AI coding agent turns. ClaudeRunner drives the
// Claude Code CLI in print mode and GeminiRunner the Gemini CLI in headless
// mode; both translate the CLI's stream-json output into Events.
package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"
)

// Event is one transcript item produced by an agent turn.
type Event struct {
	Role    string // user | assistant | tool | system
	Kind    string // text | context | tool_use | tool_result | result | error | init | rate_limit | usage
	Content string

	// Set on the init and result events.
	ClaudeSessionID string
	CostUSD         float64

	// Set on the init event: the model the session runs on.
	Model string

	// Set on usage events: how many tokens the session's context holds after
	// the agent's latest message.
	ContextTokens int

	// Set on the result event: the context window of each model the turn used,
	// subagents' models included.
	ContextWindows map[string]int

	// Set on rate_limit events.
	Limits *RateLimits

	// Set on the result event: tools the CLI refused to run without the
	// user's permission, which print mode cannot ask for.
	Denied []string
}

// RateLimits is the account's usage of its subscription windows as reported
// by the CLI. A nil window was not reported.
type RateLimits struct {
	FiveHour *LimitWindow `json:"five_hour"`
	SevenDay *LimitWindow `json:"seven_day"`
}

type LimitWindow struct {
	UsedPercent float64   `json:"used_percent"` // 0–100
	ResetsAt    time.Time `json:"resets_at"`
}

type Request struct {
	Agent    string // which CLI runs the turn, one of Agents; empty is claude
	Prompt   string
	Workdir  string
	Mode     string // claude --permission-mode value; empty uses CLI default
	Model    string // claude --model value; empty uses CLI default
	ResumeID string // continue an existing claude session
	Fork     bool   // with ResumeID: continue in a copy, leaving that session as it was
	// SystemPrompt is added to the CLI's own system prompt for this turn.
	SystemPrompt string
}

type Runner interface {
	// Run executes one turn, calling emit for each event in order.
	Run(ctx context.Context, req Request, emit func(Event)) error
}

// Agents are the CLIs a session can run on.
const (
	Claude = "claude"
	Gemini = "gemini"
)

var Agents = []string{Claude, Gemini}

// Permission modes, as the claude CLI names them; GeminiRunner maps them to
// its approval modes.
var Modes = []string{"bypassPermissions", "acceptEdits", "plan", "default"}

// Models are suggestions for --model; any alias or full model name is accepted.
var Models = []string{"opus", "sonnet", "haiku", "fable"}

// GeminiModels are suggestions for the gemini CLI's --model.
var GeminiModels = []string{"auto", "pro", "flash", "flash-lite"}

// ModelsFor are the model suggestions for agent.
func ModelsFor(agent string) []string {
	if agent == Gemini {
		return GeminiModels
	}
	return Models
}

// CanFork says whether agent can continue a session in a copy (Request.Fork).
func CanFork(agent string) bool { return agent == "" || agent == Claude }

// Runners runs each turn on the runner named by its Request.Agent.
type Runners map[string]Runner

func (rs Runners) Run(ctx context.Context, req Request, emit func(Event)) error {
	name := req.Agent
	if name == "" {
		name = Claude
	}
	r, ok := rs[name]
	if !ok {
		return fmt.Errorf("unknown agent %q", name)
	}
	return r.Run(ctx, req, emit)
}

type ClaudeRunner struct {
	Command   string   // defaults to "claude"
	ExtraArgs []string // appended to every invocation
}

// Args are the CLI arguments for req. The prompt is not among them: Run writes
// it to stdin, so a prompt starting with "-" is not read as an option.
func (r ClaudeRunner) Args(req Request) []string {
	args := []string{"-p", "--output-format", "stream-json", "--verbose"}
	if req.ResumeID != "" {
		args = append(args, "--resume", req.ResumeID)
		if req.Fork {
			args = append(args, "--fork-session")
		}
	}
	if req.Mode != "" && req.Mode != "default" {
		args = append(args, "--permission-mode", req.Mode)
	}
	if req.SystemPrompt != "" {
		args = append(args, "--append-system-prompt", req.SystemPrompt)
	}
	args = append(args, r.ExtraArgs...)
	// After ExtraArgs so a per-session model beats one from the config.
	if req.Model != "" {
		args = append(args, "--model", req.Model)
	}
	return args
}

func (r ClaudeRunner) Run(ctx context.Context, req Request, emit func(Event)) error {
	command := r.Command
	if command == "" {
		command = "claude"
	}
	return runCLI(ctx, command, r.Args(req), req, func(line []byte) {
		for _, ev := range ParseLine(line) {
			emit(ev)
		}
	}, emit)
}

// runCLI runs command in req.Workdir with the prompt on stdin and hands each
// non-blank stdout line to parse.
func runCLI(ctx context.Context, command string, args []string, req Request, parse func([]byte), emit func(Event)) error {
	cmd := exec.CommandContext(ctx, command, args...)
	cmd.Dir = req.Workdir
	cmd.Env = cleanEnv(os.Environ())
	cmd.Stdin = strings.NewReader(req.Prompt)
	var stderr tailBuffer
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	rd := bufio.NewReaderSize(stdout, 1<<20)
	for {
		line, err := rd.ReadBytes('\n')
		if len(bytes.TrimSpace(line)) > 0 {
			parse(line)
		}
		if err != nil {
			if !errors.Is(err, io.EOF) {
				emit(Event{Role: "system", Kind: "error", Content: "read output: " + err.Error()})
			}
			break
		}
	}
	if err := cmd.Wait(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return fmt.Errorf("%w: %s", err, msg)
		}
		return err
	}
	return nil
}

// cleanEnv drops variables that make the CLI think it is nested in another session.
func cleanEnv(env []string) []string {
	out := env[:0:0]
	for _, kv := range env {
		if strings.HasPrefix(kv, "CLAUDECODE=") || strings.HasPrefix(kv, "CLAUDE_CODE_ENTRYPOINT=") {
			continue
		}
		out = append(out, kv)
	}
	return out
}

const maxContent = 20000

func truncate(s string) string {
	if len(s) <= maxContent {
		return s
	}
	return s[:maxContent] + fmt.Sprintf("\n… [truncated %d bytes]", len(s)-maxContent)
}

type streamLine struct {
	Type      string  `json:"type"`
	Subtype   string  `json:"subtype"`
	SessionID string  `json:"session_id"`
	Result    string  `json:"result"`
	IsError   bool    `json:"is_error"`
	CostUSD   float64 `json:"total_cost_usd"`
	Denials   []struct {
		ToolName string `json:"tool_name"`
	} `json:"permission_denials"`
	Model string `json:"model"`
	// Set on the messages of subagents, whose context is not the session's.
	ParentToolUseID *string `json:"parent_tool_use_id"`
	Message         struct {
		Content json.RawMessage `json:"content"`
		Usage   *struct {
			InputTokens         int `json:"input_tokens"`
			CacheCreationTokens int `json:"cache_creation_input_tokens"`
			CacheReadTokens     int `json:"cache_read_input_tokens"`
			OutputTokens        int `json:"output_tokens"`
		} `json:"usage"`
	} `json:"message"`
	ModelUsage map[string]struct {
		ContextWindow int `json:"contextWindow"`
	} `json:"modelUsage"`
	RateLimitInfo struct {
		RateLimitType  string                  `json:"rateLimitType"`
		Utilization    *float64                `json:"utilization"`
		ResetsAt       int64                   `json:"resetsAt"`
		UnifiedWindows map[string]streamWindow `json:"unifiedWindows"`
	} `json:"rate_limit_info"`
}

type streamWindow struct {
	Utilization *float64 `json:"utilization"` // 0–1
	ResetsAt    int64    `json:"resetsAt"`    // unix seconds
}

func (w streamWindow) window() *LimitWindow {
	if w.Utilization == nil || w.ResetsAt == 0 {
		return nil
	}
	return &LimitWindow{UsedPercent: *w.Utilization * 100, ResetsAt: time.Unix(w.ResetsAt, 0)}
}

type block struct {
	Type    string          `json:"type"`
	Text    string          `json:"text"`
	Name    string          `json:"name"`
	Input   json.RawMessage `json:"input"`
	Content json.RawMessage `json:"content"`
	IsError bool            `json:"is_error"`
}

// ParseLine converts one stream-json line into transcript events.
// Unknown or malformed lines yield no events.
func ParseLine(line []byte) []Event {
	var l streamLine
	if err := json.Unmarshal(line, &l); err != nil {
		return nil
	}
	switch l.Type {
	case "system":
		if l.Subtype == "init" && l.SessionID != "" {
			return []Event{{Role: "system", Kind: "init", ClaudeSessionID: l.SessionID, Model: l.Model}}
		}
	case "assistant", "user":
		out := contentEvents(l.Type, l.Message.Content)
		// Everything the model was sent plus what it wrote is in the context
		// of the next request.
		if u := l.Message.Usage; u != nil && l.Type == "assistant" && l.ParentToolUseID == nil {
			n := u.InputTokens + u.CacheCreationTokens + u.CacheReadTokens + u.OutputTokens
			if n > 0 {
				out = append(out, Event{Role: "system", Kind: "usage", ContextTokens: n})
			}
		}
		return out
	case "result":
		ev := Event{Role: "system", Kind: "result", Content: l.Result, ClaudeSessionID: l.SessionID, CostUSD: l.CostUSD}
		for model, u := range l.ModelUsage {
			if u.ContextWindow > 0 {
				if ev.ContextWindows == nil {
					ev.ContextWindows = map[string]int{}
				}
				ev.ContextWindows[model] = u.ContextWindow
			}
		}
		for _, d := range l.Denials {
			ev.Denied = append(ev.Denied, d.ToolName)
		}
		if l.IsError {
			ev.Kind = "error"
			if ev.Content == "" {
				ev.Content = l.Subtype
			}
		}
		return []Event{ev}
	case "rate_limit_event":
		return rateLimitEvents(l)
	}
	return nil
}

// rateLimitEvents reads both windows from unifiedWindows; older CLIs only
// describe the window named by rateLimitType.
func rateLimitEvents(l streamLine) []Event {
	info := l.RateLimitInfo
	windows := info.UnifiedWindows
	if windows == nil && info.RateLimitType != "" {
		windows = map[string]streamWindow{info.RateLimitType: {Utilization: info.Utilization, ResetsAt: info.ResetsAt}}
	}
	lim := RateLimits{FiveHour: windows["five_hour"].window(), SevenDay: windows["seven_day"].window()}
	if lim.FiveHour == nil && lim.SevenDay == nil {
		return nil
	}
	return []Event{{Role: "system", Kind: "rate_limit", Limits: &lim}}
}

// contentEvents maps a message's content blocks. User text in the stream never is
// the user's own prompt (the app records that); it is what the CLI injects mid-turn,
// such as an expanded skill, so it becomes "context" rather than "text".
func contentEvents(role string, raw json.RawMessage) []Event {
	if len(raw) == 0 {
		return nil
	}
	textKind := "text"
	if role == "user" {
		textKind = "context"
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return []Event{{Role: role, Kind: textKind, Content: s}}
	}
	var blocks []block
	if json.Unmarshal(raw, &blocks) != nil {
		return nil
	}
	var out []Event
	for _, b := range blocks {
		switch b.Type {
		case "text":
			if strings.TrimSpace(b.Text) != "" {
				out = append(out, Event{Role: role, Kind: textKind, Content: b.Text})
			}
		case "tool_use":
			out = append(out, Event{Role: role, Kind: "tool_use", Content: b.Name + " " + compactJSON(b.Input)})
		case "tool_result":
			c := toolResultText(b.Content)
			if b.IsError {
				c = "[error] " + c
			}
			out = append(out, Event{Role: "tool", Kind: "tool_result", Content: truncate(c)})
		}
	}
	return out
}

func toolResultText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var parts []block
	if json.Unmarshal(raw, &parts) == nil {
		var texts []string
		for _, p := range parts {
			if p.Type == "text" {
				texts = append(texts, p.Text)
			} else {
				texts = append(texts, "["+p.Type+"]")
			}
		}
		return strings.Join(texts, "\n")
	}
	return string(raw)
}

func compactJSON(raw json.RawMessage) string {
	var buf bytes.Buffer
	if json.Compact(&buf, raw) != nil {
		return string(raw)
	}
	return truncate(buf.String())
}

// tailBuffer keeps the last few KB written to it.
type tailBuffer struct{ b []byte }

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.b = append(t.b, p...)
	if len(t.b) > 4096 {
		t.b = t.b[len(t.b)-4096:]
	}
	return len(p), nil
}

func (t *tailBuffer) String() string { return string(t.b) }
