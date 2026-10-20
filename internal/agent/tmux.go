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
	"path/filepath"
	"strings"
	"time"
)

// TmuxRunner keeps one interactive claude per app session in a detached tmux
// session, so a session can be attached to and taken over at any time.
//
// A turn types the prompt into the pane (or starts claude with it). Claude's
// SessionStart and Stop hooks append their JSON input to an events file: that
// gives the transcript path and marks the end of a turn. Transcript lines are
// read from claude's own JSONL file.
type TmuxRunner struct {
	Tmux      string   // defaults to "tmux"
	Socket    string   // tmux -L socket name; empty uses the default server
	Claude    string   // defaults to "claude"
	ExtraArgs []string // appended to every claude start
	StateDir  string   // where hook event files live
	Poll      time.Duration
}

// eventsEnv names the file the hooks append to; set in claude's environment.
const eventsEnv = "ASSISTANT_EVENTS"

const hookCmd = `cat >> "$` + eventsEnv + `"; echo >> "$` + eventsEnv + `"`

var hookSettings = func() string {
	h := []map[string]any{{"hooks": []map[string]string{{"type": "command", "command": hookCmd}}}}
	raw, _ := json.Marshal(map[string]any{"hooks": map[string]any{"SessionStart": h, "Stop": h}})
	return string(raw)
}()

// AttachCommand is the shell command that opens the session's tmux pane.
func (r TmuxRunner) AttachCommand(key string) string {
	if r.Socket != "" {
		return fmt.Sprintf("tmux -L %s attach -t %s", r.Socket, key)
	}
	return "tmux attach -t " + key
}

func (r TmuxRunner) tmux(ctx context.Context, args ...string) ([]byte, error) {
	return r.tmuxIn(ctx, "", args...)
}

func (r TmuxRunner) tmuxIn(ctx context.Context, stdin string, args ...string) ([]byte, error) {
	bin := r.Tmux
	if bin == "" {
		bin = "tmux"
	}
	if r.Socket != "" {
		args = append([]string{"-L", r.Socket}, args...)
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Stdin = strings.NewReader(stdin)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return out, fmt.Errorf("tmux %s: %w: %s", args[0], err, strings.TrimSpace(string(out)))
	}
	return out, nil
}

func (r TmuxRunner) alive(ctx context.Context, name string) bool {
	_, err := r.tmux(ctx, "has-session", "-t", "="+name)
	return err == nil
}

// StartArgs is the command run inside a new tmux session.
func (r TmuxRunner) StartArgs(req Request, events string) []string {
	claude := r.Claude
	if claude == "" {
		claude = "claude"
	}
	// env drops variables that make claude think it is nested in another session.
	args := []string{"env", "-u", "CLAUDECODE", "-u", "CLAUDE_CODE_ENTRYPOINT", eventsEnv + "=" + events,
		claude, "--settings", hookSettings}
	if req.ResumeID != "" {
		args = append(args, "--resume", req.ResumeID)
	}
	return append(append(args, commonArgs(req, r.ExtraArgs)...), req.Prompt)
}

func (r TmuxRunner) Run(ctx context.Context, req Request, emit func(Event)) error {
	if req.Key == "" {
		return errors.New("tmux runner needs a session key")
	}
	if err := os.MkdirAll(r.StateDir, 0o700); err != nil {
		return err
	}
	events := filepath.Join(r.StateDir, req.Key+".events")
	before, err := readHookEvents(events)
	if err != nil {
		return err
	}
	// Read the known transcript from its current end; a new one from the start.
	offsets := map[string]int64{}
	if before.transcript != "" {
		if fi, err := os.Stat(before.transcript); err == nil {
			offsets[before.transcript] = fi.Size()
		}
	}

	started := !r.alive(ctx, req.Key)
	if started {
		dir := req.Workdir
		if dir == "" {
			dir, _ = os.UserHomeDir()
		}
		args := append([]string{"new-session", "-d", "-s", req.Key, "-c", dir, "-x", "200", "-y", "50"},
			r.StartArgs(req, events)...)
		if _, err := r.tmux(ctx, args...); err != nil {
			return err
		}
	} else if err := r.typePrompt(ctx, req.Key, req.Prompt); err != nil {
		return err
	}

	poll := r.Poll
	if poll == 0 {
		poll = 300 * time.Millisecond
	}
	tick := time.NewTicker(poll)
	defer tick.Stop()
	sessionID := before.sessionID
	var trustKeyAt time.Time
	lastText := ""
	track := func(ev Event) {
		if ev.Role == "assistant" && ev.Kind == "text" {
			lastText = ev.Content
		}
		emit(ev)
	}
	var stoppedAt time.Time
	for i := 1; ; i++ {
		ev, err := readHookEvents(events)
		if err != nil {
			return err
		}
		if ev.sessionID != "" && ev.sessionID != sessionID {
			sessionID = ev.sessionID
			emit(Event{Role: "system", Kind: "init", ClaudeSessionID: sessionID})
		}
		if ev.transcript != "" {
			off, err := emitTranscript(ev.transcript, offsets[ev.transcript], track)
			if err != nil {
				return err
			}
			offsets[ev.transcript] = off
		}
		// Claude can write the final message to the transcript just after the
		// Stop hook runs, so keep reading until it shows up.
		if ev.stops > before.stops && stoppedAt.IsZero() {
			stoppedAt = time.Now()
		}
		if !stoppedAt.IsZero() && (ev.lastMessage == "" || lastText == ev.lastMessage || time.Since(stoppedAt) > 5*time.Second) {
			emit(Event{Role: "system", Kind: "result", Content: ev.lastMessage, ClaudeSessionID: sessionID})
			return nil
		}
		// A new workdir shows the folder trust prompt before anything else;
		// accept it like print mode does. One key at a time, judged by the
		// screen, because keys sent while the prompt draws get lost.
		if started && ev.starts == before.starts && time.Since(trustKeyAt) > time.Second {
			if out, err := r.tmux(ctx, "capture-pane", "-p", "-t", req.Key); err == nil {
				if key := trustPromptKey(string(out)); key != "" {
					r.tmux(ctx, "send-keys", "-t", req.Key, key)
					trustKeyAt = time.Now()
				}
			}
		}
		if i%10 == 0 && !r.alive(ctx, req.Key) {
			return fmt.Errorf("tmux session %s ended", req.Key)
		}
		select {
		case <-ctx.Done():
			// Escape interrupts the turn; claude itself keeps running.
			r.tmux(context.Background(), "send-keys", "-t", req.Key, "Escape")
			return ctx.Err()
		case <-tick.C:
		}
	}
}

