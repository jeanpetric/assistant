package agent

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const transcript = `{"type":"system","subtype":"init","session_id":"abc-123","tools":["Bash"]}
{"type":"assistant","message":{"content":[{"type":"text","text":"Let me check."},{"type":"tool_use","id":"t1","name":"Bash","input":{"command": "echo hi"}}]}}
{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t1","content":"hi"}]}}
{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t2","is_error":true,"content":[{"type":"text","text":"boom"}]}]}}
not json
{"type":"assistant","message":{"content":[{"type":"thinking","thinking":"hmm"},{"type":"text","text":"Done."}]}}
{"type":"result","subtype":"success","is_error":false,"result":"Done.","session_id":"abc-123","total_cost_usd":0.0125}
`

func parseAll(s string) []Event {
	var out []Event
	for _, l := range strings.Split(s, "\n") {
		out = append(out, ParseLine([]byte(l))...)
	}
	return out
}

func TestParseTranscript(t *testing.T) {
	got := parseAll(transcript)
	want := []Event{
		{Role: "system", Kind: "init", ClaudeSessionID: "abc-123"},
		{Role: "assistant", Kind: "text", Content: "Let me check."},
		{Role: "assistant", Kind: "tool_use", Content: `Bash {"command":"echo hi"}`},
		{Role: "tool", Kind: "tool_result", Content: "hi"},
		{Role: "tool", Kind: "tool_result", Content: "[error] boom"},
		{Role: "assistant", Kind: "text", Content: "Done."},
		{Role: "system", Kind: "result", Content: "Done.", ClaudeSessionID: "abc-123", CostUSD: 0.0125},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got  %+v\nwant %+v", got, want)
	}
}

func TestParseErrorResult(t *testing.T) {
	got := ParseLine([]byte(`{"type":"result","subtype":"error_max_turns","is_error":true,"session_id":"x"}`))
	if len(got) != 1 || got[0].Kind != "error" || got[0].Content != "error_max_turns" {
		t.Fatalf("got %+v", got)
	}
}

func TestArgs(t *testing.T) {
	r := ClaudeRunner{ExtraArgs: []string{"--model", "opus"}}
	got := r.Args(Request{Prompt: "hi", ResumeID: "s1", Mode: "plan"})
	want := []string{"-p", "hi", "--output-format", "stream-json", "--verbose", "--resume", "s1", "--permission-mode", "plan", "--model", "opus"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v", got)
	}
	got = r.Args(Request{Prompt: "hi", Model: "sonnet"})
	if want := []string{"-p", "hi", "--output-format", "stream-json", "--verbose", "--model", "opus", "--model", "sonnet"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("session model should come last: %v", got)
	}
	if got := r.Args(Request{Prompt: "hi", Mode: "default"}); strings.Contains(strings.Join(got, " "), "permission-mode") {
		t.Fatalf("default mode should not pass a flag: %v", got)
	}
}

// TestClaudeRunnerWithFakeCLI runs a shell script standing in for the claude binary.
func TestClaudeRunnerWithFakeCLI(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "fake-claude")
	body := "#!/bin/sh\npwd > \"$(dirname \"$0\")/cwd\"\ncat <<'EOF'\n" + transcript + "EOF\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	work := t.TempDir()
	var events []Event
	err := ClaudeRunner{Command: script}.Run(context.Background(), Request{Prompt: "x", Workdir: work}, func(e Event) {
		events = append(events, e)
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 7 || events[6].CostUSD != 0.0125 {
		t.Fatalf("unexpected events: %+v", events)
	}
	cwd, _ := os.ReadFile(filepath.Join(dir, "cwd"))
	gotDir, _ := filepath.EvalSymlinks(strings.TrimSpace(string(cwd)))
	wantDir, _ := filepath.EvalSymlinks(work)
	if gotDir != wantDir {
		t.Errorf("ran in %q, want %q", gotDir, wantDir)
	}
}

func TestClaudeRunnerFailure(t *testing.T) {
	script := filepath.Join(t.TempDir(), "fake-claude")
	os.WriteFile(script, []byte("#!/bin/sh\necho 'not logged in' >&2\nexit 3\n"), 0o755)
	err := ClaudeRunner{Command: script}.Run(context.Background(), Request{Prompt: "x"}, func(Event) {})
	if err == nil || !strings.Contains(err.Error(), "not logged in") {
		t.Fatalf("expected stderr in error, got %v", err)
	}
}
