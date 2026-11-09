package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
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
	if !strings.Contains(out, "aiproxy/learnings/retries-need-idempotency-keys [verified]") {
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
	if !strings.Contains(out, "-x-aiproxy/memory/MEMORY.md: added 1") || !strings.Contains(out, "-x-kubedl/memory/MEMORY.md: skipped (not in the selected projects)") {
		t.Fatalf("filtered scan:\n%s", out)
	}
	// The filtered run did not mark files as done: a full scan still takes kubedl.
	out = must(t, "", "extract", "--scan", "claude-memory", "--cmd", cmd)
	if !strings.Contains(out, "-x-kubedl/memory/MEMORY.md: added 1") {
		t.Fatalf("full scan after filtered one:\n%s", out)
	}
}
