package extract

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseCodexRollout(t *testing.T) {
	p := filepath.Join(t.TempDir(), "rollout-2026-11-08T10-00-00-abc.jsonl")
	lines := []string{
		`{"timestamp":"t","type":"session_meta","payload":{"id":"0199-abc","cwd":"/work/repo"}}`,
		`{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"<environment_context>cwd</environment_context>"}]}}`,
		`{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"fix the flaky test"}]}}`,
		`{"type":"event_msg","payload":{"type":"agent_message","message":"dup"}}`,
		`{"type":"response_item","payload":{"type":"function_call","name":"shell"}}`,
		`{"type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"The race is in the cache warmup."}]}}`,
		`{"partial":`,
	}
	os.WriteFile(p, []byte(strings.Join(lines, "\n")), 0o644)
	tr, err := ParseJSONL(p, 0)
	if err != nil {
		t.Fatal(err)
	}
	if tr.CWD != "/work/repo" || tr.SessionID != "0199-abc" {
		t.Fatalf("meta: %+v", tr)
	}
	if len(tr.Messages) != 2 || tr.Messages[0].Text != "fix the flaky test" || tr.Messages[1].Role != "assistant" {
		t.Fatalf("messages: %+v", tr.Messages)
	}
	// The partial last line is not consumed.
	data, _ := os.ReadFile(p)
	if tr.NewOffset >= int64(len(data)) {
		t.Fatalf("offset %d consumed partial line (size %d)", tr.NewOffset, len(data))
	}
}

func TestParseClaudeStripsReminders(t *testing.T) {
	p := filepath.Join(t.TempDir(), "s.jsonl")
	os.WriteFile(p, []byte(`{"type":"user","cwd":"/r","sessionId":"x","message":{"role":"user","content":"hello <system-reminder>noise</system-reminder>"}}`+"\n"+
		`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","content":"big output"}]}}`+"\n"), 0o644)
	tr, err := ParseJSONL(p, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(tr.Messages) != 1 || tr.Messages[0].Text != "hello" {
		t.Fatalf("%+v", tr.Messages)
	}
}

func TestParseClaims(t *testing.T) {
	c, err := ParseClaims("sure!\n```json\n[{\"type\":\"fact\",\"title\":\"a\",\"description\":\"b\"}]\n```")
	if err != nil || len(c) != 1 || c[0].Title != "a" {
		t.Fatal(c, err)
	}
	if c, err := ParseClaims("[]"); err != nil || len(c) != 0 {
		t.Fatal(c, err)
	}
	if _, err := ParseClaims("no claims here"); err == nil {
		t.Fatal("want error")
	}
}

func TestCondenseKeepsRecent(t *testing.T) {
	msgs := []Message{{"user", strings.Repeat("a", 100)}, {"assistant", strings.Repeat("b", 100)}, {"user", "latest"}}
	out := Condense(msgs, 1000, 120)
	if !strings.Contains(out, "latest") || strings.Contains(out, "aaaa") {
		t.Fatalf("%q", out)
	}
}
