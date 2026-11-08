package install

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestUpsertBlockIdempotent(t *testing.T) {
	p := filepath.Join(t.TempDir(), "AGENTS.md")
	os.WriteFile(p, []byte("# Project\n\nKeep this.\n"), 0o644)
	if did, err := upsertBlock(p, "v1", mdBegin, mdEnd); err != nil || !did {
		t.Fatal(did, err)
	}
	if did, _ := upsertBlock(p, "v1", mdBegin, mdEnd); did {
		t.Fatal("second identical upsert changed the file")
	}
	upsertBlock(p, "v2", mdBegin, mdEnd)
	data, _ := os.ReadFile(p)
	s := string(data)
	if !strings.HasPrefix(s, "# Project\n\nKeep this.\n\n") || strings.Contains(s, "v1") || strings.Count(s, mdBegin) != 1 || !strings.Contains(s, "v2") {
		t.Fatalf("got:\n%s", s)
	}
}

func TestMergeClaudeSettingsPreservesUserKeys(t *testing.T) {
	p := filepath.Join(t.TempDir(), "settings.json")
	os.WriteFile(p, []byte(`{"model":"opus","hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"echo hi"}]}]},"permissions":{"allow":["Bash(ls:*)"]}}`), 0o644)
	if _, err := mergeClaudeSettings(p); err != nil {
		t.Fatal(err)
	}
	if did, _ := mergeClaudeSettings(p); did {
		t.Fatal("merge not idempotent")
	}
	var m map[string]any
	data, _ := os.ReadFile(p)
	json.Unmarshal(data, &m)
	if m["model"] != "opus" {
		t.Fatal("lost user key")
	}
	ss := m["hooks"].(map[string]any)["SessionStart"].([]any)
	if len(ss) != 2 {
		t.Fatalf("SessionStart groups: %v", ss)
	}
	allow := m["permissions"].(map[string]any)["allow"].([]any)
	if allow[0] != "Bash(ls:*)" || len(allow) != 1+len(claudeAllow) {
		t.Fatalf("allow: %v", allow)
	}
	if err := os.WriteFile(p, []byte("{broken"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := mergeClaudeSettings(p); err == nil {
		t.Fatal("broken JSON must not be overwritten")
	}
}

func TestPointerIsSmall(t *testing.T) {
	p := Pointer("knowledge")
	words := len(strings.Fields(p))
	// Roughly 1.3 tokens per word: keep the always-on cost near 300 tokens.
	if words > 240 {
		t.Fatalf("pointer is %d words; keep it thin", words)
	}
	if strings.Contains(p, "{{") {
		t.Fatal("unrendered placeholder")
	}
}
