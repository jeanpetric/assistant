package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
)

// ErrNoFork: the gemini CLI cannot continue a session in a copy.
var ErrNoFork = errors.New("gemini cannot fork a session")

// GeminiRunner drives the Gemini CLI in headless mode (prompt on stdin, not a
// TTY) with --output-format stream-json.
type GeminiRunner struct {
	Command   string   // defaults to "gemini"
	ExtraArgs []string // appended to every invocation
}

// geminiModes maps Modes to the gemini CLI's --approval-mode values.
var geminiModes = map[string]string{
	"bypassPermissions": "yolo",
	"acceptEdits":       "auto_edit",
	"plan":              "plan",
}

// Args are the CLI arguments for req. Like ClaudeRunner, the prompt goes to stdin.
// --skip-trust: headless runs cannot answer the folder trust prompt, and the
// session's workdir was chosen by the user.
func (r GeminiRunner) Args(req Request) []string {
	args := []string{"--output-format", "stream-json", "--skip-trust"}
	if req.ResumeID != "" {
		args = append(args, "--resume", req.ResumeID)
	}
	if m := geminiModes[req.Mode]; m != "" {
		args = append(args, "--approval-mode", m)
	}
	args = append(args, r.ExtraArgs...)
	if req.Model != "" {
		args = append(args, "--model", req.Model)
	}
	return args
}

func (r GeminiRunner) Run(ctx context.Context, req Request, emit func(Event)) error {
	if req.Fork {
		return ErrNoFork
	}
	command := r.Command
	if command == "" {
		command = "gemini"
	}
	// The gemini CLI can only replace its system prompt, not add to it, so
	// the addition leads the turn's prompt instead.
	if req.SystemPrompt != "" {
		req.Prompt = req.SystemPrompt + "\n\n---\n\n" + req.Prompt
	}
	var p GeminiParser
	err := runCLI(ctx, command, r.Args(req), req, func(line []byte) {
		for _, ev := range p.Line(line) {
			emit(ev)
		}
	}, emit)
	for _, ev := range p.Flush() {
		emit(ev)
	}
	return err
}

// GeminiParser converts the gemini CLI's stream-json lines into events. The
// assistant's text arrives in chunks; they are joined into one text event,
// which is emitted when something else happens in the stream.
type GeminiParser struct {
	text strings.Builder
	last string // the latest text event: the turn's answer once it ends
}

type geminiLine struct {
	Type       string          `json:"type"`
	SessionID  string          `json:"session_id"`
	Model      string          `json:"model"`
	Role       string          `json:"role"`
	Content    string          `json:"content"`
	ToolName   string          `json:"tool_name"`
	Parameters json.RawMessage `json:"parameters"`
	Status     string          `json:"status"`
	Output     string          `json:"output"`
	Severity   string          `json:"severity"`
	Message    string          `json:"message"`
	Error      *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

// Line converts one stream-json line. Unknown or malformed lines yield no events.
func (p *GeminiParser) Line(line []byte) []Event {
	var l geminiLine
	if err := json.Unmarshal(line, &l); err != nil {
		return nil
	}
	if l.Type == "message" {
		// The user message echoes the prompt, which the app records itself.
		if l.Role == "assistant" {
			p.text.WriteString(l.Content)
		}
		return nil
	}
	out := p.Flush()
	switch l.Type {
	case "init":
		if l.SessionID != "" {
			out = append(out, Event{Role: "system", Kind: "init", ClaudeSessionID: l.SessionID, Model: l.Model})
		}
	case "tool_use":
		out = append(out, Event{Role: "assistant", Kind: "tool_use", Content: l.ToolName + " " + compactJSON(l.Parameters)})
	case "tool_result":
		c := l.Output
		if l.Status == "error" {
			msg := c
			if l.Error != nil {
				msg = l.Error.Message
			}
			c = "[error] " + msg
		}
		out = append(out, Event{Role: "tool", Kind: "tool_result", Content: truncate(c)})
	case "error":
		// Non-fatal: the result says whether the turn failed.
		out = append(out, Event{Role: "system", Kind: "context", Content: "[" + l.Severity + "] " + l.Message})
	case "result":
		ev := Event{Role: "system", Kind: "result", Content: p.last}
		if l.Status == "error" {
			ev.Kind, ev.Content = "error", "error"
			if l.Error != nil {
				ev.Content = l.Error.Message
			}
		}
		out = append(out, ev)
	}
	return out
}

// Flush emits the assistant text gathered so far, if any.
func (p *GeminiParser) Flush() []Event {
	s := p.text.String()
	p.text.Reset()
	if strings.TrimSpace(s) == "" {
		return nil
	}
	p.last = s
	return []Event{{Role: "assistant", Kind: "text", Content: s}}
}
