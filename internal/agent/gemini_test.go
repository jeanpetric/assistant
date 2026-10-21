package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// Shaped like gemini 0.65 output (packages/core/src/output/types.ts).
const geminiTranscript = `{"type":"init","timestamp":"2026-10-21T10:00:00.000Z","session_id":"g-123","model":"gemini-2.5-pro"}
{"type":"message","timestamp":"2026-10-21T10:00:00.100Z","role":"user","content":"the prompt"}
{"type":"message","timestamp":"2026-10-21T10:00:01.000Z","role":"assistant","content":"Let me ","delta":true}
{"type":"message","timestamp":"2026-10-21T10:00:01.100Z","role":"assistant","content":"check.","delta":true}
{"type":"tool_use","timestamp":"2026-10-21T10:00:02.000Z","tool_name":"run_shell_command","tool_id":"t1","parameters":{"command": "echo hi"}}
{"type":"tool_result","timestamp":"2026-10-21T10:00:02.500Z","tool_id":"t1","status":"success","output":"hi"}
{"type":"tool_result","timestamp":"2026-10-21T10:00:02.600Z","tool_id":"t2","status":"error","error":{"type":"x","message":"boom"}}
{"type":"error","timestamp":"2026-10-21T10:00:02.700Z","severity":"warning","message":"loop detected"}
not json
{"type":"message","timestamp":"2026-10-21T10:00:03.000Z","role":"assistant","content":"Done.","delta":true}
{"type":"result","timestamp":"2026-10-21T10:00:04.000Z","status":"success","stats":{"total_tokens":100,"input_tokens":80,"output_tokens":20,"duration_ms":4000,"tool_calls":1}}
`

func geminiParseAll(s string) []Event {
	var p GeminiParser
	var out []Event
	for _, l := range strings.Split(s, "\n") {
		out = append(out, p.Line([]byte(l))...)
	}
	return append(out, p.Flush()...)
}

func TestGeminiParseTranscript(t *testing.T) {
	got := geminiParseAll(geminiTranscript)
	want := []Event{
		{Role: "system", Kind: "init", ClaudeSessionID: "g-123", Model: "gemini-2.5-pro"},
		{Role: "assistant", Kind: "text", Content: "Let me check."},
		{Role: "assistant", Kind: "tool_use", Content: `run_shell_command {"command":"echo hi"}`},
		{Role: "tool", Kind: "tool_result", Content: "hi"},
		{Role: "tool", Kind: "tool_result", Content: "[error] boom"},
		{Role: "system", Kind: "context", Content: "[warning] loop detected"},
		{Role: "assistant", Kind: "text", Content: "Done."},
		{Role: "system", Kind: "result", Content: "Done."},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got  %+v\nwant %+v", got, want)
	}
}

func TestGeminiParseErrorResult(t *testing.T) {
	got := geminiParseAll(`{"type":"message","role":"assistant","content":"partial","delta":true}
{"type":"result","status":"error","error":{"type":"FatalTurnLimitedError","message":"Reached max session turns"}}`)
	want := []Event{
		{Role: "assistant", Kind: "text", Content: "partial"},
		{Role: "system", Kind: "error", Content: "Reached max session turns"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v", got)
	}
}

func TestGeminiArgs(t *testing.T) {
	r := GeminiRunner{ExtraArgs: []string{"--model", "pro"}}
	got := r.Args(Request{Prompt: "hi", ResumeID: "s1", Mode: "bypassPermissions", Model: "flash"})
	want := []string{"--output-format", "stream-json", "--skip-trust", "--resume", "s1", "--approval-mode", "yolo", "--model", "pro", "--model", "flash"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v", got)
	}
	for mode, flag := range map[string]string{"acceptEdits": "auto_edit", "plan": "plan"} {
		if got := (GeminiRunner{}).Args(Request{Mode: mode}); got[len(got)-1] != flag {
			t.Errorf("%s: %v", mode, got)
		}
	}
	if got := (GeminiRunner{}).Args(Request{Mode: "default"}); strings.Contains(strings.Join(got, " "), "approval") {
		t.Fatalf("default mode should not pass a flag: %v", got)
	}
}

func TestGeminiRunnerWithFakeCLI(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "fake-gemini")
	body := "#!/bin/sh\necho \"$@\" > \"$(dirname \"$0\")/args\"\ncat > \"$(dirname \"$0\")/stdin\"\ncat <<'EOF'\n" + geminiTranscript + "EOF\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	var events []Event
	// Through Runners, as the app calls it.
	rs := Runners{Claude: ClaudeRunner{Command: "/nonexistent"}, Gemini: GeminiRunner{Command: script}}
	err := rs.Run(context.Background(), Request{Agent: Gemini, Prompt: "- do it", Workdir: t.TempDir()}, func(e Event) {
		events = append(events, e)
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 8 || events[7].Kind != "result" || events[0].ClaudeSessionID != "g-123" {
		t.Fatalf("unexpected events: %+v", events)
	}
	if in, _ := os.ReadFile(filepath.Join(dir, "stdin")); string(in) != "- do it" {
		t.Errorf("prompt on stdin = %q", in)
	}
	if args, _ := os.ReadFile(filepath.Join(dir, "args")); !strings.HasPrefix(string(args), "--output-format stream-json") {
		t.Errorf("args = %q", args)
	}
}

func TestGeminiRunnerRefusesFork(t *testing.T) {
	err := GeminiRunner{Command: "/nonexistent"}.Run(context.Background(), Request{ResumeID: "s", Fork: true}, func(Event) {})
	if !errors.Is(err, ErrNoFork) {
		t.Fatalf("got %v", err)
	}
	if CanFork(Gemini) || !CanFork(Claude) || !CanFork("") {
		t.Fatal("CanFork")
	}
}

func TestRunnersUnknownAgent(t *testing.T) {
	if err := (Runners{}).Run(context.Background(), Request{Agent: "codex"}, func(Event) {}); err == nil {
		t.Fatal("expected an error")
	}
	// Empty is claude.
	called := false
	rs := Runners{Claude: runnerFunc(func() { called = true })}
	if err := rs.Run(context.Background(), Request{}, func(Event) {}); err != nil || !called {
		t.Fatalf("err %v, called %v", err, called)
	}
}

type runnerFunc func()

func (f runnerFunc) Run(context.Context, Request, func(Event)) error { f(); return nil }
