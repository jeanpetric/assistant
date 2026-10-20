package agent

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// fakeInteractive stands in for interactive claude: it answers the prompt from
// its arguments, then every line typed into the pane, writing the transcript
// and hook events the way claude does. A prompt of "hang" never finishes.
const fakeInteractive = `#!/bin/sh
dir=$(dirname "$0")
printf '%s\n' "$@" > "$dir/args"
tr="$dir/transcript.jsonl"
ev() { printf '{"hook_event_name":"%s","session_id":"fake-1","transcript_path":"%s","last_assistant_message":"%s"}\n' "$1" "$tr" "$2" >> "$ASSISTANT_EVENTS"; }
turn() {
	printf '{"type":"user","message":{"role":"user","content":"%s"}}\n' "$1" >> "$tr"
	[ "$1" = hang ] && return
	ev Stop "echo: $1"
	sleep 0.2 # like claude, the last message can land after the Stop hook
	printf '{"type":"assistant","message":{"content":[{"type":"text","text":"echo: %s"}]}}\n' "$1" >> "$tr"
}
ev SessionStart
for last; do :; done
turn "$last"
while read -r line; do turn "$line"; done
`

func tmuxRunner(t *testing.T) (TmuxRunner, string) {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	dir := t.TempDir()
	claude := filepath.Join(dir, "fake-claude")
	if err := os.WriteFile(claude, []byte(fakeInteractive), 0o755); err != nil {
		t.Fatal(err)
	}
	r := TmuxRunner{Socket: fmt.Sprintf("assistant-test-%d", os.Getpid()), Claude: claude,
		StateDir: filepath.Join(dir, "state"), Poll: 20 * time.Millisecond}
	t.Cleanup(func() { exec.Command("tmux", "-L", r.Socket, "kill-server").Run() })
	return r, dir
}

func runTurn(t *testing.T, r TmuxRunner, ctx context.Context, req Request) ([]Event, error) {
	t.Helper()
	var events []Event
	err := r.Run(ctx, req, func(e Event) { events = append(events, e) })
	return events, err
}

func TestTmuxRunnerTurns(t *testing.T) {
	r, dir := tmuxRunner(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req := Request{Key: "assistant-1", Prompt: "hello", Workdir: dir, Model: "haiku", Mode: "plan"}

	got, err := runTurn(t, r, ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	want := []Event{
		{Role: "system", Kind: "init", ClaudeSessionID: "fake-1"},
		{Role: "assistant", Kind: "text", Content: "echo: hello"},
		{Role: "system", Kind: "result", Content: "echo: hello", ClaudeSessionID: "fake-1"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("first turn\ngot  %+v\nwant %+v", got, want)
	}
	args, _ := os.ReadFile(filepath.Join(dir, "args"))
	if s := string(args); !strings.Contains(s, "--model\nhaiku\n--permission-mode\nplan\nhello\n") || !strings.Contains(s, `"Stop"`) {
		t.Errorf("claude args: %q", s)
	}

	// The follow-up is typed into the running claude; only new lines come back.
	got, err = runTurn(t, r, ctx, Request{Key: "assistant-1", Prompt: "again", ResumeID: "fake-1"})
	if err != nil {
		t.Fatal(err)
	}
	want = []Event{
		{Role: "assistant", Kind: "text", Content: "echo: again"},
		{Role: "system", Kind: "result", Content: "echo: again", ClaudeSessionID: "fake-1"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("follow-up\ngot  %+v\nwant %+v", got, want)
	}
	if r.AttachCommand("assistant-1") != "tmux -L "+r.Socket+" attach -t assistant-1" {
		t.Errorf("attach: %s", r.AttachCommand("assistant-1"))
	}
}

func TestTmuxRunnerResumesDeadSession(t *testing.T) {
	r, dir := tmuxRunner(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := runTurn(t, r, ctx, Request{Key: "assistant-2", Prompt: "one", Workdir: dir}); err != nil {
		t.Fatal(err)
	}
	r.tmux(ctx, "kill-session", "-t", "assistant-2")

	got, err := runTurn(t, r, ctx, Request{Key: "assistant-2", Prompt: "two", Workdir: dir, ResumeID: "fake-1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Content != "echo: two" {
		t.Fatalf("got %+v", got)
	}
	args, _ := os.ReadFile(filepath.Join(dir, "args"))
	if !strings.Contains(string(args), "--resume\nfake-1\n") {
		t.Errorf("claude args: %q", args)
	}
}

func TestTmuxRunnerCancel(t *testing.T) {
	r, dir := tmuxRunner(t)
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(300*time.Millisecond, cancel)
	_, err := runTurn(t, r, ctx, Request{Key: "assistant-3", Prompt: "hang", Workdir: dir})
	if err != context.Canceled {
		t.Fatalf("got %v", err)
	}
	if !r.alive(context.Background(), "assistant-3") {
		t.Error("cancel should leave the tmux session running")
	}
}

func TestTmuxRunnerSessionEnds(t *testing.T) {
	r, dir := tmuxRunner(t)
	r.Claude = "true"
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := runTurn(t, r, ctx, Request{Key: "assistant-4", Prompt: "x", Workdir: dir})
	if err == nil || !strings.Contains(err.Error(), "ended") {
		t.Fatalf("got %v", err)
	}
}

func TestTrustPromptKey(t *testing.T) {
	prompt := " Quick safety check: Is this a project you created or one you trust?\n %s No, exit\n %s Yes, I trust this folder\n Enter to confirm"
	for screen, want := range map[string]string{
		fmt.Sprintf(prompt, "❯", " "): "Down",
		fmt.Sprintf(prompt, " ", "❯"): "Enter",
		fmt.Sprintf(prompt, " ", " "): "",
		"❯ hello":                     "",
	} {
		if got := trustPromptKey(screen); got != want {
			t.Errorf("%q: got %q, want %q", screen, got, want)
		}
	}
}
