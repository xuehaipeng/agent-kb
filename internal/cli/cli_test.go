package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func setupRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("GIT_AUTHOR_NAME", "t")
	t.Setenv("GIT_AUTHOR_EMAIL", "t@t")
	t.Setenv("GIT_COMMITTER_NAME", "t")
	t.Setenv("GIT_COMMITTER_EMAIL", "t@t")
	t.Setenv("KB_ROOT", "")
	git(t, dir, "init", "-q", "-b", "main")
	if err := os.MkdirAll(filepath.Join(dir, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, "src", "app.go"), []byte("package src\n"), 0o644)
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-qm", "init")
	wd, _ := os.Getwd()
	t.Cleanup(func() { os.Chdir(wd) })
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	return dir
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

func kbRun(t *testing.T, actor string, stdin string, args ...string) (string, string, int) {
	t.Helper()
	t.Setenv("KB_ACTOR", actor)
	var out, errb bytes.Buffer
	code := Run(args, strings.NewReader(stdin), &out, &errb)
	return out.String(), errb.String(), code
}

func must(t *testing.T, actor string, args ...string) string {
	t.Helper()
	out, errs, code := kbRun(t, actor, "", args...)
	if code != 0 {
		t.Fatalf("kb %v: exit %d\nstdout: %s\nstderr: %s", args, code, out, errs)
	}
	return out
}

func TestCLIEndToEnd(t *testing.T) {
	dir := setupRepo(t)
	must(t, "", "init", "--install", "claude,codex,git-hooks")
	for _, f := range []string{"knowledge/kb.yaml", "knowledge/POINTER.md", "AGENTS.md", "CLAUDE.md", ".claude/settings.json", ".claude/skills/kb/SKILL.md"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Errorf("missing %s", f)
		}
	}
	// Re-running install is idempotent.
	out := must(t, "", "install", "claude", "codex")
	if strings.Count(out, "wrote") != 0 {
		t.Errorf("second install rewrote files:\n%s", out)
	}
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-qm", "kb init")

	out = must(t, "claude-code/opus-4.5", "add", "learning", "App", "init", "must", "run", "before", "serve", "-d", "Serve panics on nil config otherwise.", "-r", "src/**", "-t", "startup")
	if !strings.Contains(out, "added learnings/app-init-must-run-before-serve") {
		t.Fatalf("add: %s", out)
	}
	// Body from stdin.
	_, errs, code := kbRun(t, "codex/gpt-5.2", "Body from stdin.", "add", "fact", "Config lives in src", "-d", "All config is loaded in src/app.go.", "-b", "-", "-r", "src/app.go")
	if code != 0 {
		t.Fatal(errs)
	}
	out = must(t, "codex", "search", "--json", "init serve")
	var hits []hitJSON
	if err := json.Unmarshal([]byte(out), &hits); err != nil || len(hits) == 0 || hits[0].Trust != "unverified" {
		t.Fatalf("json search: %v %s", err, out)
	}
	out = must(t, "codex", "search", "-f", "src/app.go")
	if strings.Count(out, "(anchored: src/app.go)") != 2 {
		t.Fatalf("path search:\n%s", out)
	}
	if _, errs, code := kbRun(t, "claude-code", "", "verify", "app-init", "--confirm", "-e", "x"); code == 0 || !strings.Contains(errs, "self-verification") {
		t.Fatalf("self verify allowed: %s", errs)
	}
	out = must(t, "codex/gpt-5.2", "verify", "app-init", "--confirm", "-e", "src/app.go checks config")
	if !strings.Contains(out, "corroborated") {
		t.Fatal(out)
	}
	out = must(t, "claude-code", "show", "app-init")
	if !strings.Contains(out, "trust: corroborated") || !strings.Contains(out, "confirmed: codex/gpt-5.2") {
		t.Fatal(out)
	}
	// No manual 'kb index': writes keep indexes current.
	out, _, code = kbRun(t, "", "", "lint")
	if code != 0 {
		t.Fatalf("lint failed:\n%s", out)
	}
	// Pre-commit hook lets a clean commit through.
	git(t, dir, "add", "-A")
	cmd := exec.Command("git", "commit", "-qm", "knowledge")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "PATH="+os.Getenv("PATH"))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("commit with hook: %v %s", err, out)
	}

	// Review round trip.
	os.WriteFile(filepath.Join(dir, "src", "app.go"), []byte("package src\n\nfunc Serve() {}\n"), 0o644)
	git(t, dir, "commit", "-qam", "serve")
	out = must(t, "claude-code", "review", "submit", "Add", "Serve", "-m", "adds Serve", "-a", "config checked first", "--base", "HEAD~1")
	if !strings.Contains(out, "submitted reviews/") {
		t.Fatal(out)
	}
	out = must(t, "codex", "review", "list", "--for-me")
	id := strings.Fields(out)[0]
	out = must(t, "codex", "review", "show", id)
	if !strings.Contains(out, "src/app.go") || strings.Contains(out, "knowledge/log.md") {
		t.Fatalf("review show should list code, not bundle files:\n%s", out)
	}
	out = must(t, "codex", "review", "verdict", id, "--approve", "-m", "fine")
	if !strings.Contains(out, "approved") {
		t.Fatal(out)
	}
	out = must(t, "claude-code", "status", "--json")
	var st map[string]any
	if err := json.Unmarshal([]byte(out), &st); err != nil {
		t.Fatal(err)
	}
	if st["stale"].(float64) < 1 {
		t.Fatalf("app.go changed, expected stale claims: %s", out)
	}
}

