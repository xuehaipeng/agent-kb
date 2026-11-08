package mcp

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xuehaipeng/agent-kb/internal/kb"
)

func TestMCPSession(t *testing.T) {
	dir := t.TempDir()
	bundle := filepath.Join(dir, "knowledge")
	os.MkdirAll(bundle, 0o755)
	os.WriteFile(filepath.Join(bundle, kb.ConfigFile), []byte(kb.DefaultConfigYAML), 0o644)
	s, err := kb.Open(bundle)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("KB_ACTOR", "")
	t.Setenv("CLAUDECODE", "")
	msgs := []string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","clientInfo":{"name":"codex-mcp-client"}}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"kb_add","arguments":{"type":"fact","title":"Ports are fixed","description":"API listens on 8080."}}}`,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"kb_search","arguments":{"query":"port"}}}`,
		`{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"kb_verify","arguments":{"id":"ports-are-fixed","result":"confirmed","evidence":"x"}}}`,
		`{"jsonrpc":"2.0","id":6,"method":"nope"}`,
		`not json`,
	}
	pr, pw, _ := os.Pipe()
	srv := &Server{Store: s}
	done := make(chan error)
	go func() {
		err := srv.Serve(strings.NewReader(strings.Join(msgs, "\n")+"\n"), pw)
		pw.Close()
		done <- err
	}()
	var replies []map[string]any
	sc := bufio.NewScanner(pr)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		var m map[string]any
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			t.Fatalf("bad reply %q", sc.Text())
		}
		replies = append(replies, m)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if len(replies) != 7 { // notification gets no reply
		t.Fatalf("got %d replies: %v", len(replies), replies)
	}
	init := replies[0]["result"].(map[string]any)
	if init["protocolVersion"] != "2025-06-18" {
		t.Fatalf("init: %v", init)
	}
	tools := replies[1]["result"].(map[string]any)["tools"].([]any)
	if len(tools) != 6 {
		t.Fatalf("tools: %d", len(tools))
	}
	text := func(i int) string {
		r := replies[i]["result"].(map[string]any)
		return r["content"].([]any)[0].(map[string]any)["text"].(string)
	}
	if !strings.Contains(text(2), "added facts/ports-are-fixed") {
		t.Fatalf("add: %s", text(2))
	}
	if !strings.Contains(text(3), "[unverified] Ports are fixed") {
		t.Fatalf("search: %s", text(3))
	}
	// clientInfo said codex: the author may verify its own entry (verified, not cross-checked).
	if r := replies[4]["result"].(map[string]any); r["isError"] == true || !strings.Contains(text(4), "is now verified") || !strings.Contains(text(2), "added facts/ports-are-fixed [unverified]") {
		t.Fatalf("verify: %v / %s", r, text(2))
	}
	if replies[5]["error"].(map[string]any)["code"].(float64) != -32601 {
		t.Fatalf("unknown method: %v", replies[5])
	}
	if replies[6]["error"].(map[string]any)["code"].(float64) != -32700 {
		t.Fatalf("parse error: %v", replies[6])
	}
}
