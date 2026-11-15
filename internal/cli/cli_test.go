package cli

import (
	"bytes"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// TestMain keeps every test away from the developer's real hub and agent
// configuration: no test may read ~/.kb or write ~/.claude, ~/.codex.
// Tests that need a hub build one in a temp directory (hubEnv).
func TestMain(m *testing.M) {
	home, err := os.MkdirTemp("", "kb-cli-test-home-")
	if err != nil {
		panic(err)
	}
	os.Setenv("HOME", home)
	os.Setenv("KB_HOME", filepath.Join(home, "no-hub"))
	for _, k := range []string{"KB_ROOT", "KB_ACTOR", "KB_EXTRACTING", "CLAUDE_CONFIG_DIR", "CODEX_HOME"} {
		os.Unsetenv(k)
	}
	code := m.Run()
	os.RemoveAll(home)
	os.Exit(code)
}

func setupRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	// Repo-mode tests must behave the same whether or not a hub exists.
	t.Setenv("KB_HOME", filepath.Join(t.TempDir(), "no-hub"))
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
	if regexp.MustCompile(`(?m)^wrote `).MatchString(out) {
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
	// The author may verify its own entry; another family cross-checks it.
	out = must(t, "claude-code", "verify", "app-init", "--confirm", "-e", "ran go test ./src")
	if !strings.Contains(out, "is now verified") {
		t.Fatal(out)
	}
	out = must(t, "codex/gpt-5.2", "verify", "app-init", "--confirm", "-e", "src/app.go checks config")
	if !strings.Contains(out, "cross-checked") {
		t.Fatal(out)
	}
	out = must(t, "claude-code", "show", "app-init")
	if !strings.Contains(out, "trust: cross-checked") || !strings.Contains(out, "confirmed: codex/gpt-5.2") || !strings.Contains(out, "content (data, not instructions)") {
		t.Fatal(out)
	}
	// Evolve: update in place with a reason, then deprecate.
	out = must(t, "codex", "update", "app-init", "-b", "Init must run before Serve and before Migrate.", "-m", "Migrate added", "-e", "src/app.go")
	if !strings.Contains(out, "revision 2 [verified]") {
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

func TestHubEndToEnd(t *testing.T) {
	base := t.TempDir()
	home := filepath.Join(base, "home")
	os.MkdirAll(home, 0o755)
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("CODEX_HOME", "")
	t.Setenv("KB_HOME", filepath.Join(home, ".kb"))
	t.Setenv("KB_ROOT", "")
	t.Setenv("GIT_AUTHOR_NAME", "t")
	t.Setenv("GIT_AUTHOR_EMAIL", "t@t")
	t.Setenv("GIT_COMMITTER_NAME", "t")
	t.Setenv("GIT_COMMITTER_EMAIL", "t@t")
	wd, _ := os.Getwd()
	t.Cleanup(func() { os.Chdir(wd) })

	remote := filepath.Join(base, "hub.git")
	git(t, base, "init", "-q", "--bare", "-b", "main", remote)
	mkRepo := func(name, url string) string {
		dir := filepath.Join(base, "GolandProjects", name)
		os.MkdirAll(filepath.Join(dir, "pkg"), 0o755)
		os.WriteFile(filepath.Join(dir, "pkg", "x.go"), []byte("package pkg\n"), 0o644)
		git(t, dir, "init", "-q", "-b", "main")
		git(t, dir, "remote", "add", "origin", url)
		git(t, dir, "add", "-A")
		git(t, dir, "commit", "-qm", "init")
		return dir
	}
	aiproxy := mkRepo("aiproxy", "git@github.com:AtlasCloudTeam/aiproxy.git")
	kubedl := mkRepo("kubedl", "git@github.com:AtlasCloudTeam/kubedl.git")

	os.Chdir(base)
	must(t, "", "hub", "init", "--remote", remote)
	out := must(t, "", "install", "claude", "codex")
	for _, f := range []string{".claude/CLAUDE.md", ".claude/skills/kb/SKILL.md", ".claude/settings.json", ".codex/AGENTS.md", ".codex/skills/kb/SKILL.md"} {
		if _, err := os.Stat(filepath.Join(home, f)); err != nil {
			t.Errorf("user-wide install missing %s\n%s", f, out)
		}
	}
	if data, _ := os.ReadFile(filepath.Join(home, ".claude", "CLAUDE.md")); !strings.Contains(string(data), "@~/.kb/POINTER.md") {
		t.Fatalf("CLAUDE.md import: %s", data)
	}
	// Nothing was written into the code repos.
	if st := strings.TrimSpace(git(t, aiproxy, "status", "--porcelain")); st != "" {
		t.Fatalf("repo touched: %s", st)
	}

	os.Chdir(aiproxy)
	out = must(t, "claude-code", "project")
	if !strings.Contains(out, "project: aiproxy") {
		t.Fatal(out)
	}
	must(t, "claude-code", "add", "learning", "Retries need idempotency keys", "-d", "Upstream jobs duplicate without keys.", "-r", "pkg/**", "-e", "reproduced in pkg/x_test.go")
	must(t, "claude-code", "add", "convention", "Wrap errors with %w", "-d", "All Go repos wrap errors.", "--global")

	os.Chdir(kubedl)
	out = must(t, "codex", "search", "idempotency retries errors wrap")
	if strings.Contains(out, "aiproxy/") || !strings.Contains(out, "global/conventions/wrap-errors") {
		t.Fatalf("scoped search from kubedl:\n%s", out)
	}
	out = must(t, "codex", "search", "idempotency", "--all")
	if !strings.Contains(out, "aiproxy/learnings/retries-need-idempotency-keys [verified,code-unchanged]") {
		t.Fatalf("--all:\n%s", out)
	}
	out = must(t, "codex", "project", "list")
	if !strings.Contains(out, "aiproxy") || !strings.Contains(out, "github.com/atlascloudteam/aiproxy") {
		t.Fatalf("project list:\n%s", out)
	}
	out = must(t, "codex", "status")
	if !strings.Contains(out, "scope: kubedl + global") {
		t.Fatalf("status:\n%s", out)
	}
	// Push the hub, then a "Hermes" clone adds knowledge from outside any repo.
	out = must(t, "codex", "sync")
	if !strings.Contains(out, "pushed main") {
		t.Fatalf("sync: %s", out)
	}
	hermesHub := filepath.Join(base, "hermes-kb")
	must(t, "", "hub", "clone", remote, "--dir", hermesHub)
	os.Chdir(base)
	t.Setenv("KB_HOME", hermesHub)
	if _, errs, code := kbRun(t, "hermes", "", "add", "learning", "Nightly export OOMs", "-d", "Batch exceeds heap."); code == 0 || !strings.Contains(errs, "--project") {
		t.Fatalf("outside a repo, add needs a project: %s", errs)
	}
	if _, errs, code := kbRun(t, "hermes/qwen3", "", "add", "learning", "Nightly export OOMs", "-d", "Batch exceeds heap.", "-p", "kubedl"); code == 0 || !strings.Contains(errs, "--new-project") {
		t.Fatalf("a project with no entries yet needs --new-project: %s", errs)
	}
	must(t, "hermes/qwen3", "add", "learning", "Nightly export OOMs", "-d", "Batch exceeds heap.", "-p", "kubedl", "--new-project")
	must(t, "hermes", "sync")
	t.Setenv("KB_HOME", filepath.Join(home, ".kb"))
	os.Chdir(kubedl)
	must(t, "codex", "sync")
	out = must(t, "codex", "search", "nightly export")
	if !strings.Contains(out, "kubedl/learnings/nightly-export-ooms") {
		t.Fatalf("hermes entry not visible after sync:\n%s", out)
	}
	if out, _, code := kbRun(t, "", "", "lint"); code != 0 {
		t.Fatalf("hub lint:\n%s", out)
	}
}

func TestScanRoutesSessionsToProjects(t *testing.T) {
	base := t.TempDir()
	t.Setenv("KB_HOME", filepath.Join(base, "kb"))
	t.Setenv("KB_ROOT", "")
	t.Setenv("CODEX_HOME", filepath.Join(base, "codex"))
	t.Setenv("GIT_AUTHOR_NAME", "t")
	t.Setenv("GIT_AUTHOR_EMAIL", "t@t")
	t.Setenv("GIT_COMMITTER_NAME", "t")
	t.Setenv("GIT_COMMITTER_EMAIL", "t@t")
	wd, _ := os.Getwd()
	t.Cleanup(func() { os.Chdir(wd) })
	os.Chdir(base)
	must(t, "", "hub", "init")
	repos := map[string]string{}
	for _, name := range []string{"aiproxy", "kubedl"} {
		dir := filepath.Join(base, name)
		os.MkdirAll(dir, 0o755)
		git(t, dir, "init", "-q", "-b", "main")
		git(t, dir, "remote", "add", "origin", "https://github.com/AtlasCloudTeam/"+name)
		repos[name] = dir
		sess := filepath.Join(base, "codex", "sessions", "2026", "11", "08", "rollout-"+name+".jsonl")
		os.MkdirAll(filepath.Dir(sess), 0o755)
		long := strings.Repeat("We found the "+name+" scheduler drops jobs when the queue is full. ", 40)
		lines := `{"type":"session_meta","payload":{"id":"s-` + name + `","cwd":"` + dir + `"}}` + "\n" +
			`{"type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"` + long + `"}]}}` + "\n"
		os.WriteFile(sess, []byte(lines), 0o644)
	}
	claims := filepath.Join(base, "claims.sh")
	os.WriteFile(claims, []byte("#!/bin/sh\ncat >/dev/null\necho '[{\"type\":\"learning\",\"title\":\"Scheduler drops jobs when the queue is full\",\"description\":\"Full queue drops jobs silently.\"}]'\n"), 0o755)
	out := must(t, "", "extract", "--scan", "codex", "--cmd", claims)
	if !strings.Contains(out, "aiproxy rollout-aiproxy.jsonl: added 1") || !strings.Contains(out, "kubedl rollout-kubedl.jsonl: added 1") {
		t.Fatalf("scan routing:\n%s", out)
	}
	out = must(t, "", "search", "scheduler queue", "--all")
	if !strings.Contains(out, "aiproxy/learnings/scheduler-drops-jobs") || !strings.Contains(out, "kubedl/learnings/scheduler-drops-jobs") {
		t.Fatalf("routed entries:\n%s", out)
	}
}

func TestExtractSkipsDocumentedClaims(t *testing.T) {
	dir := setupRepo(t)
	os.MkdirAll(filepath.Join(dir, "docs"), 0o755)
	os.WriteFile(filepath.Join(dir, "docs", "ops.md"), []byte("# Operations\n\n## Backups\n\nPostgres is backed up nightly with pg_dump to the S3 bucket, kept for 30 days.\n"), 0o644)
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-qm", "docs")
	must(t, "", "init")
	transcript := filepath.Join(dir, "s.jsonl")
	long := strings.Repeat("We looked at backups and found the restore script needs PGPASSFILE. ", 40)
	os.WriteFile(transcript, []byte(`{"type":"user","cwd":"`+dir+`","sessionId":"s1","message":{"role":"user","content":"`+long+`"}}`+"\n"), 0o644)
	claims := filepath.Join(dir, "claims.json")
	os.WriteFile(claims, []byte(`[`+
		`{"type":"fact","title":"Postgres is backed up nightly with pg_dump to S3","description":"Nightly pg_dump backups go to the S3 bucket and are kept 30 days."},`+
		`{"type":"learning","title":"restore.sh needs PGPASSFILE set","description":"Without PGPASSFILE the restore script prompts and hangs in cron."}]`), 0o644)
	out, errs, code := kbRun(t, "claude-code", "", "extract", "--transcript", transcript, "--cmd", "cat "+claims)
	if code != 0 || !strings.Contains(out, "added 1 claim(s), 0 duplicate(s), 1 already in the repo's docs") || !strings.Contains(errs, "already documented in docs/ops.md") {
		t.Fatalf("extract: %d\n%s\n%s", code, out, errs)
	}
	// The extraction prompt lists the doc sections it must not restate.
	fresh := filepath.Join(dir, "s2.jsonl")
	data, _ := os.ReadFile(transcript)
	os.WriteFile(fresh, data, 0o644)
	out = must(t, "claude-code", "extract", "--transcript", fresh, "--dry-run")
	if !strings.Contains(out, "Already documented in the repository (do not restate):") || !strings.Contains(out, "- docs/ops.md § Backups") {
		t.Fatalf("prompt lacks doc headings:\n%s", out)
	}
}

// hubEnv isolates HOME, the hub and agent config dirs in a temp directory.
func hubEnv(t *testing.T) (base, home string) {
	t.Helper()
	base = t.TempDir()
	home = filepath.Join(base, "home")
	os.MkdirAll(home, 0o755)
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("CODEX_HOME", "")
	t.Setenv("KB_HOME", filepath.Join(home, ".kb"))
	t.Setenv("KB_ROOT", "")
	t.Setenv("GIT_AUTHOR_NAME", "t")
	t.Setenv("GIT_AUTHOR_EMAIL", "t@t")
	t.Setenv("GIT_COMMITTER_NAME", "t")
	t.Setenv("GIT_COMMITTER_EMAIL", "t@t")
	wd, _ := os.Getwd()
	t.Cleanup(func() { os.Chdir(wd) })
	os.Chdir(base)
	return base, home
}

func mkRemoteRepo(t *testing.T, base, name string) string {
	t.Helper()
	dir := filepath.Join(base, "GolandProjects", name)
	os.MkdirAll(filepath.Join(dir, "pkg"), 0o755)
	os.WriteFile(filepath.Join(dir, "pkg", "x.go"), []byte("package pkg\n"), 0o644)
	git(t, dir, "init", "-q", "-b", "main")
	git(t, dir, "remote", "add", "origin", "git@github.com:AtlasCloudTeam/"+name+".git")
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-qm", "init")
	return dir
}

// fakeModel writes a script that saves the prompt and prints a fixed reply.
func fakeModel(t *testing.T, dir, name, reply string) (cmd, promptFile string) {
	t.Helper()
	promptFile = filepath.Join(dir, name+".prompt")
	replyFile := filepath.Join(dir, name+".reply")
	os.WriteFile(replyFile, []byte(reply), 0o644)
	script := filepath.Join(dir, name+".sh")
	os.WriteFile(script, []byte("#!/bin/sh\ncat > '"+promptFile+"'\ncat '"+replyFile+"'\n"), 0o755)
	return script, promptFile
}

func TestRulesAreDeliveredEverywhere(t *testing.T) {
	base, home := hubEnv(t)
	repo := mkRemoteRepo(t, base, "aiproxy")
	must(t, "", "hub", "init")
	must(t, "", "install", "claude", "codex")
	// A block written by some other hub must never be touched.
	other := filepath.Join(home, ".gemini", "GEMINI.md")
	os.MkdirAll(filepath.Dir(other), 0o755)
	foreign := "<!-- kb:begin (managed by 'kb install'; re-run it to update, edits here are overwritten) -->\nold\n<!-- kb:source /somewhere/else -->\n<!-- kb:end -->\n"
	os.WriteFile(other, []byte(foreign), 0o644)

	os.Chdir(repo)
	out := must(t, "claude-code", "add", "rule", "Use pnpm, never npm", "-d", "lockfile is pnpm-lock.yaml", "-e", "user said so")
	if !strings.Contains(out, "aiproxy/conventions/use-pnpm-never-npm [verified]") {
		t.Fatal(out)
	}
	must(t, "claude-code", "add", "rule", "Write commit messages in English", "-d", "team convention", "-e", "user said so", "--global")
	must(t, "claude-code", "add", "rule", "Prefer table-driven tests", "-d", "inferred from the codebase")

	out = must(t, "codex", "rules")
	if !strings.Contains(out, "Use pnpm, never npm") || !strings.Contains(out, "Write commit messages in English") || !strings.Contains(out, "Proposed rules awaiting confirmation") {
		t.Fatalf("kb rules:\n%s", out)
	}
	// Claude Code: SessionStart pushes the rules into the session.
	hook := `{"session_id":"s","hook_event_name":"SessionStart","cwd":"` + repo + `"}`
	out, _, _ = kbRun(t, "claude-code", hook, "hook", "claude")
	if !strings.Contains(out, "Rules from kb") || !strings.Contains(out, "Use pnpm, never npm") || strings.Contains(out, "- Prefer table-driven tests") || !strings.Contains(out, "proposed rule(s) not applied") {
		t.Fatalf("SessionStart:\n%s", out)
	}
	// Codex: global rules are kept current inside ~/.codex/AGENTS.md.
	data, _ := os.ReadFile(filepath.Join(home, ".codex", "AGENTS.md"))
	if !strings.Contains(string(data), "Write commit messages in English") || strings.Contains(string(data), "pnpm") {
		t.Fatalf("~/.codex/AGENTS.md:\n%s", data)
	}
	if got, _ := os.ReadFile(other); string(got) != foreign {
		t.Fatalf("foreign block was modified:\n%s", got)
	}
}

func TestImportAgentMemories(t *testing.T) {
	base, home := hubEnv(t)
	repo := mkRemoteRepo(t, base, "aiproxy")
	must(t, "", "hub", "init")
	// Claude Code auto memory for the aiproxy project, located via a transcript's cwd.
	pdir := filepath.Join(home, ".claude", "projects", "-x-aiproxy")
	os.MkdirAll(filepath.Join(pdir, "memory"), 0o755)
	os.WriteFile(filepath.Join(pdir, "t.jsonl"), []byte(`{"type":"user","cwd":"`+repo+`","sessionId":"s1","message":{"role":"user","content":"hi"}}`+"\n"), 0o644)
	os.WriteFile(filepath.Join(pdir, "memory", "MEMORY.md"), []byte("- The staging cluster rejects images without a digest pin.\n- User prefers short PR descriptions.\n"), 0o644)
	// Codex memories are cross-project.
	os.MkdirAll(filepath.Join(home, ".codex", "memories"), 0o755)
	os.WriteFile(filepath.Join(home, ".codex", "memories", "MEMORY.md"), []byte("- The user wants Go errors wrapped with %w everywhere.\n"), 0o644)

	claudeCmd, claudePrompt := fakeModel(t, base, "claude", `[{"type":"learning","title":"Staging rejects images without a digest pin","description":"Deploys to staging fail unless the image is pinned by digest."},{"type":"convention","project":"global","title":"Keep PR descriptions short","description":"User preference."}]`)
	out := must(t, "", "extract", "--scan", "claude-memory", "--cmd", claudeCmd)
	if !strings.Contains(out, "added 2") {
		t.Fatalf("claude memory import:\n%s", out)
	}
	prompt, _ := os.ReadFile(claudePrompt)
	if !strings.Contains(string(prompt), "saved in its private memory") || !strings.Contains(string(prompt), "digest pin") {
		t.Fatalf("prompt:\n%s", prompt)
	}
	os.Chdir(repo)
	show := must(t, "codex", "show", "aiproxy/learnings/staging-rejects-images-without-a-digest-pin")
	if !strings.Contains(show, "trust: verified") || !strings.Contains(show, "generated: claude-code") || !strings.Contains(show, "saved in claude-code's memory (~/.claude/projects/-x-aiproxy/memory/MEMORY.md)") {
		t.Fatalf("imported entry:\n%s", show)
	}
	if out := must(t, "codex", "rules"); !strings.Contains(out, "Keep PR descriptions short") {
		t.Fatalf("memory preference should be a delivered global rule:\n%s", out)
	}
	// Unchanged files are not re-read.
	if out := must(t, "", "extract", "--scan", "claude-memory", "--cmd", claudeCmd); !strings.Contains(out, "unchanged") {
		t.Fatalf("second scan:\n%s", out)
	}
	codexCmd, _ := fakeModel(t, base, "codex", `[{"type":"convention","title":"Wrap Go errors with %w","description":"User wants it in every repo."}]`)
	out = must(t, "", "extract", "--scan", "codex-memory", "--cmd", codexCmd)
	if !strings.Contains(out, "added 1, duplicates 0, already documented 0 [global/conventions/wrap-go-errors-with-w]") {
		t.Fatalf("codex memory import:\n%s", out)
	}
}

func TestImportRuleFiles(t *testing.T) {
	base, home := hubEnv(t)
	repo := mkRemoteRepo(t, base, "aiproxy")
	must(t, "", "hub", "init")
	must(t, "", "install", "claude")
	// The user's own Claude rules, around kb's managed block.
	claudeMD := filepath.Join(home, ".claude", "CLAUDE.md")
	data, _ := os.ReadFile(claudeMD)
	os.WriteFile(claudeMD, []byte("# My rules\n- Answer in English.\n- Never run git push --force.\n\n"+string(data)), 0o644)
	os.MkdirAll(filepath.Join(repo, ".cursor", "rules"), 0o755)
	os.WriteFile(filepath.Join(repo, ".cursor", "rules", "go.mdc"), []byte("---\nalwaysApply: true\n---\nUse testify/require in tests.\n"), 0o644)

	userCmd, userPrompt := fakeModel(t, base, "user", `[{"title":"Never run git push --force","description":"user rule"},{"title":"Answer in English","description":"user rule"}]`)
	os.Chdir(repo)
	out := must(t, "", "rules", "import", "--user", "--cmd", userCmd)
	if !strings.Contains(out, "~/.claude/CLAUDE.md: added 2") {
		t.Fatalf("user rules import:\n%s", out)
	}
	prompt, _ := os.ReadFile(userPrompt)
	if strings.Contains(string(prompt), "Shared knowledge (kb)") || !strings.Contains(string(prompt), "Never run git push --force") {
		t.Fatalf("kb's own block must be stripped from the prompt:\n%s", prompt)
	}
	repoCmd, _ := fakeModel(t, base, "repo", `[{"title":"Use testify/require in tests","description":"from Cursor rules"}]`)
	out = must(t, "", "rules", "import", "--repo", "--cmd", repoCmd)
	if !strings.Contains(out, "go.mdc: added 1, duplicates 0, already documented 0 [aiproxy/conventions/use-testify-require-in-tests]") {
		t.Fatalf("repo rules import:\n%s", out)
	}
	out = must(t, "codex", "rules")
	for _, want := range []string{"Use testify/require in tests", "Never run git push --force", "Answer in English"} {
		if !strings.Contains(out, want) {
			t.Errorf("codex should now get %q:\n%s", want, out)
		}
	}
}

func TestSeedProjectMemories(t *testing.T) {
	base, home := hubEnv(t)
	aiproxy := mkRemoteRepo(t, base, "aiproxy")
	kubedl := mkRemoteRepo(t, base, "kubedl")
	os.MkdirAll(filepath.Join(aiproxy, "docs"), 0o755)
	os.WriteFile(filepath.Join(aiproxy, "docs", "ops.md"), []byte("# Ops\n\n## Deploy\n\nDeploys go through Argo CD with a manual sync to the prod cluster.\n"), 0o644)
	git(t, aiproxy, "add", "-A")
	git(t, aiproxy, "commit", "-qm", "docs")
	must(t, "", "hub", "init")
	os.Chdir(aiproxy)
	must(t, "claude-code", "project") // registers the checkout
	must(t, "claude-code", "add", "fact", "aiproxy listens on 8080", "-d", "HTTP port.", "-e", "main.go")
	os.Chdir(kubedl)
	must(t, "claude-code", "add", "fact", "kubedl uses volcano", "-d", "Scheduler.", "-e", "values.yaml")

	// An agent seeds what it remembers about particular projects, in one call.
	os.Chdir(base)
	seed := `[
  {"type":"learning","project":"aiproxy","title":"Upstream 429 storms need jittered backoff","description":"Retries without jitter amplify throttling.","resources":["pkg/**"]},
  {"type":"rule","project":"kubedl","title":"Create the volcano queue before submitting jobs","description":"Jobs pend forever otherwise."},
  {"type":"rule","global":true,"title":"Keep PR descriptions short","description":"User preference."},
  {"type":"fact","project":"aiproxy","title":"aiproxy listens on 8080","description":"HTTP port."},
  {"type":"fact","project":"aiproxy","title":"Deploys go through Argo CD with a manual sync to prod","description":"Argo CD manual sync to the prod cluster."},
  {"type":"learning","project":"kubdl","title":"Typo project","description":"should be refused"}
]`
	out, errs, code := kbRun(t, "claude-code", seed, "add", "--from", "-", "-e", "from my claude-code memory")
	if code != 0 {
		t.Fatalf("seed: %s %s", out, errs)
	}
	for _, want := range []string{
		"added      aiproxy/learnings/upstream-429-storms-need-jittered-backoff [verified]",
		"added      kubedl/conventions/create-the-volcano-queue-before-submitting-jobs [verified]",
		"added      global/conventions/keep-pr-descriptions-short [verified]",
		`duplicate  "aiproxy listens on 8080": already aiproxy/facts/aiproxy-listens-on-8080`,
		`documented "Deploys go through Argo CD with a manual sync to prod": the repo's docs already say it in aiproxy:docs/ops.md:3`,
		`skipped    "Typo project": unknown project "kubdl" (did you mean "kubedl"?)`,
		"seeded 3 of 6 (1 duplicate, 1 already documented, 1 skipped)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("seed output missing %q:\n%s", want, out)
		}
	}
	if _, err := os.Stat(filepath.Join(home, ".kb", "projects", "kubdl")); err == nil {
		t.Fatal("typo created a stray project")
	}
	// The seeded rule reaches agents working in kubedl.
	os.Chdir(kubedl)
	if out := must(t, "codex", "rules"); !strings.Contains(out, "Create the volcano queue before submitting jobs") || !strings.Contains(out, "Keep PR descriptions short") {
		t.Fatalf("seeded rules not delivered:\n%s", out)
	}
}

func TestMemoryScanForParticularProjects(t *testing.T) {
	base, home := hubEnv(t)
	aiproxy := mkRemoteRepo(t, base, "aiproxy")
	kubedl := mkRemoteRepo(t, base, "kubedl")
	must(t, "", "hub", "init")
	for name, dir := range map[string]string{"aiproxy": aiproxy, "kubedl": kubedl} {
		pdir := filepath.Join(home, ".claude", "projects", "-x-"+name)
		os.MkdirAll(filepath.Join(pdir, "memory"), 0o755)
		os.WriteFile(filepath.Join(pdir, "t.jsonl"), []byte(`{"type":"user","cwd":"`+dir+`","message":{"role":"user","content":"hi"}}`+"\n"), 0o644)
		os.WriteFile(filepath.Join(pdir, "memory", "MEMORY.md"), []byte("- "+name+" note that is long enough to import.\n"), 0o644)
	}
	cmd, _ := fakeModel(t, base, "m", `[{"type":"learning","title":"A remembered gotcha","description":"From memory."}]`)
	out := must(t, "", "extract", "--scan", "claude-memory", "-p", "aiproxy", "--cmd", cmd)
	if !strings.Contains(out, "-x-aiproxy/memory/MEMORY.md: added 1") || !strings.Contains(out, "-x-kubedl/memory: skipped (not in the selected projects)") {
		t.Fatalf("filtered scan:\n%s", out)
	}
	// The filtered run did not mark files as done: a full scan still takes kubedl.
	out = must(t, "", "extract", "--scan", "claude-memory", "--cmd", cmd)
	if !strings.Contains(out, "-x-kubedl/memory/MEMORY.md: added 1") {
		t.Fatalf("full scan after filtered one:\n%s", out)
	}
}

// A populated hub: two projects with their own rules and health, a global
// rule for everyone and one restricted to kubedl.
func populatedHub(t *testing.T) (base, home, aiproxy, kubedl string) {
	t.Helper()
	base, home = hubEnv(t)
	aiproxy = mkRemoteRepo(t, base, "aiproxy")
	kubedl = mkRemoteRepo(t, base, "kubedl")
	must(t, "", "hub", "init")
	must(t, "", "install", "claude", "codex")
	os.Chdir(aiproxy)
	must(t, "claude-code", "add", "rule", "Use pnpm, never npm", "-d", "lockfile is pnpm-lock.yaml", "-e", "user said so")
	must(t, "claude-code", "add", "rule", "Prefer table-driven tests", "-d", "inferred from the codebase")
	must(t, "claude-code", "add", "learning", "Retries need idempotency keys", "-d", "Upstream jobs duplicate without keys.", "-r", "pkg/**", "-e", "reproduced in pkg/x_test.go")
	must(t, "claude-code", "add", "rule", "Write commit messages in English", "-d", "team convention", "-e", "user said so", "--global")
	must(t, "claude-code", "add", "rule", "Tag images with the git sha", "-d", "deploys are traced by sha", "-e", "user said so", "--global", "--for", "kubedl")
	os.Chdir(kubedl)
	must(t, "claude-code", "add", "rule", "Submit jobs through volcano queues", "-d", "the default queue is throttled", "-e", "user said so")
	// aiproxy's code moves on: its learning is now stale there.
	os.WriteFile(filepath.Join(aiproxy, "pkg", "x.go"), []byte("package pkg\n// changed\n"), 0o644)
	git(t, aiproxy, "commit", "-qam", "change")
	os.Chdir(base)
	return base, home, aiproxy, kubedl
}

func TestRuleScopeOnEverySurface(t *testing.T) {
	base, home, aiproxy, kubedl := populatedHub(t)
	const (
		pnpm, volcano, english, sha = "Use pnpm, never npm", "Submit jobs through volcano queues", "Write commit messages in English", "Tag images with the git sha"
	)
	expect := func(name, out string, want, not []string) {
		t.Helper()
		for _, w := range want {
			if !strings.Contains(out, w) {
				t.Errorf("%s: missing %q\n%s", name, w, out)
			}
		}
		for _, n := range not {
			if strings.Contains(out, n) {
				t.Errorf("%s: must not contain %q\n%s", name, n, out)
			}
		}
	}
	hook := func(cwd string) string {
		out, _, code := kbRun(t, "claude-code", `{"session_id":"s","hook_event_name":"SessionStart","cwd":"`+cwd+`"}`, "hook", "claude")
		if code != 0 {
			t.Fatalf("hook exit %d", code)
		}
		return out
	}
	mcpInstructions := func() string {
		out, _, code := kbRun(t, "", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","clientInfo":{"name":"cursor"}}}`+"\n", "mcp")
		if code != 0 {
			t.Fatalf("mcp exit %d", code)
		}
		var m struct {
			Result struct {
				Instructions string `json:"instructions"`
			} `json:"result"`
		}
		if err := json.Unmarshal([]byte(out), &m); err != nil {
			t.Fatalf("mcp reply: %v\n%s", err, out)
		}
		return m.Result.Instructions
	}

	// No recognized project: only the rules that apply everywhere, on every
	// surface, and no health news from unrelated projects.
	os.Chdir(base)
	projectOnly := []string{pnpm, volcano, sha, "table-driven"}
	out := hook(base)
	expect("hook, no project", out, []string{english}, append(projectOnly, "stale entr", "proposed rule", "aiproxy/"))
	expect("mcp, no project", mcpInstructions(), []string{english}, projectOnly)
	out = must(t, "codex", "rules")
	expect("kb rules, no project", out, []string{english, "not inside a project"}, projectOnly)
	expect("kb rules -q, no project", must(t, "codex", "rules", "-q"), []string{english}, append(projectOnly, "not inside a project"))
	// Explicit scopes from the same place.
	expect("kb rules -p kubedl", must(t, "codex", "rules", "-p", "kubedl"), []string{volcano, sha, english, "kubedl:"}, []string{pnpm, "table-driven"})
	expect("kb rules --all", must(t, "codex", "rules", "--all"), []string{pnpm, volcano, sha, english, "Prefer table-driven tests", "(only for kubedl)", "for inspection"}, nil)
	// User-level blocks are read in every repository.
	data, _ := os.ReadFile(filepath.Join(home, ".codex", "AGENTS.md"))
	expect("~/.codex/AGENTS.md", string(data), []string{english}, projectOnly)

	// Discovery keeps its all-project default; only the rule count narrows.
	expect("search, no project", must(t, "codex", "search", "idempotency retries"), []string{"aiproxy/learnings/retries-need-idempotency-keys"}, nil)
	var st struct {
		Scope        string `json:"scope"`
		Total        int    `json:"total"`
		Stale        int    `json:"stale"`
		Rules        int    `json:"rules_delivered"`
		RulesScope   string `json:"rules_scope"`
		RulesOmitted *int   `json:"rules_omitted_by_cap"`
	}
	if err := json.Unmarshal([]byte(must(t, "codex", "status", "--json")), &st); err != nil {
		t.Fatal(err)
	}
	if st.Scope != "all projects" || st.Total != 6 || st.Stale != 1 || st.Rules != 1 || st.RulesScope != "global only" || st.RulesOmitted == nil {
		t.Fatalf("unscoped status: %+v", st)
	}
	expect("status, no project", must(t, "codex", "status"), []string{"scope: all projects", "knowledge: 6 active", "rules that apply (global only): 1"}, nil)
	expect("status -p kubedl", must(t, "codex", "status", "-p", "kubedl"), []string{"rules that apply (kubedl + global): 3"}, nil)
	expect("status --all", must(t, "codex", "status", "--all"), []string{"rules that apply (all projects): 4"}, nil)

	// Recognized project: its own rules plus the global ones that apply.
	os.Chdir(aiproxy)
	expect("hook in aiproxy", hook(aiproxy), []string{pnpm, english, "proposed rule(s) not applied", "1 stale entr(ies) in aiproxy + global"}, []string{volcano, sha})
	expect("mcp in aiproxy", mcpInstructions(), []string{pnpm, english}, []string{volcano, sha, "table-driven"})
	expect("kb rules in aiproxy", must(t, "codex", "rules"), []string{pnpm, english, "Prefer table-driven tests"}, []string{volcano, sha, "not inside a project"})
	os.Chdir(kubedl)
	expect("hook in kubedl", hook(kubedl), []string{volcano, sha, english}, []string{pnpm, "stale entr", "proposed rule"})
	expect("mcp in kubedl", mcpInstructions(), []string{volcano, sha, english}, []string{pnpm})

	// No hub at all, outside any repository: every surface is silent.
	t.Setenv("KB_HOME", filepath.Join(base, "absent"))
	os.Chdir(base)
	if out, _, code := kbRun(t, "claude-code", `{"hook_event_name":"SessionStart","cwd":"`+base+`"}`, "hook", "claude"); code != 0 || out != "" {
		t.Fatalf("hook without a hub: %d %q", code, out)
	}
	if _, errs, code := kbRun(t, "codex", "", "rules"); code == 0 || !strings.Contains(errs, "no knowledge base found") {
		t.Fatalf("kb rules without a hub: %d %q", code, errs)
	}
}

func TestCheckoutBaselineCLI(t *testing.T) {
	base, _, aiproxy, _ := populatedHub(t)
	const repo = "github.com/atlascloudteam/aiproxy"
	id := "aiproxy/learnings/retries-need-idempotency-keys"
	real := func(p string) string { r, _ := filepath.EvalSymlinks(p); return r }
	aiproxy = real(aiproxy)
	search := func(args ...string) (hitJSON, map[string]any) {
		t.Helper()
		out := must(t, "codex", append([]string{"search", "idempotency retries", "--json", "--no-docs"}, args...)...)
		var hits []hitJSON
		var raw []map[string]any
		if err := json.Unmarshal([]byte(out), &hits); err != nil || len(hits) != 1 {
			t.Fatalf("search: %v\n%s", err, out)
		}
		json.Unmarshal([]byte(out), &raw)
		return hits[0], raw[0]
	}

	// A second, independent clone that has not changed pkg/ since the anchor
	// would give a different answer: kb must not pick between them silently.
	os.Chdir(base)
	h, raw := search()
	if !h.Stale || h.CodeFreshness != "changed" || h.Baseline == nil || h.Baseline.Path != aiproxy || h.Baseline.Repo != repo || h.Baseline.Head == "" || h.Baseline.Selection != "primary worktree" {
		t.Fatalf("single clone: %+v baseline %+v", h, h.Baseline)
	}
	if raw["stale"] != true || raw["stale_reason"] == nil {
		t.Fatalf("legacy stale fields: %v", raw)
	}
	anchor := strings.TrimSpace(git(t, aiproxy, "rev-parse", "HEAD~1"))
	clone := filepath.Join(base, "aiproxy-at-anchor")
	git(t, base, "clone", "-q", aiproxy, clone)
	git(t, clone, "remote", "set-url", "origin", "git@github.com:AtlasCloudTeam/aiproxy.git")
	git(t, clone, "reset", "-q", "--hard", anchor)
	clone = real(clone)
	os.Chdir(clone)
	must(t, "codex", "project") // running kb in it registers it
	os.Chdir(base)

	h, raw = search()
	if h.CodeFreshness != "unknown" || h.FreshnessCode != "ambiguous_checkout" || h.Stale || !strings.Contains(h.FreshnessReason, "kb project checkout set aiproxy") {
		t.Fatalf("two clones: %+v", h)
	}
	// Legacy JSON: unknown alone leaves "stale" false, i.e. absent.
	if _, present := raw["stale"]; present {
		t.Fatalf("unknown must not set the legacy stale field: %v", raw)
	}
	out := must(t, "codex", "search", "idempotency retries", "--no-docs")
	if !strings.Contains(out, id+" [verified,unknown-freshness] ") {
		t.Fatalf("badge:\n%s", out)
	}
	out = must(t, "codex", "show", id)
	if !strings.Contains(out, "trust: verified") || !strings.Contains(out, "code freshness: unknown - 2 independent clones of "+repo) {
		t.Fatalf("show:\n%s", out)
	}
	out = must(t, "codex", "status")
	if !strings.Contains(out, "stale: 0") || !strings.Contains(out, "unknown 1") || !strings.Contains(out, "1 with several clones and no preferred checkout") {
		t.Fatalf("status:\n%s", out)
	}
	out = must(t, "codex", "project", "checkout", "list", "aiproxy")
	if !strings.Contains(out, "repo "+repo) || !strings.Contains(out, "baseline: none - 2 independent clones") || !strings.Contains(out, "kb project checkout set aiproxy --repo "+repo+" --path <checkout>") {
		t.Fatalf("checkout list:\n%s", out)
	}

	// An explicit baseline answers the same from any directory.
	for _, dir := range []string{base, aiproxy, clone} {
		os.Chdir(dir)
		if h, _ := search("--checkout", clone); h.CodeFreshness != "unchanged" || h.Stale || h.Baseline.Path != clone || h.Baseline.Selection != "explicit --checkout" {
			t.Fatalf("--checkout clone from %s: %+v baseline %+v", dir, h, h.Baseline)
		}
		if h, _ := search("--checkout", aiproxy); h.CodeFreshness != "changed" || !h.Stale {
			t.Fatalf("--checkout main from %s: %+v", dir, h)
		}
		if h, _ := search("--checkout", aiproxy, "--ref", "HEAD~1"); h.CodeFreshness != "unchanged" || h.Baseline.Ref != "HEAD~1" {
			t.Fatalf("--ref from %s: %+v baseline %+v", dir, h, h.Baseline)
		}
	}
	os.Chdir(base)
	if _, errs, code := kbRun(t, "codex", "", "search", "idempotency", "--checkout", base); code == 0 || !strings.Contains(errs, "not a git work tree") {
		t.Fatalf("--checkout outside git: %d %q", code, errs)
	}

	// A stored preference: local, validated, reversible.
	if _, errs, code := kbRun(t, "codex", "", "project", "checkout", "set", "aiproxy", "--repo", "github.com/atlascloudteam/kubedl", "--path", clone); code == 0 || !strings.Contains(errs, "is a checkout of "+repo) {
		t.Fatalf("wrong repo accepted: %d %q", code, errs)
	}
	out = must(t, "codex", "project", "checkout", "set", "aiproxy", "--repo", repo, "--path", clone)
	if !strings.Contains(out, "baseline for "+repo+" in project aiproxy on this machine: "+clone) {
		t.Fatalf("set: %s", out)
	}
	if h, _ := search(); h.CodeFreshness != "unchanged" || h.Baseline.Path != clone || h.Baseline.Selection != "preferred checkout" {
		t.Fatalf("preferred: %+v baseline %+v", h, h.Baseline)
	}
	out = must(t, "codex", "project", "checkout", "list", "aiproxy")
	if !strings.Contains(out, "(preferred)") || !strings.Contains(out, "baseline: preferred checkout") {
		t.Fatalf("checkout list with a preference:\n%s", out)
	}
	// Inside a checkout, that checkout is the baseline whatever is preferred.
	os.Chdir(aiproxy)
	if h, _ := search(); h.CodeFreshness != "changed" || h.Baseline.Selection != "current checkout" {
		t.Fatalf("current checkout: %+v baseline %+v", h, h.Baseline)
	}
	os.Chdir(base)
	must(t, "codex", "project", "checkout", "clear", "aiproxy")
	if h, _ := search(); h.CodeFreshness != "unknown" {
		t.Fatalf("after clear: %+v", h)
	}
	if st := strings.TrimSpace(git(t, os.Getenv("KB_HOME"), "status", "--porcelain")); st != "" {
		t.Fatalf("checkout preferences must stay out of the shared hub:\n%s", st)
	}
}

// Startup regression: a disputed global rule restricted to kubedl must not
// reach a session outside any project through the health brief, and a
// preference command must fail when it cannot save.
func TestStartupBriefAndPreferenceErrors(t *testing.T) {
	base, _, aiproxy, kubedl := populatedHub(t)
	const sha = "global/conventions/tag-images-with-the-git-sha"
	must(t, "codex", "verify", sha, "--dispute", "-e", "images are tagged with semver now")
	hook := func(cwd string) string {
		os.Chdir(cwd)
		out, _, _ := kbRun(t, "claude-code", `{"session_id":"s","hook_event_name":"SessionStart","cwd":"`+cwd+`"}`, "hook", "claude")
		return out
	}
	if out := hook(base); strings.Contains(out, sha) || strings.Contains(out, "disputed") || strings.Contains(out, "git sha") {
		t.Fatalf("hook outside a project carries a kubedl-only global rule:\n%s", out)
	}
	if out := hook(aiproxy); strings.Contains(out, sha) || strings.Contains(out, "disputed") {
		t.Fatalf("hook in aiproxy carries a kubedl-only global rule:\n%s", out)
	}
	if out := hook(kubedl); !strings.Contains(out, "1 disputed entr(ies): "+sha) {
		t.Fatalf("hook in kubedl should report its disputed rule:\n%s", out)
	}
	// Interactive status still finds it.
	os.Chdir(base)
	if out := must(t, "codex", "status"); !strings.Contains(out, sha) {
		t.Fatalf("kb status lost the disputed entry:\n%s", out)
	}

	if os.Geteuid() == 0 {
		return // root ignores directory permissions
	}
	local := filepath.Join(os.Getenv("KB_HOME"), ".kb")
	os.Chmod(local, 0o555)
	t.Cleanup(func() { os.Chmod(local, 0o755) })
	out, errs, code := kbRun(t, "codex", "", "project", "checkout", "set", "aiproxy", "--path", aiproxy)
	if code == 0 || strings.Contains(out, "baseline for") || !strings.Contains(errs, "was not saved") {
		t.Fatalf("set on read-only local state: exit %d\nstdout: %s\nstderr: %s", code, out, errs)
	}
	os.Chmod(local, 0o755)
	if out := must(t, "codex", "project", "checkout", "list", "aiproxy"); strings.Contains(out, "(preferred)") {
		t.Fatalf("nothing should have been saved:\n%s", out)
	}
}

// End to end: the SessionStart hook's stdout stays under Claude Code's
// 10,000-character hook limit however many rules apply, and kb status
// reports exactly what the hook delivered.
func TestSessionStartOutputStaysWithinHookLimit(t *testing.T) {
	base, _ := hubEnv(t)
	must(t, "", "hub", "init")
	var items []string
	for i := 0; i < 45; i++ {
		title := fmt.Sprintf("规则%02d：%s", i, strings.Repeat("始终优先使用文档中记录的方法而不是临时方案，", 12))
		items = append(items, fmt.Sprintf(`{"type":"rule","global":true,"title":%q,"description":"because the team agreed on it"}`, title))
	}
	out, errs, code := kbRun(t, "claude-code", "["+strings.Join(items, ",")+"]", "add", "--from", "-", "-e", "user said so", "--force")
	if code != 0 || !strings.Contains(out, "seeded 45 of 45") {
		t.Fatalf("seed: %d\n%s\n%s", code, out, errs)
	}
	units := func(s string) int {
		n := 0
		for _, r := range s {
			if r >= 0x10000 {
				n++
			}
			n++
		}
		return n
	}
	os.Chdir(base)
	hook, _, code := kbRun(t, "claude-code", `{"session_id":"s","hook_event_name":"SessionStart","cwd":"`+base+`"}`, "hook", "claude")
	if code != 0 || units(hook) > 9000 || units(hook) < 6000 {
		t.Fatalf("hook stdout is %d units (exit %d); it must stay under the 10,000-character hook limit and use its budget", units(hook), code)
	}
	rendered := strings.Count(hook, "\n- ")
	if rendered >= 40 || rendered < 20 || !strings.Contains(hook, fmt.Sprintf("(+%d more rule(s) apply here but were left out", 45-rendered)) {
		t.Fatalf("hook rendered %d rules:\n%s", rendered, hook[len(hook)-200:])
	}
	var st struct {
		Rules    int `json:"rules_delivered"`
		Omitted  int `json:"rules_omitted_by_cap"`
		Delivery struct {
			Surface, Scope, Unit     string
			Budget, Size, Applicable int
			Rendered                 int
			ByCount                  int `json:"omitted_by_count"`
			BySize                   int `json:"omitted_by_size"`
		} `json:"rules_delivery"`
	}
	if err := json.Unmarshal([]byte(must(t, "claude-code", "status", "--json")), &st); err != nil {
		t.Fatal(err)
	}
	d := st.Delivery
	if st.Rules != 45 || st.Omitted != 45-rendered || d.Surface != "session-start" || d.Scope != "global only" || d.Unit != "utf16-code-units" || d.Budget != 9000 ||
		d.Size != units(hook) || d.Applicable != 45 || d.Rendered != rendered || d.ByCount != 5 || d.BySize != 40-rendered {
		t.Fatalf("status does not match what the hook delivered (%d rules, %d units): %+v", rendered, units(hook), st)
	}
	// The rules a cap left out can be listed on request.
	if n := strings.Count(must(t, "claude-code", "rules", "-q"), "\n- "); n != 40 {
		t.Fatalf("kb rules lists %d", n)
	}
	if n := strings.Count(must(t, "claude-code", "rules", "-q", "--all-rules"), "\n- "); n != 45 {
		t.Fatalf("kb rules --all-rules lists %d", n)
	}
}

// Claude Code's typed memory files are imported without a model, keep their
// identity across edits, renames and deletions, and carry the source's own
// dates instead of the time and commit of the import.
func TestClaudeMemoryDeterministicImport(t *testing.T) {
	base, home := hubEnv(t)
	repo := mkRemoteRepo(t, base, "aiproxy")
	must(t, "", "hub", "init")
	pdir := filepath.Join(home, ".claude", "projects", "-x-aiproxy")
	mem := filepath.Join(pdir, "memory")
	os.MkdirAll(mem, 0o755)
	os.WriteFile(filepath.Join(pdir, "t.jsonl"), []byte(`{"type":"user","cwd":"`+repo+`","sessionId":"s1","message":{"role":"user","content":"hi"}}`+"\n"), 0o644)
	write := func(name, content string) string {
		t.Helper()
		p := filepath.Join(mem, name)
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	topic := func(name, desc, typ, extra, body string) string {
		return "---\nname: " + name + "\ndescription: " + desc + "\ntype: " + typ + "\n" + extra + "---\n\n" + body + "\n"
	}
	write("MEMORY.md", "# Memory index\n\n- [Testing](feedback_testing.md) — integration tests hit a real database\n- [Release](project_release.md) — freeze dates\n")
	write("feedback_testing.md", topic("feedback-testing", "Integration tests must hit a real database", "feedback",
		"modified: 2026-10-20T08:00:00Z\noriginSessionId: 11111111-2222-3333-4444-555555555555\n",
		"Never mock the database in integration tests.\n\n**Why:** a mocked migration passed while production failed.\n**How to apply:** use the docker compose database."))
	write("project_release.md", topic("project-release", "Merge freeze before the mobile release", "project",
		"modified: 2025-01-10T09:30:00Z\n", "Non-critical merges are frozen from the Thursday before each mobile release."))
	ref := write("reference_dashboard.md", topic("reference-dashboard", "Latency dashboard the on-call watches", "reference", "", "The on-call latency dashboard is the board named api-latency in the monitoring workspace."))
	old := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	os.Chtimes(ref, old, old)
	write("user_role.md", topic("user-role", "Who the user is", "user", "", "The user is a backend engineer."))

	// Every file here is a typed topic: an extractor that cannot run proves
	// no model was called.
	scan := func(cmd string) string {
		t.Helper()
		os.Chdir(base)
		return must(t, "", "extract", "--scan", "claude-memory", "--cmd", cmd)
	}
	out := scan("/nonexistent/model")
	if !strings.Contains(out, "memory [aiproxy]: 3 imported, 0 updated, 0 unchanged, 1 index skipped, 1 user memories skipped") || strings.Contains(out, "error") || strings.Contains(out, "fallback") {
		t.Fatalf("first scan:\n%s", out)
	}
	const ruleID, factID, refID = "aiproxy/conventions/integration-tests-must-hit-a-real-database", "aiproxy/facts/merge-freeze-before-the-mobile-release", "aiproxy/facts/latency-dashboard-the-on-call-watches"
	show := func(id string) string { t.Helper(); os.Chdir(repo); return must(t, "codex", "show", id) }
	rule := show(ruleID)
	for _, want := range []string{"trust: verified", "generated: claude-code at ", "saved in claude-code's memory (~/.claude/projects/-x-aiproxy/memory/feedback_testing.md)",
		"source modified 2026-10-20T08:00:00Z", "ingested ", "from session 11111111-2222-3333-4444-555555555555", "Never mock the database", "tags: feedback"} {
		if !strings.Contains(rule, want) {
			t.Errorf("rule entry lacks %q:\n%s", want, rule)
		}
	}
	// Source-only: no commit from the checkout the import happened to run
	// near, and a convention has no deadline to invent.
	if strings.Contains(rule, "(commit ") || strings.Contains(rule, "stale_after") {
		t.Fatalf("an imported memory must not carry an import-time commit or deadline:\n%s", rule)
	}
	// It is the project's rule, not a global one.
	if out := must(t, "codex", "rules", "-q"); !strings.Contains(out, "aiproxy:\n- Integration tests must hit a real database - Never mock the database in integration tests.") {
		t.Fatalf("rules in aiproxy:\n%s", out)
	}
	os.Chdir(base)
	if out := must(t, "codex", "rules", "-q"); strings.Contains(out, "Integration tests") {
		t.Fatalf("feedback must not be promoted to the global scope:\n%s", out)
	}
	// Deadlines come from the source's date: an old note is due at once and
	// says what its deadline is based on; the observation date stays unknown.
	fact := show(factID)
	if !strings.Contains(fact, "stale_after: 2025-04-10T09:30:00Z (deadline from source_updated_at; when the claim was last observed is unknown)") || !strings.Contains(fact, "STALE: past stale_after 2025-04-10 (deadline from source_updated_at)") || strings.Contains(fact, "(commit ") {
		t.Fatalf("old project memory:\n%s", fact)
	}
	if r := show(refID); !strings.Contains(r, "stale_after: 2000-03-31T00:00:00Z (deadline from source_mtime;") || !strings.Contains(r, "source modified 2000-01-01T00:00:00Z") {
		t.Fatalf("memory without a modified field:\n%s", r)
	}
	if out := must(t, "codex", "search", "role backend engineer", "--all"); strings.Contains(out, "aiproxy/") {
		t.Fatalf("a user memory was imported:\n%s", out)
	}

	// Unchanged: nothing happens, however often it runs.
	for i := 0; i < 2; i++ {
		if out := scan("/nonexistent/model"); !strings.Contains(out, "0 imported, 0 updated, 3 unchanged") {
			t.Fatalf("rescan:\n%s", out)
		}
	}
	// Edited: the same entry gets a new revision; reading the source again
	// is not a newer check, so no commit appears and deadlines do not move.
	write("feedback_testing.md", topic("feedback-testing", "Integration tests must hit a real database", "feedback",
		"modified: 2026-11-01T08:00:00Z\n", "Never mock the database in integration or contract tests."))
	write("project_release.md", topic("project-release", "Merge freeze before the mobile release", "project",
		"modified: 2026-11-10T09:30:00Z\n", "Non-critical merges are frozen from the Wednesday before each mobile release."))
	if out := scan("/nonexistent/model"); !strings.Contains(out, "0 imported, 2 updated, 1 unchanged") {
		t.Fatalf("after edits:\n%s", out)
	}
	rule = show(ruleID)
	if !strings.Contains(rule, "revision: 2") || !strings.Contains(rule, "contract tests") || !strings.Contains(rule, "source modified 2026-11-01T08:00:00Z") || strings.Contains(rule, "(commit ") || !strings.Contains(rule, "trust: verified") {
		t.Fatalf("updated rule:\n%s", rule)
	}
	if fact = show(factID); !strings.Contains(fact, "revision: 2") || !strings.Contains(fact, "Wednesday") || !strings.Contains(fact, "stale_after: 2025-04-10T09:30:00Z (deadline from source_updated_at") {
		t.Fatalf("an edited source must not postpone the deadline:\n%s", fact)
	}
	os.Chdir(base)
	if out := must(t, "codex", "status", "--all"); !strings.Contains(out, "knowledge: 3 active") {
		t.Fatalf("an edit created another entry:\n%s", out)
	}

	// Edited by hand since the import, then the source changes: a conflict
	// to merge, never an overwrite.
	os.Chdir(repo)
	must(t, "codex", "update", ruleID, "-b", "Never mock the database; use the compose stack (see docs).", "-m", "pointed at the compose stack")
	write("feedback_testing.md", topic("feedback-testing", "Integration tests must hit a real database", "feedback", "", "Mocks are fine for unit tests only."))
	out = scan("/nonexistent/model")
	if !strings.Contains(out, "1 conflicts") || !strings.Contains(out, "conflict: feedback_testing.md changed, but "+ruleID+" was edited since the last import (revision 3, imported 2)") {
		t.Fatalf("conflict:\n%s", out)
	}
	if rule = show(ruleID); !strings.Contains(rule, "compose stack") || strings.Contains(rule, "Mocks are fine") {
		t.Fatalf("a manual edit was overwritten:\n%s", rule)
	}
	// The conflict stands until it is settled, and settling it by hand does
	// not touch the entry.
	if out := scan("/nonexistent/model"); !strings.Contains(out, "1 conflicts") || !strings.Contains(out, "conflict (unresolved): feedback_testing.md and "+ruleID) {
		t.Fatalf("a conflict must keep being reported:\n%s", out)
	}
	os.Chdir(base)
	if out := must(t, "", "extract", "--scan", "claude-memory", "--cmd", "/nonexistent/model", "--accept-conflicts"); strings.Contains(out, "conflict") {
		t.Fatalf("accepting conflicts:\n%s", out)
	}
	if out := scan("/nonexistent/model"); strings.Contains(out, "conflict") {
		t.Fatalf("after accepting:\n%s", out)
	}
	if rule = show(ruleID); !strings.Contains(rule, "revision: 3") || !strings.Contains(rule, "compose stack") {
		t.Fatalf("accepting a conflict changed the entry:\n%s", rule)
	}

	// Renamed: the same source, no new entry.
	os.Rename(filepath.Join(mem, "project_release.md"), filepath.Join(mem, "project_merge_freeze.md"))
	if out := scan("/nonexistent/model"); !strings.Contains(out, "1 renamed") || strings.Contains(out, "1 imported") || strings.Contains(out, "sources deleted") {
		t.Fatalf("rename:\n%s", out)
	}
	// Deleted: flagged, kept, and not flagged while the directory cannot be
	// read completely.
	os.Remove(ref)
	if os.Geteuid() != 0 {
		locked := write("zz_locked.md", topic("locked", "Locked note", "reference", "", "Unreadable for now."))
		os.Chmod(locked, 0o000)
		if out := scan("/nonexistent/model"); strings.Contains(out, "sources deleted") || !strings.Contains(out, "not read completely") {
			t.Fatalf("incomplete scan must not report deletions:\n%s", out)
		}
		os.Remove(locked)
	}
	out = scan("/nonexistent/model")
	if !strings.Contains(out, "1 sources deleted") || !strings.Contains(out, "source deleted: reference_dashboard.md no longer backs "+refID+"; the entry is kept") {
		t.Fatalf("deletion:\n%s", out)
	}
	if r := show(refID); !strings.Contains(r, "trust: verified") {
		t.Fatalf("the entry of a deleted source must be kept:\n%s", r)
	}
	must(t, "codex", "deprecate", refID, "-m", "dashboard was retired")
	if out := scan("/nonexistent/model"); strings.Contains(out, "sources deleted") {
		t.Fatalf("a retired entry needs no further review:\n%s", out)
	}

	// Files that are not a single typed note go to the model, visibly, and
	// an index's pointers to topic files are not sent again.
	write("MEMORY.md", "# Memory index\n\n- [Testing](feedback_testing.md) — integration tests hit a real database\n\nThe staging cluster rejects images without a digest pin.\n")
	write("legacy_notes.md", "The deploy bot needs the release label before it will merge anything at all.\n")
	write("odd_type.md", topic("odd", "Something else", "scratch", "", "A note of a type this adapter does not know about yet."))
	write("mixed.md", topic("mixed", "Several things", "project", "", "# One\n\nFirst topic of the file.\n\n# Two\n\nSecond topic of the file."))
	cmd, prompt := fakeModel(t, base, "fallback", `[]`)
	out = scan(cmd)
	for _, want := range []string{"4 sent to the model fallback", "fallback: MEMORY.md holds notes besides its 1 link(s)", "fallback: legacy_notes.md (no usable frontmatter)",
		`fallback: odd_type.md (unknown memory type "scratch")`, "fallback: mixed.md (several sections: not a single note)"} {
		if !strings.Contains(out, want) {
			t.Errorf("fallback report lacks %q:\n%s", want, out)
		}
	}
	// The prompt file holds the last call; MEMORY.md sorts first, so check
	// its content was reduced to its own notes through a dedicated run.
	os.Remove(filepath.Join(mem, "legacy_notes.md"))
	os.Remove(filepath.Join(mem, "odd_type.md"))
	os.Remove(filepath.Join(mem, "mixed.md"))
	write("MEMORY.md", "# Memory index\n\n- [Testing](feedback_testing.md) — integration tests hit a real database\n\nThe staging cluster rejects images without a pinned digest.\n")
	scan(cmd)
	if p, _ := os.ReadFile(prompt); !strings.Contains(string(p), "pinned digest") || strings.Contains(string(p), "feedback_testing.md") {
		t.Fatalf("index fallback prompt should hold only the index's own notes:\n%s", p)
	}

	// A typed file the earlier model-based import already took is not
	// imported a second time: unchanged it is done, changed it stays on the
	// model path.
	legacy := write("legacy_typed.md", topic("legacy-typed", "Deploys need the release label", "project", "", "The deploy bot merges only pull requests that carry the release label."))
	typed, _ := os.ReadFile(legacy)
	if err := legacyImportState(filepath.Join(os.Getenv("KB_HOME"), ".kb", "extract"), legacy, typed); err != nil {
		t.Fatal(err)
	}
	if out := scan("/nonexistent/model"); !strings.Contains(out, "1 imported earlier through the model") || strings.Contains(out, " 1 imported,") {
		t.Fatalf("a file the model path already imported:\n%s", out)
	}
	if out := scan("/nonexistent/model"); strings.Contains(out, "imported earlier") {
		t.Fatalf("rescan of a file the model path imported:\n%s", out)
	}
	write("legacy_typed.md", topic("legacy-typed", "Deploys need the release label", "project", "", "The deploy bot merges only labelled pull requests, and only on weekdays."))
	if out := scan(cmd); !strings.Contains(out, "fallback: legacy_typed.md (changed since the model imported it") {
		t.Fatalf("a changed file the model path imported:\n%s", out)
	}

	// A directory with no project to attach to is reported, not skipped silently.
	os.MkdirAll(filepath.Join(home, ".claude", "projects", "-x-unknown", "memory"), 0o755)
	os.WriteFile(filepath.Join(home, ".claude", "projects", "-x-unknown", "memory", "a.md"), []byte(topic("a", "A note", "project", "", "Body of a note without a project.")), 0o644)
	if out := scan(cmd); !strings.Contains(out, "-x-unknown/memory: skipped (no project could be established") {
		t.Fatalf("unmapped directory:\n%s", out)
	}
	// The ledger is local state: nothing of it reaches the shared hub.
	if st := strings.TrimSpace(git(t, os.Getenv("KB_HOME"), "status", "--porcelain")); st != "" {
		t.Fatalf("import state leaked into the hub:\n%s", st)
	}
}

// legacyImportState writes the state file the model-based importer keeps for
// a file it has imported (path-keyed, with the hash of its trimmed content).
func legacyImportState(dir, path string, content []byte) error {
	sum := sha256.Sum256([]byte(strings.TrimSpace(string(content))))
	key := sha1.Sum([]byte(path))
	data, _ := json.Marshal(map[string]string{"path": path, "hash": hex.EncodeToString(sum[:]), "updated": "2026-11-01T00:00:00Z"})
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, hex.EncodeToString(key[:8])+".json"), data, 0o644)
}