func TestHookSessionStartAndExtractGuard(t *testing.T) {
	dir := setupRepo(t)
	must(t, "", "init")
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-qm", "kb")
	os.WriteFile(filepath.Join(dir, "src", "app.go"), []byte("package src\n// x\n"), 0o644)
	git(t, dir, "commit", "-qam", "x")
	must(t, "codex", "review", "submit", "Codex change", "--base", "HEAD~1")

	hook := `{"session_id":"abc","hook_event_name":"SessionStart","cwd":"` + dir + `","source":"startup"}`
	out, _, code := kbRun(t, "claude-code", hook, "hook", "claude")
	if code != 0 || !strings.Contains(out, "review(s) waiting for you") {
		t.Fatalf("SessionStart brief: %d %q", code, out)
	}
	// With the guard set, SessionEnd must not spawn anything.
	t.Setenv("KB_EXTRACTING", "1")
	out, _, code = kbRun(t, "claude-code", `{"hook_event_name":"SessionEnd","transcript_path":"/nonexistent.jsonl","cwd":"`+dir+`"}`, "hook", "claude")
	if code != 0 || out != "" {
		t.Fatalf("guarded SessionEnd: %d %q", code, out)
	}
	if _, err := os.Stat(filepath.Join(dir, "knowledge", ".kb", "extract.log")); err == nil {
		t.Fatal("extract should not have been spawned")
	}
	// Hooks outside a kb repo are silent no-ops.
	os.Chdir(t.TempDir())
	out, _, code = kbRun(t, "claude-code", `{"hook_event_name":"SessionStart"}`, "hook", "claude")
	if code != 0 || out != "" {
		t.Fatalf("non-kb repo: %d %q", code, out)
	}
}

