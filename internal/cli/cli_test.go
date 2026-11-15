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
	// What a Claude Code session has at its start: the managed rules file it
	// loads itself plus what the SessionStart hook prints.
	hook := func(cwd string) string {
		out, _, code := kbRun(t, "claude-code", `{"session_id":"s","hook_event_name":"SessionStart","cwd":"`+cwd+`"}`, "hook", "claude")
		if code != 0 {
			t.Fatalf("hook exit %d", code)
		}
		native, _ := os.ReadFile(filepath.Join(home, ".claude", "rules", "kb.md"))
		return string(native) + out
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
	// A link that leads somewhere else is content of the index, not a
	// pointer to a topic file: no topic file preserves it.
	write("MEMORY.md", "# Memory index\n\n- [Testing](feedback_testing.md) — integration tests hit a real database\n- [Production runbook](https://example.com/runbook) — follow this during incidents, it is the only current copy\n- [Old notes](notes_that_do_not_exist.md) — a pointer to a file that is not there\n")
	if out := scan(cmd); !strings.Contains(out, "fallback: MEMORY.md holds notes besides its 1 link(s)") || strings.Contains(out, "1 index skipped") {
		t.Fatalf("an index holding external references:\n%s", out)
	}
	if p, _ := os.ReadFile(prompt); !strings.Contains(string(p), "Production runbook") || !strings.Contains(string(p), "notes_that_do_not_exist.md") || strings.Contains(string(p), "feedback_testing.md") {
		t.Fatalf("external references must reach the fallback, topic pointers must not:\n%s", p)
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

// Identity edge cases found in review: a tracked memory that stops being a
// single note, a rename target matched twice, and a deleted source whose
// entry was checked again.
func TestClaudeMemoryIdentityEdgeCases(t *testing.T) {
	base, home := hubEnv(t)
	repo := mkRemoteRepo(t, base, "aiproxy")
	must(t, "", "hub", "init")
	pdir := filepath.Join(home, ".claude", "projects", "-x-aiproxy")
	mem := filepath.Join(pdir, "memory")
	os.MkdirAll(mem, 0o755)
	os.WriteFile(filepath.Join(pdir, "t.jsonl"), []byte(`{"type":"user","cwd":"`+repo+`","sessionId":"s1","message":{"role":"user","content":"hi"}}`+"\n"), 0o644)
	write := func(name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(mem, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	topic := func(name, desc, typ, body string) string {
		return "---\nname: " + name + "\ndescription: " + desc + "\ntype: " + typ + "\n---\n\n" + body + "\n"
	}
	// An extractor that cannot run: any model call shows up as an error.
	scan := func(extra ...string) string {
		t.Helper()
		os.Chdir(base)
		return must(t, "", append([]string{"extract", "--scan", "claude-memory", "--cmd", "/nonexistent/model"}, extra...)...)
	}
	show := func(id string) string { t.Helper(); os.Chdir(repo); return must(t, "codex", "show", id) }

	write("feedback_logging.md", topic("feedback-logging", "Log with structured fields only", "feedback", "Never build log lines with string formatting."))
	write("project_alpha.md", topic("shared-name", "Alpha rollout is gated on the canary", "project", "The alpha rollout waits for the canary to pass for a day."))
	write("project_beta.md", topic("shared-name", "Beta customers get weekly builds", "project", "Beta customers receive a build every Tuesday."))
	write("reference_runbook.md", topic("reference-runbook", "Incident runbook location", "reference", "The incident runbook is the pinned page in the operations space."))
	if out := scan(); !strings.Contains(out, "4 imported") {
		t.Fatalf("first scan:\n%s", out)
	}
	const ruleID, alphaID, betaID, runbookID = "aiproxy/conventions/log-with-structured-fields-only", "aiproxy/facts/alpha-rollout-is-gated-on-the-canary", "aiproxy/facts/beta-customers-get-weekly-builds", "aiproxy/facts/incident-runbook-location"

	// 1. A tracked memory grows into several sections. The model path only
	// adds entries, so it must not take the file: the delivered rule would
	// stay as it was next to a new one.
	write("feedback_logging.md", topic("feedback-logging", "Log with structured fields only", "feedback", "# Logging\n\nUse structured fields.\n\n# Metrics\n\nAlso count every retry."))
	out := scan()
	if !strings.Contains(out, "1 conflicts") || !strings.Contains(out, "conflict: feedback_logging.md can no longer be imported as one note (several sections: not a single note), so "+ruleID+" was left as it is") ||
		strings.Contains(out, "fallback") || strings.Contains(out, "error") {
		t.Fatalf("a tracked source that became a mixed note:\n%s", out)
	}
	if out := scan(); !strings.Contains(out, "1 conflicts") {
		t.Fatalf("the conflict must stand:\n%s", out)
	}
	if rule := show(ruleID); !strings.Contains(rule, "revision: 1") || !strings.Contains(rule, "string formatting") {
		t.Fatalf("the tracked entry changed:\n%s", rule)
	}
	os.Chdir(base)
	if out := must(t, "codex", "status", "--all"); !strings.Contains(out, "knowledge: 4 active") {
		t.Fatalf("a second entry was created:\n%s", out)
	}
	// Merged by hand, then accepted: the merged revision is the new baseline.
	os.Chdir(repo)
	must(t, "codex", "update", ruleID, "-b", "Use structured fields for logs, and count every retry.", "-m", "merged the memory's new sections")
	if out := scan("--accept-conflicts"); strings.Contains(out, "conflict") {
		t.Fatalf("accepting:\n%s", out)
	}
	if out := scan(); strings.Contains(out, "conflict") || strings.Contains(out, "fallback") {
		t.Fatalf("after accepting, the unchanged mixed file is settled:\n%s", out)
	}
	// The source becomes a single note again and is edited: it updates the
	// merged entry instead of conflicting with the pre-merge revision.
	write("feedback_logging.md", topic("feedback-logging", "Log with structured fields only", "feedback", "Use structured fields; retries are counted by the metrics library."))
	if out := scan(); !strings.Contains(out, "1 updated") || strings.Contains(out, "conflict") {
		t.Fatalf("an edit after an accepted merge:\n%s", out)
	}
	if rule := show(ruleID); !strings.Contains(rule, "revision: 3") || !strings.Contains(rule, "metrics library") {
		t.Fatalf("entry after an accepted merge and a new edit:\n%s", rule)
	}
	// The same holds when a tracked memory turns private.
	write("feedback_logging.md", topic("feedback-logging", "Log with structured fields only", "user", "The user prefers structured logs."))
	if out := scan(); !strings.Contains(out, "can no longer be imported as one note (is now a user memory)") {
		t.Fatalf("a tracked source that became a user memory:\n%s", out)
	}

	// A tracked memory changes type: its entry cannot change type, so no
	// later edit may be applied to it, with or without an accepted conflict.
	write("reference_runbook.md", topic("reference-runbook", "Incident runbook location", "feedback", "Always open the runbook before paging anyone."))
	if out := scan(); !strings.Contains(out, "conflict: reference_runbook.md: the memory is now a feedback, but its entry is a fact; "+runbookID+" was left as it is") {
		t.Fatalf("type change:\n%s", out)
	}
	write("reference_runbook.md", topic("reference-runbook", "Incident runbook location", "feedback", "Always open the runbook before paging anyone, even at night."))
	if out := scan(); !strings.Contains(out, "the memory is now a feedback, but its entry is a fact") || strings.Contains(out, "1 updated") {
		t.Fatalf("a second edit must not slip past the type conflict:\n%s", out)
	}
	scan("--accept-conflicts")
	write("reference_runbook.md", topic("reference-runbook", "Incident runbook location", "feedback", "Always open the runbook first."))
	if out := scan(); !strings.Contains(out, "the memory is now a feedback, but its entry is a fact") || strings.Contains(out, "1 updated") {
		t.Fatalf("accepting must not let a later edit change a fact into a rule's text:\n%s", out)
	}
	if rb := show(runbookID); !strings.Contains(rb, "pinned page in the operations space") || !strings.Contains(rb, "revision: 1") || !strings.Contains(rb, "type: fact") {
		t.Fatalf("the fact was overwritten by a feedback memory:\n%s", rb)
	}
	write("reference_runbook.md", topic("reference-runbook", "Incident runbook location", "reference", "The incident runbook is the pinned page in the operations space."))
	if out := scan(); strings.Contains(out, "conflict: reference_runbook.md") {
		t.Fatalf("back to its own type:\n%s", out)
	}
	// The way out the conflict advertises: deprecate the entry, and the
	// memory imports as a new entry of its new type, without another edit.
	write("project_beta_plan.md", topic("beta-plan", "Beta plan is reviewed monthly", "project", "The beta plan is reviewed on the first Monday of each month."))
	if out := scan(); !strings.Contains(out, "1 imported") {
		t.Fatalf("new memory:\n%s", out)
	}
	const planFact = "aiproxy/facts/beta-plan-is-reviewed-monthly"
	write("project_beta_plan.md", topic("beta-plan", "Beta plan is reviewed monthly", "feedback", "Review the beta plan before changing its scope."))
	if out := scan(); !strings.Contains(out, "the memory is now a feedback, but its entry is a fact; "+planFact) {
		t.Fatalf("type change of the new memory:\n%s", out)
	}
	os.Chdir(repo)
	must(t, "codex", "deprecate", planFact, "-m", "it is a working rule now, not a fact")
	out = scan()
	if strings.Contains(out, "conflict: project_beta_plan.md") || !strings.Contains(out, "1 imported") {
		t.Fatalf("deprecating the entry must settle the conflict and let the memory import:\n%s", out)
	}
	if r := show("aiproxy/conventions/beta-plan-is-reviewed-monthly"); !strings.Contains(r, "Review the beta plan before changing its scope") || !strings.Contains(r, "trust: verified") {
		t.Fatalf("the memory's new entry:\n%s", r)
	}
	if out := scan(); strings.Contains(out, "conflict: project_beta_plan.md") || strings.Contains(out, "1 imported") {
		t.Fatalf("rescan after the takeover:\n%s", out)
	}
	// A retired entry whose source did not conflict is not resurrected.
	os.Chdir(repo)
	must(t, "codex", "deprecate", "aiproxy/conventions/beta-plan-is-reviewed-monthly", "-m", "dropped")
	if out := scan(); strings.Contains(out, "1 imported") {
		t.Fatalf("an unchanged source must not bring back an entry the user retired:\n%s", out)
	}

	// 2. Two sources share a name. One is renamed (same content), the other
	// deleted: the new path stands for the renamed one only.
	os.Rename(filepath.Join(mem, "project_alpha.md"), filepath.Join(mem, "project_alpha_rollout.md"))
	os.Remove(filepath.Join(mem, "project_beta.md"))
	out = scan()
	if !strings.Contains(out, "1 renamed") || !strings.Contains(out, "1 sources deleted") || !strings.Contains(out, "source deleted: project_beta.md no longer backs "+betaID) || strings.Contains(out, "no longer backs "+alphaID) {
		t.Fatalf("rename next to a deletion:\n%s", out)
	}
	// The entry follows its source to the new file name, with the same
	// content, revision and deadline.
	// (The confirmation recorded at import keeps naming the file as it was
	// then: verification events are history and are never rewritten.)
	if a := show(alphaID); !strings.Contains(a, "source [^memory-claude-code]: ~/.claude/projects/-x-aiproxy/memory/project_alpha_rollout.md by") || strings.Contains(a, "source [^memory-claude-code]: ~/.claude/projects/-x-aiproxy/memory/project_alpha.md") || !strings.Contains(a, "revision: 1") || !strings.Contains(a, "trust: verified") {
		t.Fatalf("a renamed source must be named by its entry:\n%s", a)
	}
	write("project_alpha_rollout.md", topic("shared-name", "Alpha rollout is gated on the canary", "project", "The alpha rollout waits for the canary to pass for two days."))
	if out := scan(); !strings.Contains(out, "1 updated") {
		t.Fatalf("edit after the rename:\n%s", out)
	}
	if a := show(alphaID); !strings.Contains(a, "two days") || !strings.Contains(a, "revision: 2") {
		t.Fatalf("the renamed source lost its entry:\n%s", a)
	}
	if b := show(betaID); !strings.Contains(b, "every Tuesday") || !strings.Contains(b, "revision: 1") {
		t.Fatalf("the deleted source's entry was overwritten:\n%s", b)
	}

	// 3. A deleted source keeps being reported until its entry is checked
	// again; the check the report recommends settles it. An edit made by
	// hand before the deletion does not settle anything.
	os.Chdir(repo)
	must(t, "codex", "update", runbookID, "-d", "Where the incident runbook lives", "-m", "clearer wording")
	os.Remove(filepath.Join(mem, "reference_runbook.md"))
	if out := scan(); !strings.Contains(out, "2 sources deleted") || !strings.Contains(out, "no longer backs "+runbookID) {
		t.Fatalf("second deletion:\n%s", out)
	}
	if out := scan(); !strings.Contains(out, "2 sources deleted") || !strings.Contains(out, "no longer backs "+runbookID) {
		t.Fatalf("a warning must not clear itself because the entry was edited before the deletion:\n%s", out)
	}
	os.Chdir(repo)
	must(t, "codex", "verify", runbookID, "--confirm", "-e", "the pinned page is still there")
	out = scan()
	if strings.Contains(out, runbookID) || !strings.Contains(out, "1 sources deleted") || !strings.Contains(out, betaID) {
		t.Fatalf("a confirmation must settle its deleted-source warning, and only its own:\n%s", out)
	}
	if out := scan(); strings.Contains(out, runbookID) {
		t.Fatalf("settled warning came back:\n%s", out)
	}
	// A vanished source whose name is carried by several new files with
	// different content: which one continues it is unknown, so none is
	// imported and nothing is reported deleted.
	write("feedback_reviews.md", topic("feedback-reviews", "Review requests name one owner", "feedback", "Every review request names exactly one owner."))
	if out := scan(); !strings.Contains(out, "1 imported") {
		t.Fatalf("new feedback:\n%s", out)
	}
	const reviewsID = "aiproxy/conventions/review-requests-name-one-owner"
	os.Remove(filepath.Join(mem, "feedback_reviews.md"))
	write("feedback_reviews_v2.md", topic("feedback-reviews", "Review requests name one owner and one backup", "feedback", "Every review request names one owner and one backup."))
	write("feedback_reviews_backup.md", topic("feedback-reviews", "Reviews are requested in the team channel", "feedback", "Ask for reviews in the team channel, not in private messages."))
	out = scan()
	if !strings.Contains(out, `ambiguous: 2 new file(s) carry the name "feedback-reviews" of 1 deleted source(s) (`+reviewsID+` among them)`) || strings.Contains(out, "1 imported") || strings.Contains(out, "2 imported") || strings.Contains(out, "no longer backs "+reviewsID) {
		t.Fatalf("ambiguous name match must be withheld:\n%s", out)
	}
	os.Chdir(base)
	if out := must(t, "codex", "search", "review requests owner backup team channel", "--all"); strings.Contains(out, "one-owner-and-one-backup") || strings.Contains(out, "team-channel") {
		t.Fatalf("an ambiguous candidate was imported:\n%s", out)
	}
	// Resolved by keeping one file with that name.
	os.Remove(filepath.Join(mem, "feedback_reviews_backup.md"))
	if out := scan(); !strings.Contains(out, "1 renamed") || !strings.Contains(out, "1 updated") || strings.Contains(out, "ambiguous") {
		t.Fatalf("after removing the backup:\n%s", out)
	}
	if r := show(reviewsID); !strings.Contains(r, "one owner and one backup") || !strings.Contains(r, "revision: 2") {
		t.Fatalf("the surviving file should continue the entry:\n%s", r)
	}

	// Two tracked sources share a name. The one that sorts first is
	// deleted, the other is renamed without an edit: its exact content
	// settles it, whatever the order the sources are looked at in.
	write("project_a_first.md", topic("pair", "First of a pair is about caching", "project", "Responses are cached for five minutes."))
	write("project_b_second.md", topic("pair", "Second of a pair is about paging", "project", "List endpoints page by cursor."))
	if out := scan(); !strings.Contains(out, "2 imported") {
		t.Fatalf("pair:\n%s", out)
	}
	const firstID, secondID = "aiproxy/facts/first-of-a-pair-is-about-caching", "aiproxy/facts/second-of-a-pair-is-about-paging"
	os.Remove(filepath.Join(mem, "project_a_first.md"))
	os.Rename(filepath.Join(mem, "project_b_second.md"), filepath.Join(mem, "project_paging.md"))
	out = scan()
	if strings.Contains(out, "ambiguous") || !strings.Contains(out, "1 renamed") || !strings.Contains(out, "no longer backs "+firstID) || strings.Contains(out, "no longer backs "+secondID) {
		t.Fatalf("an exact-content rename must be settled before names are considered:\n%s", out)
	}
	if sec := show(secondID); !strings.Contains(sec, "memory/project_paging.md by") {
		t.Fatalf("the renamed source:\n%s", sec)
	}
	os.Chdir(repo)
	must(t, "codex", "deprecate", firstID, "-m", "caching was removed")

	// The way out an ambiguous rename advertises: deprecate the old entry,
	// and the files carrying its name are imported as new sources.
	write("feedback_naming.md", topic("feedback-naming", "Branch names start with the ticket id", "feedback", "Name branches after their ticket."))
	if out := scan(); !strings.Contains(out, "1 imported") {
		t.Fatalf("naming rule:\n%s", out)
	}
	const namingID = "aiproxy/conventions/branch-names-start-with-the-ticket-id"
	os.Remove(filepath.Join(mem, "feedback_naming.md"))
	write("feedback_naming_git.md", topic("feedback-naming", "Branches are named ticket-slug", "feedback", "Use the ticket id and a short slug for branch names."))
	write("feedback_naming_files.md", topic("feedback-naming", "Test files sit next to their source", "feedback", "Keep a test file beside the file it tests."))
	if out := scan(); !strings.Contains(out, `ambiguous: 2 new file(s) carry the name "feedback-naming"`) || strings.Contains(out, "imported,") && !strings.Contains(out, "0 imported") {
		t.Fatalf("ambiguous replacements:\n%s", out)
	}
	os.Chdir(repo)
	must(t, "codex", "deprecate", namingID, "-m", "split into two rules")
	out = scan()
	if strings.Contains(out, "ambiguous") || !strings.Contains(out, "2 imported") || strings.Contains(out, "no longer backs "+namingID) {
		t.Fatalf("after deprecating the old entry the replacements must import:\n%s", out)
	}
	if out := scan(); strings.Contains(out, "ambiguous") || !strings.Contains(out, "0 imported") {
		t.Fatalf("rescan after the split:\n%s", out)
	}

	// Two tracked sources share a name, both vanish, and one edited file
	// with that name appears: which source it continues is unknown, so
	// neither is reported deleted and nothing is imported.
	write("project_gamma.md", topic("twin", "Gamma cluster runs the batch jobs", "project", "Batch jobs run on the gamma cluster."))
	write("project_delta.md", topic("twin", "Delta cluster runs the web tier", "project", "The web tier runs on the delta cluster."))
	if out := scan(); !strings.Contains(out, "2 imported") {
		t.Fatalf("twins:\n%s", out)
	}
	const gammaID, deltaID = "aiproxy/facts/gamma-cluster-runs-the-batch-jobs", "aiproxy/facts/delta-cluster-runs-the-web-tier"
	os.Remove(filepath.Join(mem, "project_gamma.md"))
	os.Remove(filepath.Join(mem, "project_delta.md"))
	write("project_clusters.md", topic("twin", "Clusters and what runs on them", "project", "Gamma runs batch, delta runs web, epsilon runs nothing yet."))
	out = scan()
	if !strings.Contains(out, `ambiguous: 1 new file(s) carry the name "twin" of 2 deleted source(s)`) || strings.Contains(out, "no longer backs "+gammaID) || strings.Contains(out, "no longer backs "+deltaID) || strings.Contains(out, "1 imported") || strings.Count(out, "ambiguous:") != 1 {
		t.Fatalf("a whole ambiguous name group must be withheld:\n%s", out)
	}
	os.Remove(filepath.Join(mem, "project_clusters.md")) // now both are simply gone
	if out := scan(); !strings.Contains(out, "no longer backs "+gammaID) || !strings.Contains(out, "no longer backs "+deltaID) {
		t.Fatalf("once the candidate is gone, both deletions are reported:\n%s", out)
	}

	// --force reconsiders an unchanged source that owns no entry. Here the
	// memory duplicated an entry that has since been deprecated.
	os.Chdir(repo)
	must(t, "codex", "add", "fact", "Staging database is refreshed nightly", "-d", "Copied from production every night at two.", "-e", "read the cron config")
	write("project_staging.md", topic("staging", "Staging database is refreshed nightly", "project", "Copied from production every night at two."))
	if out := scan(); !strings.Contains(out, "1 already known") {
		t.Fatalf("duplicate memory:\n%s", out)
	}
	os.Chdir(repo)
	must(t, "codex", "deprecate", "aiproxy/facts/staging-database-is-refreshed-nightly", "-m", "superseded by the memory import")
	if out := scan(); strings.Contains(out, "1 imported") {
		t.Fatalf("an unforced scan leaves an unchanged source alone:\n%s", out)
	}
	if out := scan("--force"); !strings.Contains(out, "1 imported") {
		t.Fatalf("--force must reconsider a source that owns no entry:\n%s", out)
	}
	// Forcing never rewrites or duplicates a source that owns an entry.
	os.Chdir(base)
	active := must(t, "codex", "status", "--all")
	if out := scan("--force"); strings.Contains(out, "1 imported") || strings.Contains(out, "1 updated") {
		t.Fatalf("--force touched sources that own entries:\n%s", out)
	}
	if again := must(t, "codex", "status", "--all"); strings.SplitN(again, "\n", 3)[1] != strings.SplitN(active, "\n", 3)[1] {
		t.Fatalf("--force changed the entries:\n%s\n%s", active, again)
	}

	// A whole memory directory that was imported and is then removed: its
	// sources are reconciled like deleted files, but only when the
	// directory is known to be gone.
	pdir2 := filepath.Join(home, ".claude", "projects", "-x-aiproxy-wt")
	mem2 := filepath.Join(pdir2, "memory")
	os.MkdirAll(mem2, 0o755)
	os.WriteFile(filepath.Join(pdir2, "t.jsonl"), []byte(`{"type":"user","cwd":"`+repo+`","sessionId":"s2","message":{"role":"user","content":"hi"}}`+"\n"), 0o644)
	os.WriteFile(filepath.Join(mem2, "feedback_worktree.md"), []byte(topic("feedback-worktree", "Worktrees are removed after merge", "feedback", "Delete a worktree once its branch is merged.")), 0o644)
	if out := scan(); !strings.Contains(out, "-x-aiproxy-wt/memory [aiproxy]: 1 imported") {
		t.Fatalf("second directory:\n%s", out)
	}
	const wtID = "aiproxy/conventions/worktrees-are-removed-after-merge"
	if os.Geteuid() != 0 {
		os.Chmod(pdir2, 0o000) // unreachable is not deleted
		if out := scan(); strings.Contains(out, "no longer backs "+wtID) || !strings.Contains(out, "-x-aiproxy-wt/memory: imported before, and not checked now") {
			t.Fatalf("an unreadable directory must not count as deleted:\n%s", out)
		}
		os.Chmod(pdir2, 0o755)
	}
	os.RemoveAll(mem2)
	out = scan()
	if !strings.Contains(out, "-x-aiproxy-wt/memory [aiproxy]: 0 imported, 0 updated, 0 unchanged, 1 sources deleted") || !strings.Contains(out, "no longer backs "+wtID) || !strings.Contains(out, "the directory itself no longer exists") {
		t.Fatalf("a removed directory must be reconciled:\n%s", out)
	}
	if r := show(wtID); !strings.Contains(r, "trust: verified") {
		t.Fatalf("the entry of a removed directory must be kept:\n%s", r)
	}
	os.Chdir(repo)
	must(t, "codex", "deprecate", wtID, "-m", "no longer how we work")
	if out := scan(); strings.Contains(out, "-x-aiproxy-wt") {
		t.Fatalf("a reconciled directory needs no further report:\n%s", out)
	}

	// A dry run reports the same rename without recording it.
	os.Rename(filepath.Join(mem, "project_alpha_rollout.md"), filepath.Join(mem, "project_alpha_gate.md"))
	if out := scan("--dry-run"); !strings.Contains(out, "1 renamed") || strings.Contains(out, "1 would be imported") || strings.Contains(out, "no longer backs "+alphaID) {
		t.Fatalf("dry run of a rename:\n%s", out)
	}
	if out := scan(); !strings.Contains(out, "1 renamed") {
		t.Fatalf("the dry run recorded the rename:\n%s", out)
	}
}

// Rule files are imported the way their own agent loads them: a path-scoped
// rule stays conditional, a condition kb cannot reproduce is not imported,
// imports stay inside the repository and comments are dropped.
func TestImportConditionalRuleFiles(t *testing.T) {
	base, home := hubEnv(t)
	repo := mkRemoteRepo(t, base, "aiproxy")
	must(t, "", "hub", "init")
	file := func(rel, content string) {
		t.Helper()
		p := filepath.Join(repo, filepath.FromSlash(rel))
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	file(".claude/rules/general.md", "Keep functions under fifty lines where it reads well.\n")
	file(".claude/rules/api.md", "---\npaths:\n  - \"src/api/**/*.ts\"\n---\n\n# API rules\n\n- Validate every API handler input with the shared schema helpers.\n")
	file(".claude/rules/frontend/styles.md", "---\npaths: \"src/**/*.{ts,tsx}, /*.md\"\n---\nUse design tokens for colours, never literals.\n")
	file(".claude/rules/broken.md", "---\npaths: [src/**\n  oops: : :\n---\nThis rule has frontmatter that does not parse.\n")
	file(".claude/rules/badglob.md", "---\npaths:\n  - \"photos [2024/**\"\n---\nThis rule has a pattern that cannot be matched exactly.\n")
	file(".cursor/rules/attached.mdc", "---\ndescription: only when attached\nglobs: src/**/*.go\n---\nPrefer table-driven tests in Go files.\n")
	file("docs/extra.md", "Run the linter before every commit.\n\n@../CLAUDE.md\n")
	file("AGENTS.md", "Agents file content that Codex reads natively.\n")
	file("CLAUDE.md", "# Project\n\n<!-- maintainer note: do not\n   ship this comment -->\nAlways write a changelog entry.\n\nSee @docs/extra.md and @AGENTS.md, and @types/node is a package, not a file.\n\n```html\n<!-- kept: this comment is inside a code block -->\n```\n\nSecrets: @~/outside.md\n")
	file("CLAUDE.local.md", "My sandbox URL is private.\n")
	file("packages/web/CLAUDE.md", "Web components use CSS modules.\n")
	os.WriteFile(filepath.Join(home, "outside.md"), []byte("Content outside the repository that must not be imported.\n"), 0o644)
	os.WriteFile(filepath.Join(home, "shared-rule.md"), []byte("A rule that lives outside the repository.\n"), 0o644)
	os.Symlink(filepath.Join(home, "shared-rule.md"), filepath.Join(repo, ".claude", "rules", "linked.md"))
	git(t, repo, "add", "-A")
	git(t, repo, "commit", "-qm", "rules")

	// A model stand-in that names one rule after the file it was given and
	// keeps each prompt, so what was sent can be checked per file.
	prompts := filepath.Join(base, "prompts")
	os.MkdirAll(prompts, 0o755)
	script := filepath.Join(base, "model.sh")
	os.WriteFile(script, []byte("#!/bin/sh\nP=$(cat)\nF=$(printf '%s\\n' \"$P\" | sed -n 's/^File \\(.*\\):$/\\1/p' | tail -1)\nN=$(basename \"$(dirname \"$F\")\")-$(basename \"$F\")\nprintf '%s' \"$P\" > '"+prompts+"'/\"$N\".txt\nprintf '[{\"title\":\"Rule from %s\",\"description\":\"imported for the test\"}]' \"$N\"\n"), 0o755)
	os.Chdir(repo)
	out := must(t, "", "rules", "import", "--repo", "--cmd", script)
	for _, want := range []string{
		".claude/rules/broken.md: not imported (its frontmatter does not parse",
		".claude/rules/badglob.md: not imported (pattern \"photos [2024/**\"",
		".cursor/rules/attached.mdc: not imported (a Cursor rule that is not `alwaysApply: true`",
		"note: CLAUDE.local.md: not imported (personal instructions",
		".claude/rules/linked.md: not imported (a link to a location outside",
		"CLAUDE.md: @AGENTS.md was not followed (other agents read AGENTS.md natively); @~/outside.md was not followed (it is outside",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("import report lacks %q:\n%s", want, out)
		}
	}
	for _, not := range []string{"Rule from rules-broken.md", "Rule from rules-badglob.md", "Rule from rules-attached.mdc", "Rule from rules-linked.md", "CLAUDE.local.md: added"} {
		if strings.Contains(out, not) {
			t.Errorf("import report must not contain %q:\n%s", not, out)
		}
	}
	read := func(name string) string {
		data, err := os.ReadFile(filepath.Join(prompts, name+".txt"))
		if err != nil {
			t.Fatalf("no prompt for %s (%v) in:\n%s", name, err, out)
		}
		return string(data)
	}
	// What the model saw for CLAUDE.md: its text, the import inside the
	// repository (once, despite the cycle), no comment, nothing from outside.
	p := read("aiproxy-CLAUDE.md")
	for _, want := range []string{"Always write a changelog entry.", "Run the linter before every commit.", "<!-- kept: this comment is inside a code block -->"} {
		if !strings.Contains(p, want) {
			t.Errorf("CLAUDE.md prompt lacks %q:\n%s", want, p)
		}
	}
	for _, not := range []string{"maintainer note", "must not be imported", "Codex reads natively", "sandbox URL"} {
		if strings.Contains(p, not) {
			t.Errorf("CLAUDE.md prompt must not contain %q:\n%s", not, p)
		}
	}
	if strings.Count(p, "Always write a changelog entry.") != 1 {
		t.Errorf("an import cycle included the file twice:\n%s", p)
	}
	if api := read("rules-api.md"); strings.Contains(api, "paths:") || !strings.Contains(api, "Validate every API handler input") {
		t.Errorf("frontmatter must not reach the model:\n%s", api)
	}

	// Always-on delivery has the unconditional rules only.
	rules := must(t, "codex", "rules", "-q")
	for _, want := range []string{"Rule from rules-general.md", "Rule from aiproxy-CLAUDE.md", "(+3 rule(s) apply only to particular files"} {
		if !strings.Contains(rules, want) {
			t.Errorf("kb rules lacks %q:\n%s", want, rules)
		}
	}
	hook, _, _ := kbRun(t, "claude-code", `{"session_id":"s","hook_event_name":"SessionStart","cwd":"`+repo+`"}`, "hook", "claude")
	for _, text := range []string{rules, hook} {
		for _, not := range []string{"Rule from rules-api.md", "Rule from frontend-styles.md", "Rule from web-CLAUDE.md"} {
			if strings.Contains(text, not) {
				t.Errorf("a conditional rule was delivered as always-on (%q):\n%s", not, text)
			}
		}
	}
	// A conditional rule appears only for a matching file.
	forFile := func(path string) string { t.Helper(); return must(t, "codex", "rules", "-q", "-f", path) }
	for path, want := range map[string]string{
		"src/api/users/create.ts": "Rule from rules-api.md",
		"src/ui/Button.tsx":       "Rule from frontend-styles.md",
		"README.md":               "Rule from frontend-styles.md",
		"packages/web/src/a.css":  "Rule from web-CLAUDE.md",
	} {
		if got := forFile(path); !strings.Contains(got, want) {
			t.Errorf("kb rules -f %s lacks %q:\n%s", path, want, got)
		}
	}
	for path, not := range map[string]string{
		"src/api/users/create.go": "Rule from rules-api.md",       // wrong extension
		"docs/guide.md":           "Rule from frontend-styles.md", // /*.md is rooted
		"packages/api/src/a.css":  "Rule from web-CLAUDE.md",      // another package
		"cmd/main.go":             "Rule from",
	} {
		if got := forFile(path); strings.Contains(got, not) {
			t.Errorf("kb rules -f %s must not show %q:\n%s", path, not, got)
		}
	}
	if out := must(t, "codex", "search", "-f", "src/api/users/create.ts"); !strings.Contains(out, "rule for this file: Rule from rules-api.md") {
		t.Errorf("search by file:\n%s", out)
	}
	show := must(t, "codex", "show", "aiproxy/conventions/rule-from-web-claude-md")
	if !strings.Contains(show, "conditional rule: applies only when working on ** under packages/web/") {
		t.Errorf("nested CLAUDE.md should be conditional on its directory:\n%s", show)
	}
	var st struct {
		Rules       int `json:"rules_delivered"`
		Conditional int `json:"rules_conditional"`
	}
	json.Unmarshal([]byte(must(t, "codex", "status", "--json")), &st)
	if st.Rules != 2 || st.Conditional != 3 {
		t.Errorf("status: %+v", st)
	}
	// A rule can be recorded as conditional by hand too.
	must(t, "claude-code", "add", "rule", "Migrations are forward-only", "-d", "no down migrations", "-e", "user said so", "--when", "db/migrations/**")
	if got := forFile("db/migrations/0001_init.sql"); !strings.Contains(got, "Migrations are forward-only") {
		t.Errorf("--when rule:\n%s", got)
	}
	if got := must(t, "codex", "rules", "-q"); strings.Contains(got, "Migrations are forward-only") {
		t.Errorf("--when rule delivered as always-on:\n%s", got)
	}
	// A path-scoped file named on the command line has no known base.
	if out := must(t, "", "rules", "import", filepath.Join(repo, ".claude", "rules", "api.md"), "--cmd", script); !strings.Contains(out, "its base directory is unknown here") {
		t.Errorf("explicit path-scoped file:\n%s", out)
	}
}

// Item 3: global rules reach Claude Code through a managed rules file, the
// hooks deliver the rest to the main agent and everything to subagents,
// capture mode is a recorded choice, and what each agent received is visible.
func TestNativeRulesAndSubagentDelivery(t *testing.T) {
	base, home, aiproxy, kubedl := populatedHub(t)
	const (
		pnpm, volcano, english, sha = "Use pnpm, never npm", "Submit jobs through volcano queues", "Write commit messages in English", "Tag images with the git sha"
	)
	native := filepath.Join(home, ".claude", "rules", "kb.md")
	settings := filepath.Join(home, ".claude", "settings.json")
	readNative := func() string { data, _ := os.ReadFile(native); return string(data) }
	hub, _ := filepath.EvalSymlinks(os.Getenv("KB_HOME")) // the marker names the hub by its resolved path
	run := func(cwd, input string) string {
		t.Helper()
		os.Chdir(cwd)
		out, _, code := kbRun(t, "claude-code", input, "hook", "claude")
		if code != 0 {
			t.Fatalf("hook exit %d", code)
		}
		return out
	}
	start := func(cwd, session, source string) string {
		return run(cwd, `{"session_id":"`+session+`","hook_event_name":"SessionStart","source":"`+source+`","cwd":"`+cwd+`"}`)
	}
	context := func(out string) string {
		t.Helper()
		if strings.TrimSpace(out) == "" {
			return ""
		}
		var m struct {
			Out struct {
				Event   string `json:"hookEventName"`
				Context string `json:"additionalContext"`
			} `json:"hookSpecificOutput"`
		}
		if err := json.Unmarshal([]byte(out), &m); err != nil {
			t.Fatalf("hook output is not the JSON a context-only event needs: %v\n%s", err, out)
		}
		return m.Out.Event + "|" + m.Out.Context
	}
	subagent := func(cwd, session, id, typ string) string {
		return context(run(cwd, `{"session_id":"`+session+`","hook_event_name":"SubagentStart","agent_id":"`+id+`","agent_type":"`+typ+`","cwd":"`+cwd+`"}`))
	}
	has := func(name, text string, want, not []string) {
		t.Helper()
		for _, w := range want {
			if !strings.Contains(text, w) {
				t.Errorf("%s: missing %q\n%s", name, w, text)
			}
		}
		for _, n := range not {
			if strings.Contains(text, n) {
				t.Errorf("%s: must not contain %q\n%s", name, n, text)
			}
		}
	}

	// The managed file: only global rules that apply everywhere, between
	// kb's markers, owned by this hub.
	has("managed rules file", readNative(), []string{english, "<!-- kb:begin", "<!-- kb:source " + hub, "<!-- kb:end -->"}, []string{pnpm, volcano, sha, "table-driven"})

	// Main agent, every way a session starts: the hook leaves the universal
	// rule to the file and delivers the project's rules and the global rules
	// restricted to it. Nothing is suppressed because a session was seen.
	for _, source := range []string{"startup", "resume", "clear", "compact", "fork"} {
		has("main in kubedl ("+source+")", start(kubedl, "main-k", source), []string{volcano, sha}, []string{english, pnpm})
		has("main in aiproxy ("+source+")", start(aiproxy, "main-a", source), []string{pnpm}, []string{english, volcano, sha})
		if out := start(base, "main-n", source); strings.Contains(out, pnpm) || strings.Contains(out, volcano) || strings.Contains(out, sha) || strings.Contains(out, english) {
			t.Errorf("main with no project (%s) should get everything from the file and nothing from the hook:\n%s", source, out)
		}
	}
	// Subagents get the complete applicable set as context, whatever their
	// type and whatever the main agent received: Explore and Plan load no
	// CLAUDE.md or rules files at all.
	for _, typ := range []string{"code-reviewer", "Explore", "Plan", "general-purpose"} {
		has("subagent "+typ+" in kubedl", subagent(kubedl, "main-k", "agent-"+typ, typ), []string{"SubagentStart|", volcano, sha, english}, []string{pnpm, "table-driven"})
		has("subagent "+typ+" in aiproxy", subagent(aiproxy, "main-a", "agent-"+typ, typ), []string{"SubagentStart|", pnpm, english}, []string{volcano, sha})
		has("subagent "+typ+" with no project", subagent(base, "main-n", "agent-"+typ, typ), []string{"SubagentStart|", english}, []string{pnpm, volcano, sha})
	}
	// A subagent's own start after it compacts (it carries an agent id).
	has("subagent after its compaction", run(kubedl, `{"session_id":"main-k","hook_event_name":"SessionStart","source":"compact","agent_id":"agent-Explore","agent_type":"Explore","cwd":"`+kubedl+`"}`), []string{volcano, sha, english}, []string{pnpm})

	// A rule change rewrites the file; a restricted or project rule never enters it.
	os.Chdir(aiproxy)
	must(t, "claude-code", "add", "rule", "Never force-push shared branches", "-d", "history is shared", "-e", "user said so", "--global")
	must(t, "claude-code", "add", "rule", "Use the kubedl staging queue for tests", "-d", "prod queue is metered", "-e", "user said so", "--global", "--for", "kubedl")
	has("managed rules file after a change", readNative(), []string{english, "Never force-push shared branches"}, []string{"staging queue", pnpm})
	has("main in kubedl after a change", start(kubedl, "main-k", "resume"), []string{volcano, sha, "staging queue"}, []string{"force-push", english})
	// The file is never read back in as a rule file.
	script, _ := fakeModel(t, base, "norules", `[]`)
	if out := must(t, "", "rules", "import", "--user", "--cmd", script); strings.Contains(out, "rules/kb.md") {
		t.Errorf("kb's own rules file was offered for import:\n%s", out)
	}

	// What each agent last received is visible, per agent: the subagents
	// that started before the change still hold the old rules.
	os.Chdir(kubedl)
	diag := must(t, "claude-code", "rules", "--delivery")
	has("delivery diagnostics", diag, []string{
		"managed rules file: " + native + " is current",
		"claude installation: capture full; rule refresh hook off",
		"main-k [kubedl]", "main: current; via session-start on SessionStart:resume, universal rules from the managed file",
		"agent-Explore (Explore): STALE: it has ", "refreshed at its next start or compaction",
		"subagents: SubagentStart gives each one the complete rule set",
	}, []string{"before its next tool call"})

	// Fallback: when the file is missing or stale, the hook delivers the
	// universal rules itself and says so in the diagnostics.
	os.Remove(native)
	has("main in kubedl without the file", start(kubedl, "main-k", "resume"), []string{volcano, sha, english, "force-push"}, []string{pnpm})
	has("diagnostics without the file", must(t, "claude-code", "rules", "--delivery"), []string{"is missing or out of date, so SessionStart delivers every rule itself (fallback)"}, nil)
	os.WriteFile(native, []byte("<!-- kb:begin (managed by 'kb install'; re-run it to update, edits here are overwritten) -->\nan older copy\n<!-- kb:source "+hub+" -->\n<!-- kb:end -->\n"), 0o644)
	has("main in kubedl with a stale file", start(kubedl, "main-k", "resume"), []string{english}, nil)
	must(t, "", "install", "claude")
	has("repaired file", readNative(), []string{english, "force-push"}, []string{"older copy"})
	has("main in kubedl after repair", start(kubedl, "main-k", "resume"), []string{volcano}, []string{english})
	// A rules file someone else wrote is not overwritten; the hook covers.
	os.WriteFile(native, []byte("My own notes in a file named kb.md.\n"), 0o644)
	if out := must(t, "", "install", "claude"); !strings.Contains(out, "was not written by this hub: left as it is") {
		t.Errorf("foreign rules file:\n%s", out)
	}
	if got := readNative(); got != "My own notes in a file named kb.md.\n" {
		t.Errorf("a file kb did not write was changed:\n%s", got)
	}
	has("main in kubedl beside a foreign file", start(kubedl, "main-k", "resume"), []string{english, volcano}, nil)
	os.Remove(native)
	// Turning the file off is remembered.
	must(t, "", "install", "claude", "--native-rules", "off")
	must(t, "", "install", "claude")
	if _, err := os.Stat(native); err == nil {
		t.Error("the managed rules file came back after it was turned off")
	}
	has("main in kubedl with the file turned off", start(kubedl, "main-k", "startup"), []string{english, volcano, sha}, nil)
	must(t, "", "install", "claude", "--native-rules", "on")
	has("file turned on again", readNative(), []string{english}, []string{volcano})

	// Capture mode is a recorded choice, separate from rule delivery.
	hooksIn := func() map[string]any {
		var m map[string]any
		data, _ := os.ReadFile(settings)
		json.Unmarshal(data, &m)
		h, _ := m["hooks"].(map[string]any)
		return h
	}
	for _, ev := range []string{"SessionStart", "SubagentStart", "PreCompact", "SessionEnd"} {
		if hooksIn()[ev] == nil {
			t.Fatalf("full install lacks %s", ev)
		}
	}
	out := must(t, "", "install", "claude", "--capture", "manual")
	if !strings.Contains(out, "Capture mode: manual (recorded for this machine)") {
		t.Errorf("manual install notes:\n%s", out)
	}
	for i := 0; i < 2; i++ { // a later bare install, twice: still manual
		if h := hooksIn(); h["PreCompact"] != nil || h["SessionEnd"] != nil || h["SessionStart"] == nil || h["SubagentStart"] == nil {
			t.Fatalf("manual capture must keep delivery hooks and drop capture hooks: %v", h)
		}
		out = must(t, "", "install", "claude")
		if !strings.Contains(out, "Capture mode: manual (recorded for this machine)") {
			t.Errorf("a bare install forgot the recorded mode:\n%s", out)
		}
	}
	has("delivery still works in manual mode", start(kubedl, "main-k", "startup"), []string{volcano, sha}, nil)
	must(t, "", "install", "claude", "--capture", "full")
	if h := hooksIn(); h["PreCompact"] == nil || h["SessionEnd"] == nil {
		t.Fatalf("back to full capture: %v", h)
	}
	if st := strings.TrimSpace(git(t, os.Getenv("KB_HOME"), "status", "--porcelain")); st != "" {
		t.Errorf("installation choices leaked into the shared hub:\n%s", st)
	}

	// Rule refresh (opt-in): an agent whose rules changed since it received
	// them gets the current set before its next tool call, once.
	tool := func(session, id string) string {
		in := `{"session_id":"` + session + `","hook_event_name":"PreToolUse","tool_name":"Read","cwd":"` + kubedl + `"`
		if id != "" {
			in += `,"agent_id":"` + id + `","agent_type":"Explore"`
		}
		return context(run(kubedl, in+"}"))
	}
	if h := hooksIn(); h["PreToolUse"] != nil {
		t.Fatalf("the refresh hook must be opt-in: %v", h)
	}
	must(t, "", "install", "claude", "--rule-refresh", "on")
	if h := hooksIn(); h["PreToolUse"] == nil {
		t.Fatal("refresh hook not installed")
	}
	start(kubedl, "refresh", "startup")
	subagent(kubedl, "refresh", "agent-r", "Explore")
	if out := tool("refresh", "") + tool("refresh", "agent-r"); out != "" {
		t.Errorf("nothing changed: the refresh hook must stay silent:\n%s", out)
	}
	os.Chdir(kubedl)
	must(t, "claude-code", "add", "rule", "Label every job with its team", "-d", "cost reports need it", "-e", "user said so")
	has("refresh for the subagent", tool("refresh", "agent-r"), []string{"PreToolUse|kb: the rules that apply here changed", "Label every job with its team", volcano, english}, []string{pnpm})
	has("refresh for the main agent", tool("refresh", ""), []string{"Label every job with its team"}, nil)
	if out := tool("refresh", "") + tool("refresh", "agent-r"); out != "" {
		t.Errorf("an agent is refreshed once per change:\n%s", out)
	}
	has("diagnostics with refresh on", must(t, "claude-code", "rules", "--delivery"), []string{"rule refresh hook on", "agent-r (Explore): current; via rule-refresh on PreToolUse:refresh"}, nil)
	must(t, "", "install", "claude", "--rule-refresh", "off")
	if h := hooksIn(); h["PreToolUse"] != nil {
		t.Fatalf("refresh hook not removed: %v", h)
	}
}

// Item 6B: a memory directory is imported only when its project is
// established; shared directories need an explicit mapping; subagent memory
// keeps its scope; and what a scan sees is reported, with retention.
func TestMemoryDiscoveryAndOwnership(t *testing.T) {
	base, home := hubEnv(t)
	aiproxy := mkRemoteRepo(t, base, "aiproxy")
	kubedl := mkRemoteRepo(t, base, "kubedl")
	must(t, "", "hub", "init")
	for _, repo := range []string{aiproxy, kubedl} { // register both projects
		os.Chdir(repo)
		must(t, "claude-code", "add", "fact", "Seed fact for "+filepath.Base(repo), "-d", "so the project exists")
	}
	topic := func(name, desc, typ, body string) string {
		return "---\nname: " + name + "\ndescription: " + desc + "\ntype: " + typ + "\n---\n\n" + body + "\n"
	}
	put := func(dir, name, content string) {
		t.Helper()
		os.MkdirAll(dir, 0o755)
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	transcript := func(dir, name, cwd string) {
		put(dir, name, `{"type":"user","cwd":"`+cwd+`","sessionId":"`+name+`","message":{"role":"user","content":"hi"}}`+"\n")
	}
	claude := filepath.Join(home, ".claude")
	// One project's directory, and one whose transcripts ran in two projects
	// (a shared project directory name).
	own := filepath.Join(claude, "projects", "-x-aiproxy")
	transcript(own, "a.jsonl", aiproxy)
	put(filepath.Join(own, "memory"), "project_one.md", topic("one", "Aiproxy deploys on Tuesdays", "project", "Releases go out every Tuesday."))
	shared := filepath.Join(claude, "projects", "work")
	transcript(shared, "a.jsonl", aiproxy)
	transcript(shared, "b.jsonl", kubedl)
	put(filepath.Join(shared, "memory"), "project_two.md", topic("two", "Shared directory note", "project", "A note from a directory two projects wrote to."))
	// A directory a repository's own settings name; one the user-level
	// settings name; one a session got through --settings.
	repoMem := filepath.Join(base, "mem-kubedl")
	put(repoMem, "project_three.md", topic("three", "Kubedl queues are per team", "project", "Each team has its own queue."))
	put(filepath.Join(kubedl, ".claude"), "settings.json", `{"autoMemoryDirectory":"`+repoMem+`"}`)
	userMem := filepath.Join(base, "mem-user")
	put(userMem, "project_four.md", topic("four", "User-level directory note", "project", "A note in the directory user settings name."))
	put(claude, "settings.json", `{"autoMemoryDirectory":"`+userMem+`","cleanupPeriodDays":210}`)
	extraMem := filepath.Join(base, "mem-extra")
	put(extraMem, "project_five.md", topic("five", "Extra settings directory note", "project", "A note in a directory an extra settings file names."))
	extraSettings := filepath.Join(base, "extra-settings.json")
	os.WriteFile(extraSettings, []byte(`{"autoMemoryDirectory":"`+extraMem+`"}`), 0o644)
	// Subagent memory: project scope under a checkout, and user scope.
	put(filepath.Join(aiproxy, ".claude", "agent-memory", "reviewer"), "feedback_agent.md", topic("agent", "Reviewer checks error wrapping", "feedback", "Flag errors returned without context."))
	put(filepath.Join(claude, "agent-memory", "reviewer"), "feedback_user_agent.md", topic("user-agent", "Reviewer prefers small diffs", "feedback", "Ask for a split when a diff is large."))

	os.Chdir(base)
	scan := func(extra ...string) string {
		t.Helper()
		os.Chdir(base)
		return must(t, "", append([]string{"extract", "--scan", "claude-memory", "--cmd", "/nonexistent/model"}, extra...)...)
	}
	out := scan()
	for _, want := range []string{
		"-x-aiproxy/memory [aiproxy]: 1 imported",
		"mem-kubedl [kubedl]: 1 imported",
		"agent-memory/reviewer [aiproxy]: 1 imported",
		"projects/work/memory: skipped (its transcripts ran in several projects (aiproxy, kubedl), so whose memory it holds is not known",
		"mem-user: skipped (autoMemoryDirectory in ~/.claude/settings.json is shared by every project that runs with it",
		"~/.claude/agent-memory/reviewer: skipped (a user-level subagent's memory spans projects and is not imported as global knowledge on its own",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("scan lacks %q:\n%s", want, out)
		}
	}
	for _, not := range []string{"mem-extra", "Shared directory note"} {
		if strings.Contains(out, not) {
			t.Errorf("scan must not mention %q yet:\n%s", not, out)
		}
	}
	search := func(q string) string { os.Chdir(base); return must(t, "codex", "search", q, "--all", "--no-docs") }
	if got := search("shared directory note user-level extra settings small diffs"); strings.Contains(got, "/facts/") || strings.Contains(got, "/conventions/") {
		t.Errorf("a directory without an established project was imported:\n%s", got)
	}
	if got := search("reviewer checks error wrapping"); !strings.Contains(got, "aiproxy/conventions/reviewer-checks-error-wrapping") {
		t.Errorf("project-scope subagent memory:\n%s", got)
	}
	// The directory an extra settings file names is seen only when given.
	if out := scan("--settings", extraSettings); !strings.Contains(out, "mem-extra: skipped (autoMemoryDirectory in ") {
		t.Errorf("--settings:\n%s", out)
	}
	// An explicit mapping settles ownership, on this machine only.
	if _, errs, code := kbRun(t, "", "", "extract", "--map-root", filepath.Join(shared, "memory")+"=nosuchproject"); code == 0 || !strings.Contains(errs, "unknown project") {
		t.Errorf("mapping to an unknown project: %d %s", code, errs)
	}
	if out := must(t, "", "extract", "--map-root", filepath.Join(shared, "memory")+"=kubedl", "--map-root", userMem+"=aiproxy", "--map-root", filepath.Join(claude, "agent-memory", "reviewer")+"=global"); !strings.Contains(out, "now belongs to kubedl (on this machine)") {
		t.Errorf("--map-root:\n%s", out)
	}
	out = scan()
	for _, want := range []string{"projects/work/memory [kubedl]: 1 imported", "mem-user [aiproxy]: 1 imported", "agent-memory/reviewer [global]: 1 imported"} {
		if !strings.Contains(out, want) {
			t.Errorf("scan after mapping lacks %q:\n%s", want, out)
		}
	}
	if got := search("shared directory note"); !strings.Contains(got, "kubedl/facts/shared-directory-note") {
		t.Errorf("mapped directory:\n%s", got)
	}
	// What a scan sees, whose it is, and how far back transcripts go.
	os.Chdir(base)
	report := must(t, "", "extract", "--sources", "--settings", extraSettings)
	for _, want := range []string{
		"Claude Code config directory: ~/.claude",
		"memory directories: 7 found, 6 with an established project, 1 not imported",
		"-x-aiproxy/memory [auto memory, 1 file(s)] -> aiproxy: its transcripts ran in this project",
		"mem-kubedl [auto memory, 1 file(s)] -> kubedl: named by autoMemoryDirectory in ",
		"projects/work/memory [auto memory, 1 file(s)] -> kubedl: mapped by hand (--map-root)",
		".claude/agent-memory/reviewer [subagent memory (project), 1 file(s)] -> aiproxy: under a checkout of aiproxy",
		"~/.claude/agent-memory/reviewer [subagent memory (user), 1 file(s)] -> global: mapped by hand",
		"mem-extra [auto memory, 1 file(s)] NOT IMPORTED: autoMemoryDirectory in ",
		"transcript retention: 210 day(s) (cleanupPeriodDays in ~/.claude/settings.json)",
		"transcripts on disk: 3, the oldest last written 0 day(s) ago",
	} {
		if !strings.Contains(report, want) {
			t.Errorf("sources report lacks %q:\n%s", want, report)
		}
	}
	// Removing a mapping makes the directory unowned again; its entries stay.
	must(t, "", "extract", "--map-root", userMem+"=")
	if out := scan(); !strings.Contains(out, "mem-user: skipped (autoMemoryDirectory in") {
		t.Errorf("after removing a mapping:\n%s", out)
	}
	// Another config directory is a different set of directories.
	alt := filepath.Join(base, "alt-config")
	put(filepath.Join(alt, "projects", "-x-kubedl", "memory"), "project_six.md", topic("six", "Alt config note", "project", "A note under another config directory."))
	transcript(filepath.Join(alt, "projects", "-x-kubedl"), "c.jsonl", kubedl)
	t.Setenv("CLAUDE_CONFIG_DIR", alt)
	if out := scan(); !strings.Contains(out, "-x-kubedl/memory [kubedl]: 1 imported") || strings.Contains(out, "-x-aiproxy") {
		t.Errorf("CLAUDE_CONFIG_DIR:\n%s", out)
	}
	if rep := must(t, "", "extract", "--sources"); !strings.Contains(rep, "transcript retention: 30 day(s) (the default)") {
		t.Errorf("default retention:\n%s", rep)
	}
	if st := strings.TrimSpace(git(t, os.Getenv("KB_HOME"), "status", "--porcelain")); st != "" {
		t.Errorf("mappings leaked into the shared hub:\n%s", st)
	}
}

// Local access statistics are a maintenance signal: they tell listed from
// shown from delivered, and never change trust or remove anything.
func TestAccessStatisticsAreOnlyASignal(t *testing.T) {
	base, _, aiproxy, _ := populatedHub(t)
	const learning = "aiproxy/learnings/retries-need-idempotency-keys"
	os.Chdir(aiproxy)
	before := must(t, "codex", "show", "--raw", learning)
	// populatedHub's own shows and hooks aside, start from a clean record.
	os.Remove(filepath.Join(os.Getenv("KB_HOME"), ".kb", "access.json"))

	must(t, "codex", "search", "idempotency retries")
	must(t, "codex", "search", "idempotency retries")
	must(t, "codex", "show", learning)
	kbRun(t, "claude-code", `{"session_id":"acc","hook_event_name":"SessionStart","cwd":"`+aiproxy+`"}`, "hook", "claude")
	t.Setenv("KB_NO_ACCESS_LOG", "1")
	must(t, "codex", "search", "idempotency retries") // not counted
	t.Setenv("KB_NO_ACCESS_LOG", "")

	data, _ := os.ReadFile(filepath.Join(os.Getenv("KB_HOME"), ".kb", "access.json"))
	var acc struct {
		Since   string `json:"since"`
		Entries map[string]struct {
			Listed, Shown, Delivered int
		} `json:"entries"`
	}
	if err := json.Unmarshal(data, &acc); err != nil || acc.Since == "" {
		t.Fatalf("access record: %v\n%s", err, data)
	}
	if e := acc.Entries[learning]; e.Listed != 2 || e.Shown != 1 || e.Delivered != 0 {
		t.Errorf("a searched and shown learning: %+v", e)
	}
	if e := acc.Entries["aiproxy/conventions/use-pnpm-never-npm"]; e.Delivered != 1 || e.Shown != 0 {
		t.Errorf("a rule pushed by the hook is delivered, not shown: %+v", e)
	}
	out := must(t, "codex", "status", "--access")
	for _, want := range []string{"a maintenance signal for this machine only", "never retrieved on this machine", learning + ": last ", "listed 2, shown 1, delivered 0"} {
		if !strings.Contains(out, want) {
			t.Errorf("status --access lacks %q:\n%s", want, out)
		}
	}
	var st struct {
		Total  int `json:"total"`
		Access struct {
			Retrieved      int      `json:"retrieved"`
			NeverRetrieved int      `json:"never_retrieved"`
			NeverIDs       []string `json:"never_retrieved_ids"`
		} `json:"access"`
		ByState map[string]int `json:"by_state"`
	}
	json.Unmarshal([]byte(must(t, "codex", "status", "--json")), &st)
	if st.Access.Retrieved+st.Access.NeverRetrieved != st.Total || st.Access.NeverRetrieved == 0 || len(st.Access.NeverIDs) != st.Access.NeverRetrieved {
		t.Errorf("status access summary: %+v", st)
	}
	if out := must(t, "codex", "status"); !strings.Contains(out, "never retrieved on this machine since") || !strings.Contains(out, "a maintenance hint only") {
		t.Errorf("status text:\n%s", out)
	}
	// Nothing about the knowledge changed: same file, same trust, nothing
	// expired or removed, and nothing reached the shared hub.
	if after := must(t, "codex", "show", "--raw", learning); after != before {
		t.Errorf("an entry changed because it was retrieved:\n%s", after)
	}
	os.Chdir(base)
	if all := must(t, "codex", "status", "--all"); !strings.Contains(all, "knowledge: 6 active") || !strings.Contains(all, "deprecated 0") {
		t.Errorf("access statistics must not retire anything:\n%s", all)
	}
	if st := strings.TrimSpace(git(t, os.Getenv("KB_HOME"), "status", "--porcelain")); st != "" {
		t.Errorf("access statistics leaked into the shared hub:\n%s", st)
	}
}

// Item 4: file context is off by default; when turned on it hands an agent
// what is recorded about a file it reads or edits, once per agent, entry
// revision and compaction, within a budget and within the project.
func TestFileContextHook(t *testing.T) {
	base, home, aiproxy, kubedl := populatedHub(t)
	const learning = "aiproxy/learnings/retries-need-idempotency-keys"
	settings := filepath.Join(home, ".claude", "settings.json")
	hooksIn := func() map[string]any {
		var m map[string]any
		data, _ := os.ReadFile(settings)
		json.Unmarshal(data, &m)
		h, _ := m["hooks"].(map[string]any)
		return h
	}
	file := filepath.Join(aiproxy, "pkg", "x.go")
	tool := func(cwd, event, toolName, path, session, agent string) string {
		t.Helper()
		os.Chdir(cwd)
		in := `{"session_id":"` + session + `","hook_event_name":"` + event + `","tool_name":"` + toolName + `","tool_input":{"file_path":"` + path + `","command":"cat ` + path + `"},"cwd":"` + cwd + `"`
		if agent != "" {
			in += `,"agent_id":"` + agent + `","agent_type":"Explore"`
		}
		out, _, code := kbRun(t, "claude-code", in+"}", "hook", "claude", "file")
		if code != 0 {
			t.Fatalf("hook exit %d", code)
		}
		if strings.TrimSpace(out) == "" {
			return ""
		}
		var m struct {
			Out map[string]any `json:"hookSpecificOutput"`
		}
		if err := json.Unmarshal([]byte(out), &m); err != nil {
			t.Fatalf("not hook JSON: %v\n%s", err, out)
		}
		if _, decides := m.Out["permissionDecision"]; decides {
			t.Fatalf("the file-context hook must never decide a permission: %s", out)
		}
		return fmt.Sprint(m.Out["hookEventName"]) + "|" + fmt.Sprint(m.Out["additionalContext"])
	}
	read := func(session, agent string) string { return tool(aiproxy, "PostToolUse", "Read", file, session, agent) }

	// Off by default: no hook entry, and the handler says nothing even if
	// something still calls it.
	if h := hooksIn(); h["PostToolUse"] != nil || h["PreToolUse"] != nil {
		t.Fatalf("file context must be opt-in: %v", h)
	}
	if out := read("off", ""); out != "" {
		t.Fatalf("off must inject nothing:\n%s", out)
	}

	out := must(t, "", "install", "claude", "--file-context", "post")
	if !strings.Contains(out, "File context is on (post)") || !strings.Contains(out, "not the edit that triggered it") {
		t.Errorf("install notes:\n%s", out)
	}
	post := hooksIn()["PostToolUse"].([]any)[0].(map[string]any)
	if post["matcher"] != "Read|Edit|Write" || hooksIn()["PreToolUse"] != nil {
		t.Fatalf("post mode hooks: %v", hooksIn())
	}
	// After a Read: the anchored entry's header, with its freshness label,
	// worded as input for what follows.
	out = read("s1", "")
	for _, want := range []string{"PostToolUse|kb: recorded about pkg/x.go, for the work that follows", "- " + learning + " [verified,code-changed,stale] Retries need idempotency keys - Upstream jobs duplicate without keys."} {
		if !strings.Contains(out, want) {
			t.Errorf("post-read context lacks %q:\n%s", want, out)
		}
	}
	// Once per agent: a second touch adds nothing, by Read or by Edit.
	if again := read("s1", "") + tool(aiproxy, "PostToolUse", "Edit", file, "s1", ""); again != "" {
		t.Errorf("the same agent got the same entry twice:\n%s", again)
	}
	// Another agent of the session has its own context.
	if sub := read("s1", "agent-1"); !strings.Contains(sub, learning) {
		t.Errorf("a subagent must get it regardless of its parent:\n%s", sub)
	}
	if sub := read("s1", "agent-1"); sub != "" {
		t.Errorf("the subagent got it twice:\n%s", sub)
	}
	// After that agent compacts, it starts over; the main agent does not.
	os.Chdir(aiproxy)
	kbRun(t, "claude-code", `{"session_id":"s1","hook_event_name":"SessionStart","source":"compact","agent_id":"agent-1","agent_type":"Explore","cwd":"`+aiproxy+`"}`, "hook", "claude")
	if sub := read("s1", "agent-1"); !strings.Contains(sub, learning) {
		t.Errorf("after its compaction the subagent should get the entry again:\n%s", sub)
	}
	if again := read("s1", ""); again != "" {
		t.Errorf("another agent's compaction reset the main agent:\n%s", again)
	}
	// A rewritten entry is given again; a rule that applies to the file comes first.
	must(t, "codex", "update", learning, "-d", "Upstream jobs duplicate unless every retry carries a key.", "-m", "clearer", "-e", "pkg/x.go")
	must(t, "claude-code", "add", "rule", "Keep pkg free of global state", "-d", "tests run in parallel", "-e", "user said so", "--when", "pkg/**")
	out = read("s1", "")
	if !strings.Contains(out, "unless every retry carries a key") || !strings.Contains(out, "- rule for this file: Keep pkg free of global state") || strings.Index(out, "rule for this file") > strings.Index(out, learning) {
		t.Errorf("after an update and a new file rule:\n%s", out)
	}
	// Scope: another project's knowledge never leaks, even for the same
	// relative path; files outside the repository and tools that are not
	// Read, Edit or Write are not covered.
	if out := tool(kubedl, "PostToolUse", "Read", filepath.Join(kubedl, "pkg", "x.go"), "s2", ""); out != "" {
		t.Errorf("cross-project leak:\n%s", out)
	}
	if out := tool(aiproxy, "PostToolUse", "Read", filepath.Join(base, "elsewhere.go"), "s3", ""); out != "" {
		t.Errorf("a file outside the repository:\n%s", out)
	}
	if out := tool(aiproxy, "PostToolUse", "Bash", file, "s3", ""); out != "" {
		t.Errorf("a shell command is not a covered file access:\n%s", out)
	}
	// Post mode never speaks before an edit.
	if out := tool(aiproxy, "PreToolUse", "Edit", file, "s4", ""); out != "" {
		t.Errorf("post mode must not inject before an edit:\n%s", out)
	}

	// Pre mode: context before the edit, worded as such, then nothing new
	// after it.
	must(t, "", "install", "claude", "--file-context", "pre")
	if h := hooksIn(); h["PreToolUse"].([]any)[0].(map[string]any)["matcher"] != "Edit|Write" || h["PostToolUse"] == nil {
		t.Fatalf("pre mode hooks: %v", h)
	}
	out = tool(aiproxy, "PreToolUse", "Edit", file, "s5", "")
	if !strings.Contains(out, "PreToolUse|kb: recorded about pkg/x.go, before you change it") || !strings.Contains(out, learning) {
		t.Errorf("pre-edit context:\n%s", out)
	}
	if after := tool(aiproxy, "PostToolUse", "Edit", file, "s5", ""); after != "" {
		t.Errorf("the edit that follows adds nothing new:\n%s", after)
	}
	if out := tool(aiproxy, "PreToolUse", "Read", file, "s6", ""); out != "" {
		t.Errorf("pre mode covers Edit and Write, not Read:\n%s", out)
	}

	// The benchmark measures the same lookup against judged cases without
	// injecting or recording anything.
	cases := filepath.Join(base, "cases.json")
	os.WriteFile(cases, []byte(`[{"file":"pkg/x.go","relevant":["`+learning+`","aiproxy/learnings/not-recorded"]},{"file":"docs/readme.md","relevant":[]}]`), 0o644)
	bench := must(t, "codex", "bench", "file-context", "--cases", cases)
	for _, want := range []string{"pkg/x.go: injected ", "relevant 1 of 2", "docs/readme.md: injected 0", "cases 2, injections 1: precision 0.50 (1 of 2 injected were judged relevant)", "recall 0.50 (1 of 2 judged relevant were injected)", "budget 2000", "File context stays off until they say it helps."} {
		if !strings.Contains(bench, want) {
			t.Errorf("benchmark lacks %q:\n%s", want, bench)
		}
	}
	// Budget: many anchored entries fit one injection, whole lines only.
	var items []string
	for i := 0; i < 30; i++ {
		items = append(items, fmt.Sprintf(`{"type":"learning","title":"Package pitfall number %02d about retries and timeouts","description":"A long enough description of pitfall %02d to take real space in a header line.","resources":["pkg/**"]}`, i, i))
	}
	os.Chdir(aiproxy)
	if out, errs, code := kbRun(t, "claude-code", "["+strings.Join(items, ",")+"]", "add", "--from", "-", "-e", "read the code", "--force"); code != 0 || !strings.Contains(out, "seeded 30 of 30") {
		t.Fatalf("seed: %d %s %s", code, out, errs)
	}
	out = read("s7", "")
	ctx := out[strings.Index(out, "|")+1:]
	units := 0
	for _, r := range ctx {
		if r >= 0x10000 {
			units++
		}
		units++
	}
	if units > 2000 || units < 1200 || !strings.Contains(ctx, "more: kb search -f pkg/x.go)") {
		t.Errorf("injection is %d units; it must stay within 2000 and say what it left out:\n%s", units, ctx)
	}
	for _, l := range strings.Split(strings.TrimSpace(ctx), "\n")[1:] {
		if !strings.HasPrefix(l, "- ") && !strings.HasPrefix(l, "(+") {
			t.Errorf("a line was split: %q", l)
		}
	}
	// What was left out comes with the next touch, not never.
	if next := read("s7", ""); !strings.Contains(next, "Package pitfall number") || strings.Contains(next, learning+" [") {
		t.Errorf("the next touch should bring entries not yet given:\n%s", next)
	}

	if d := must(t, "codex", "rules", "--delivery"); !strings.Contains(d, "file context pre") {
		t.Errorf("diagnostics:\n%s", d)
	}
	// Off again: the hooks go, kb's other hooks stay, and it is remembered.
	must(t, "", "install", "claude", "--file-context", "off")
	must(t, "", "install", "claude")
	if h := hooksIn(); h["PostToolUse"] != nil || h["PreToolUse"] != nil || h["SessionStart"] == nil || h["SubagentStart"] == nil {
		t.Fatalf("after turning file context off: %v", h)
	}
	if out := read("s8", ""); out != "" {
		t.Errorf("off must inject nothing:\n%s", out)
	}
}
