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
	os.WriteFile(p, []byte(`{"model":"opus","hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"echo hi"}]}],"SessionEnd":[{"hooks":[{"type":"command","command":"my-cleanup"},{"type":"command","command":"kb hook claude","timeout":30}]}],"Stop":[{"hooks":[{"type":"command","command":"notify"}]}]},"permissions":{"allow":["Bash(ls:*)"]}}`), 0o644)
	allEvents := append(append([]string{}, claudeDeliveryEvents...), claudeCaptureEvents...)
	all := generic(allEvents...)
	if _, err := mergeClaudeSettings(p, all, generic(claudeRefreshEvent)); err != nil {
		t.Fatal(err)
	}
	if did, _ := mergeClaudeSettings(p, all, generic(claudeRefreshEvent)); did {
		t.Fatal("merge not idempotent")
	}
	read := func() map[string]any {
		var m map[string]any
		data, _ := os.ReadFile(p)
		json.Unmarshal(data, &m)
		return m
	}
	m := read()
	if m["model"] != "opus" {
		t.Fatal("lost user key")
	}
	hooks := m["hooks"].(map[string]any)
	if ss := hooks["SessionStart"].([]any); len(ss) != 2 {
		t.Fatalf("SessionStart groups: %v", ss)
	}
	for _, ev := range allEvents {
		if _, ok := hooks[ev]; !ok {
			t.Fatalf("%s hook missing: %v", ev, hooks)
		}
	}
	if _, ok := hooks[claudeRefreshEvent]; ok {
		t.Fatalf("the rule refresh hook is opt-in: %v", hooks)
	}
	allow := m["permissions"].(map[string]any)["allow"].([]any)
	if allow[0] != "Bash(ls:*)" || len(allow) != 1+len(claudeAllow) {
		t.Fatalf("allow: %v", allow)
	}

	// Manual capture: kb's own capture hooks go, and only those. Another
	// tool's hook in the same event, and events kb does not own, stay.
	if _, err := mergeClaudeSettings(p, generic(claudeDeliveryEvents...), generic(append([]string{claudeRefreshEvent}, claudeCaptureEvents...)...)); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(p)
	hooks = read()["hooks"].(map[string]any)
	if _, ok := hooks["PreCompact"]; ok {
		t.Fatalf("PreCompact held only kb's hook and should be gone:\n%s", data)
	}
	end := hooks["SessionEnd"].([]any)[0].(map[string]any)["hooks"].([]any)
	if len(end) != 1 || end[0].(map[string]any)["command"] != "my-cleanup" {
		t.Fatalf("another tool's SessionEnd hook must stay, kb's must go:\n%s", data)
	}
	if hooks["Stop"] == nil || len(hooks["SessionStart"].([]any)) != 2 || hooks["SubagentStart"] == nil {
		t.Fatalf("unrelated or delivery hooks changed:\n%s", data)
	}
	// Back to full: the capture hooks return.
	if _, err := mergeClaudeSettings(p, all, nil); err != nil {
		t.Fatal(err)
	}
	if hooks = read()["hooks"].(map[string]any); hooks["PreCompact"] == nil || len(hooks["SessionEnd"].([]any)) != 2 {
		t.Fatalf("full capture: %v", hooks)
	}

	// File context is a hook of its own, with a matcher, beside kb's other
	// hook on the same event; removing it leaves that one alone.
	refresh := generic(claudeRefreshEvent)
	if _, err := mergeClaudeSettings(p, append(append([]hookSpec{}, refresh...), fileContextSpecs("pre")...), nil); err != nil {
		t.Fatal(err)
	}
	hooks = read()["hooks"].(map[string]any)
	pre := hooks["PreToolUse"].([]any)
	post := hooks["PostToolUse"].([]any)
	if len(pre) != 2 || len(post) != 1 || post[0].(map[string]any)["matcher"] != "Read|Edit|Write" {
		t.Fatalf("file-context hooks: pre=%v post=%v", pre, post)
	}
	for _, g := range append(append([]any{}, pre...), post...) {
		for _, h := range g.(map[string]any)["hooks"].([]any) {
			if _, async := h.(map[string]any)["async"]; async {
				t.Fatalf("kb's hooks are synchronous: async context can arrive a turn late or be lost: %v", h)
			}
		}
	}
	if did, _ := mergeClaudeSettings(p, append(append([]hookSpec{}, refresh...), fileContextSpecs("pre")...), nil); did {
		t.Fatal("file-context merge not idempotent")
	}
	if _, err := mergeClaudeSettings(p, refresh, fileContextSpecs("pre")); err != nil {
		t.Fatal(err)
	}
	hooks = read()["hooks"].(map[string]any)
	if hooks["PostToolUse"] != nil || len(hooks["PreToolUse"].([]any)) != 1 {
		t.Fatalf("removing file context must leave the refresh hook: %v", hooks)
	}

	if err := os.WriteFile(p, []byte("{broken"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := mergeClaudeSettings(p, all, nil); err == nil {
		t.Fatal("broken JSON must not be overwritten")
	}
}

func TestPointerIsSmall(t *testing.T) {
	p := Pointer()
	words := len(strings.Fields(p))
	// Roughly 1.3 tokens per word: keep the always-on cost near 300 tokens.
	if words > 240 {
		t.Fatalf("pointer is %d words; keep it thin", words)
	}
	if strings.Contains(p, "{{") {
		t.Fatal("unrendered placeholder")
	}
}