func TestExtractWithFakeModel(t *testing.T) {
	dir := setupRepo(t)
	must(t, "", "init")
	transcript := filepath.Join(dir, "session.jsonl")
	long := strings.Repeat("We traced the hang to the vitest worker pool on Node 24. ", 40)
	lines := []string{
		`{"type":"user","cwd":"` + dir + `","sessionId":"s-123","message":{"role":"user","content":"why does pnpm test hang?"}}`,
		`{"type":"assistant","sessionId":"s-123","message":{"role":"assistant","content":[{"type":"text","text":"` + long + `"},{"type":"tool_use","name":"Bash","input":{}}]}}`,
	}
	os.WriteFile(transcript, []byte(strings.Join(lines, "\n")+"\n"), 0o644)
	claims := filepath.Join(dir, "claims.json")
	os.WriteFile(claims, []byte("Here you go:\n```json\n"+`[{"type":"learning","title":"Vitest hangs on Node 24 without --no-isolate","description":"Worker pool deadlocks on Node 24.1.","body":"Use --no-isolate.","resources":["src/**"],"tags":["vitest"]}]`+"\n```\n"), 0o644)
	out := must(t, "claude-code", "extract", "--transcript", transcript, "--cmd", "cat "+claims)
	if !strings.Contains(out, "added 1 claim") {
		t.Fatalf("extract: %s", out)
	}
	out = must(t, "codex", "show", "vitest-hangs")
	if !strings.Contains(out, "source [^sess-s-123]") || !strings.Contains(out, "generated: claude-code") {
		t.Fatalf("provenance: %s", out)
	}
	// Second run: offset saved, nothing new to read.
	out = must(t, "claude-code", "extract", "--transcript", transcript, "--cmd", "cat "+claims)
	if !strings.Contains(out, "skipped") {
		t.Fatalf("expected skip on unchanged transcript: %s", out)
	}
	must(t, "", "index", "--quiet")
	if out, _, code := kbRun(t, "", "", "lint"); code != 0 {
		t.Fatalf("lint after extract:\n%s", out)
	}
}

func TestSyncWithRemote(t *testing.T) {
	dir := setupRepo(t)
	remote := filepath.Join(t.TempDir(), "remote.git")
	git(t, dir, "init", "-q", "--bare", "-b", "main", remote)
	git(t, dir, "remote", "add", "origin", remote)
	must(t, "", "init")
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-qm", "kb")
	git(t, dir, "push", "-q", "-u", "origin", "main")

	// A second clone (the Hermes host) adds a claim and syncs.
	hermes := filepath.Join(t.TempDir(), "hermes")
	git(t, dir, "clone", "-q", remote, hermes)
	os.Chdir(hermes)
	must(t, "hermes/qwen3", "add", "learning", "Nightly job OOMs at 2am", "-d", "Batch export exceeds 4GB heap.")
	out := must(t, "hermes", "sync")
	if !strings.Contains(out, "pushed main") {
		t.Fatalf("hermes sync: %s", out)
	}
	// Local side pulls it.
	os.Chdir(dir)
	out = must(t, "claude-code", "sync")
	if !strings.Contains(out, "up to date") && !strings.Contains(out, "pushed") {
		t.Fatalf("local sync: %s", out)
	}
	out = must(t, "claude-code", "search", "nightly OOM")
	if !strings.Contains(out, "nightly-job-ooms") {
		t.Fatalf("pulled claim not found: %s", out)
	}
	if strings.TrimSpace(git(t, dir, "status", "--porcelain")) != "" {
		t.Fatalf("work tree dirty after sync: %s", git(t, dir, "status", "--porcelain"))
	}
}

func TestFlagsInterspersed(t *testing.T) {
	f := newFlags("x")
	var d string
	var r []string
	var b bool
	f.str(&d, "", "", "d")
	f.list(&r, true, "", "r")
	f.boolean(&b, "", "force")
	pos, err := f.parse([]string{"learning", "-d", "desc", "Title", "words", "-r", "a/**,b", "--force", "-r=c"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(pos, " ") != "learning Title words" || d != "desc" || strings.Join(r, ",") != "a/**,b,c" || !b {
		t.Fatalf("pos=%v d=%q r=%v b=%v", pos, d, r, b)
	}
	if _, err := f.parse([]string{"--nope"}); err == nil {
		t.Fatal("unknown flag accepted")
	}
}