// typePrompt submits a prompt to the running claude. Multi-line text goes in
// as a bracketed paste so newlines don't submit early.
func (r TmuxRunner) typePrompt(ctx context.Context, name, prompt string) error {
	if !strings.Contains(prompt, "\n") {
		if _, err := r.tmux(ctx, "send-keys", "-t", name, "-l", prompt); err != nil {
			return err
		}
	} else {
		buf := name + "-prompt"
		if _, err := r.tmuxIn(ctx, prompt, "load-buffer", "-b", buf, "-"); err != nil {
			return err
		}
		if _, err := r.tmux(ctx, "paste-buffer", "-p", "-d", "-b", buf, "-t", name); err != nil {
			return err
		}
	}
	// Give the TUI a moment to take the text before submitting it.
	time.Sleep(300 * time.Millisecond)
	_, err := r.tmux(ctx, "send-keys", "-t", name, "Enter")
	return err
}

// trustPromptKey returns the key that moves claude's folder trust prompt
// towards "Yes", or "" when the screen shows no such prompt.
func trustPromptKey(screen string) string {
	if !strings.Contains(screen, "Yes, I trust this folder") {
		return ""
	}
	for _, line := range strings.Split(screen, "\n") {
		if strings.Contains(line, "❯") {
			if strings.Contains(line, "Yes, I trust this folder") {
				return "Enter"
			}
			return "Down"
		}
	}
	return ""
}

type hookState struct {
	starts, stops int
	sessionID     string
	transcript    string
	lastMessage   string
}

// readHookEvents summarises the hook events file. A missing file is empty.
func readHookEvents(path string) (hookState, error) {
	var st hookState
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return st, nil
	}
	if err != nil {
		return st, err
	}
	for _, line := range bytes.Split(raw, []byte("\n")) {
		var ev struct {
			Event       string `json:"hook_event_name"`
			SessionID   string `json:"session_id"`
			Transcript  string `json:"transcript_path"`
			LastMessage string `json:"last_assistant_message"`
		}
		if json.Unmarshal(line, &ev) != nil {
			continue
		}
		switch ev.Event {
		case "SessionStart":
			st.starts++
			st.sessionID, st.transcript = ev.SessionID, ev.Transcript
		case "Stop":
			st.stops++
			st.lastMessage = ev.LastMessage
		}
	}
	return st, nil
}

// emitTranscript emits events for complete lines after offset and returns the
// new offset. The prompt itself is skipped: the caller already recorded it.
func emitTranscript(path string, offset int64, emit func(Event)) (int64, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return offset, nil
	}
	if err != nil {
		return offset, err
	}
	defer f.Close()
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return offset, err
	}
	rd := bufio.NewReaderSize(f, 1<<20)
	for {
		line, err := rd.ReadBytes('\n')
		if err != nil {
			// A partial last line is read again once claude finishes it.
			return offset, nil
		}
		offset += int64(len(line))
		for _, ev := range ParseLine(line) {
			if ev.Role == "user" && ev.Kind == "text" {
				continue
			}
			emit(ev)
		}
	}
}
