package agent

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

const transcript = `{"type":"system","subtype":"init","session_id":"abc-123","tools":["Bash"]}
{"type":"assistant","message":{"content":[{"type":"text","text":"Let me check."},{"type":"tool_use","id":"t1","name":"Bash","input":{"command": "echo hi"}}]}}
{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t1","content":"hi"}]}}
{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t2","is_error":true,"content":[{"type":"text","text":"boom"}]}]}}
{"type":"user","message":{"content":[{"type":"text","text":"Base directory for this skill: /x"}]}}
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
		{Role: "user", Kind: "context", Content: "Base directory for this skill: /x"},
		{Role: "assistant", Kind: "text", Content: "Done."},
		{Role: "system", Kind: "result", Content: "Done.", ClaudeSessionID: "abc-123", CostUSD: 0.0125},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got  %+v\nwant %+v", got, want)
	}
}

func TestParsePermissionDenials(t *testing.T) {
	got := ParseLine([]byte(`{"type":"result","subtype":"success","result":"Blocked.","session_id":"x",` +
		`"permission_denials":[{"tool_name":"Bash","tool_use_id":"t1","tool_input":{"command":"rm x"}},{"tool_name":"Write","tool_use_id":"t2"}]}`))
	if len(got) != 1 || got[0].Kind != "result" || !reflect.DeepEqual(got[0].Denied, []string{"Bash", "Write"}) {
		t.Fatalf("got %+v", got)
	}
}

func TestParseErrorResult(t *testing.T) {
	got := ParseLine([]byte(`{"type":"result","subtype":"error_max_turns","is_error":true,"session_id":"x"}`))
	if len(got) != 1 || got[0].Kind != "error" || got[0].Content != "error_max_turns" {
		t.Fatalf("got %+v", got)
	}
}

func TestParseRateLimit(t *testing.T) {
	// Captured from claude 2.1.304 on a Team plan.
	line := `{"type":"rate_limit_event","rate_limit_info":{"status":"allowed","resetsAt":1792497600,"rateLimitType":"five_hour","overageStatus":"rejected","isUsingOverage":false,"unifiedWindows":{"five_hour":{"utilization":0.12,"resetsAt":1792497600},"seven_day":{"utilization":0.03,"resetsAt":1793001600}}},"uuid":"a4c4","session_id":"1f36"}`
	got := ParseLine([]byte(line))
	want := []Event{{Role: "system", Kind: "rate_limit", Limits: &RateLimits{
		FiveHour: &LimitWindow{UsedPercent: 12, ResetsAt: time.Unix(1792497600, 0)},
		SevenDay: &LimitWindow{UsedPercent: 3, ResetsAt: time.Unix(1793001600, 0)},
	}}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v", got)
	}

	// Without unifiedWindows only the named window is known.
	got = ParseLine([]byte(`{"type":"rate_limit_event","rate_limit_info":{"rateLimitType":"seven_day","utilization":0.5,"resetsAt":1793001600}}`))
	if len(got) != 1 || got[0].Limits.FiveHour != nil || got[0].Limits.SevenDay.UsedPercent != 50 {
		t.Fatalf("got %+v", got)
	}

	// No utilization: nothing to report.
	if got := ParseLine([]byte(`{"type":"rate_limit_event","rate_limit_info":{"status":"allowed","resetsAt":1792497600,"rateLimitType":"five_hour"}}`)); got != nil {
		t.Fatalf("got %+v", got)
	}
}

func TestArgs(t *testing.T) {
	r := ClaudeRunner{ExtraArgs: []string{"--model", "opus"}}
	got := r.Args(Request{Prompt: "hi", ResumeID: "s1", Mode: "plan"})
	want := []string{"-p", "--output-format", "stream-json", "--verbose", "--resume", "s1", "--permission-mode", "plan", "--model", "opus"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v", got)
	}
	got = r.Args(Request{Prompt: "hi", Model: "sonnet"})
	if want := []string{"-p", "--output-format", "stream-json", "--verbose", "--model", "opus", "--model", "sonnet"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("session model should come last: %v", got)
	}
	if got := r.Args(Request{Prompt: "hi", Mode: "default"}); strings.Contains(strings.Join(got, " "), "permission-mode") {
		t.Fatalf("default mode should not pass a flag: %v", got)
	}
	got = ClaudeRunner{}.Args(Request{ResumeID: "s1", Fork: true, Mode: "plan"})
	if want := []string{"-p", "--output-format", "stream-json", "--verbose", "--resume", "s1", "--fork-session", "--permission-mode", "plan"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("fork: %v", got)
	}
	if got := r.Args(Request{Fork: true}); strings.Contains(strings.Join(got, " "), "fork") {
		t.Fatalf("there is nothing to fork without a session to resume: %v", got)
	}
	got = ClaudeRunner{}.Args(Request{ResumeID: "s1", SystemPrompt: "You run in the app."})
	if want := []string{"-p", "--output-format", "stream-json", "--verbose", "--resume", "s1", "--append-system-prompt", "You run in the app."}; !reflect.DeepEqual(got, want) {
		t.Fatalf("system prompt: %v", got)
	}
}

// TestClaudeRunnerWithFakeCLI runs a shell script standing in for the claude binary.
func TestClaudeRunnerWithFakeCLI(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "fake-claude")
	body := "#!/bin/sh\npwd > \"$(dirname \"$0\")/cwd\"\ncat > \"$(dirname \"$0\")/stdin\"\ncat <<'EOF'\n" + transcript + "EOF\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	work := t.TempDir()
	var events []Event
	err := ClaudeRunner{Command: script}.Run(context.Background(), Request{Prompt: "- a prompt that looks like an option", Workdir: work}, func(e Event) {
		events = append(events, e)
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 8 || events[7].CostUSD != 0.0125 {
		t.Fatalf("unexpected events: %+v", events)
	}
	if in, _ := os.ReadFile(filepath.Join(dir, "stdin")); string(in) != "- a prompt that looks like an option" {
		t.Errorf("prompt on stdin = %q", in)
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

func TestParseContextUsage(t *testing.T) {
	got := parseAll(`{"type":"system","subtype":"init","session_id":"s","model":"claude-opus-5-5"}
{"type":"assistant","message":{"content":[{"type":"text","text":"Hi."}],"usage":{"input_tokens":2,"cache_creation_input_tokens":300,"cache_read_input_tokens":4000,"output_tokens":50}},"parent_tool_use_id":null}
{"type":"assistant","message":{"content":[{"type":"text","text":"sub"}],"usage":{"input_tokens":9000}},"parent_tool_use_id":"t1"}
{"type":"user","message":{"content":"x","usage":{"input_tokens":9000}}}
{"type":"result","subtype":"success","result":"Hi.","session_id":"s","modelUsage":{"claude-opus-5-5":{"contextWindow":200000},"claude-haiku-5-5":{"contextWindow":1000000},"other":{}}}`)
	want := []Event{
		{Role: "system", Kind: "init", ClaudeSessionID: "s", Model: "claude-opus-5-5"},
		{Role: "assistant", Kind: "text", Content: "Hi."},
		{Role: "system", Kind: "usage", ContextTokens: 4352},
		{Role: "assistant", Kind: "text", Content: "sub"},
		{Role: "user", Kind: "context", Content: "x"},
		{Role: "system", Kind: "result", Content: "Hi.", ClaudeSessionID: "s",
			ContextWindows: map[string]int{"claude-opus-5-5": 200000, "claude-haiku-5-5": 1000000}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got  %+v\nwant %+v", got, want)
	}
}
