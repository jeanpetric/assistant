// Package agent runs AI coding agent turns. ClaudeRunner drives the
// Claude Code CLI in print mode and translates its stream-json output.
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
)

// Event is one transcript item produced by an agent turn.
type Event struct {
	Role    string // user | assistant | system
	Kind    string // text | tool_use | tool_result | result | error
	Content string

	// Set on the init and result events.
	ClaudeSessionID string
	CostUSD         float64
}

type Request struct {
	Prompt   string
	Workdir  string
	Mode     string // claude --permission-mode value; empty uses CLI default
	Model    string // claude --model value; empty uses CLI default
	ResumeID string // continue an existing claude session
}

type Runner interface {
	// Run executes one turn, calling emit for each event in order.
	Run(ctx context.Context, req Request, emit func(Event)) error
}

// Permission modes understood by the claude CLI.
var Modes = []string{"bypassPermissions", "acceptEdits", "plan", "default"}

// Models are suggestions for --model; any alias or full model name is accepted.
var Models = []string{"opus", "sonnet", "haiku", "fable"}

type ClaudeRunner struct {
	Command   string   // defaults to "claude"
	ExtraArgs []string // appended to every invocation
}

func (r ClaudeRunner) Args(req Request) []string {
	args := []string{"-p", req.Prompt, "--output-format", "stream-json", "--verbose"}
	if req.ResumeID != "" {
		args = append(args, "--resume", req.ResumeID)
	}
	if req.Mode != "" && req.Mode != "default" {
		args = append(args, "--permission-mode", req.Mode)
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
	cmd := exec.CommandContext(ctx, command, r.Args(req)...)
	cmd.Dir = req.Workdir
	cmd.Env = cleanEnv(os.Environ())
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
			for _, ev := range ParseLine(line) {
				emit(ev)
			}
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
	Message   struct {
		Content json.RawMessage `json:"content"`
	} `json:"message"`
}

type block struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	Content   json.RawMessage `json:"content"`
	IsError   bool            `json:"is_error"`
	ToolUseID string          `json:"tool_use_id"`
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
			return []Event{{Role: "system", Kind: "init", ClaudeSessionID: l.SessionID}}
		}
	case "assistant", "user":
		return contentEvents(l.Type, l.Message.Content)
	case "result":
		ev := Event{Role: "system", Kind: "result", Content: l.Result, ClaudeSessionID: l.SessionID, CostUSD: l.CostUSD}
		if l.IsError {
			ev.Kind = "error"
			if ev.Content == "" {
				ev.Content = l.Subtype
			}
		}
		return []Event{ev}
	}
	return nil
}

func contentEvents(role string, raw json.RawMessage) []Event {
	if len(raw) == 0 {
		return nil
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return []Event{{Role: role, Kind: "text", Content: s}}
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
				out = append(out, Event{Role: role, Kind: "text", Content: b.Text})
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
