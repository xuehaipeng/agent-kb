package kb

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// newRepo creates a git repo with a knowledge bundle and returns the store.
func newRepo(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q", "-b", "main")
	write(t, filepath.Join(dir, "internal/gateway/stream.go"), "package gateway\n")
	write(t, filepath.Join(dir, "README.md"), "hi\n")
	write(t, filepath.Join(dir, "knowledge", ConfigFile), DefaultConfigYAML)
	run("add", "-A")
	run("commit", "-q", "-m", "init")
	// Open through a symlink, as macOS does for t.TempDir (/var -> /private/var),
	// so path handling is exercised against git's resolved toplevel everywhere.
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(dir, link); err != nil {
		t.Fatal(err)
	}
	s, err := Open(filepath.Join(link, "knowledge"))
	if err != nil {
		t.Fatal(err)
	}
	s.Now = func() time.Time { return time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC) }
	return s
}

func write(t *testing.T, p, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func gitRun(t *testing.T, s *Store, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = s.CodeRoot
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	s.git = nil
}

var (
	claude = Actor{Producer: "claude-code", Model: "opus-4.5"}
	codex  = Actor{Producer: "codex", Model: "gpt-5.2"}
	human  = Actor{Producer: "human", Model: "xue"}
)

func TestFamilies(t *testing.T) {
	cfg, _ := LoadConfig(t.TempDir())
	cases := map[string]string{
		"claude-code/opus-4.5":      "anthropic",
		"codex":                     "openai",
		"cursor/claude-sonnet-4.5":  "anthropic",
		"cursor/gpt-5":              "openai",
		"cursor":                    "cursor",
		"hermes/claude-sonnet-4":    "anthropic",
		"hermes":                    "nous",
		"human:xue":                 "human",
		"antigravity/gemini-3-pro":  "google",
		"something-new/unknown-llm": "something-new",
	}
	for in, want := range cases {
		if got := cfg.FamilyOf(in); got != want {
			t.Errorf("FamilyOf(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestDetectProducer(t *testing.T) {
	if p := detectProducer([]string{"CLAUDECODE=1"}); p != "claude-code" {
		t.Fatal(p)
	}
	if p := detectProducer([]string{"CODEX_SANDBOX=seatbelt"}); p != "codex" {
		t.Fatal(p)
	}
	if p := detectProducer([]string{"HOME=/x"}); p != "" {
		t.Fatal(p)
	}
}

func TestGlob(t *testing.T) {
	cases := []struct {
		pat, file string
		want      bool
	}{
		{"pkg/inference/**", "pkg/inference/sglang/cache.go", true},
		{"pkg/inference/**", "pkg/other/x.go", false},
		{"pkg/inference", "pkg/inference/a.go", true},
		{"pkg/inference/", "pkg/inference/a/b.go", true},
		{"internal/scheduler/watermark.go", "internal/scheduler/watermark.go", true},
		{"*.go", "a/b/c.go", true},
		{"*.go", "a/b/c.ts", false},
		{"**/*_test.go", "x_test.go", true},
		{"**/*_test.go", "a/b/x_test.go", true},
		{"src/**/api/*.ts", "src/a/b/api/x.ts", true},
		{"src/**/api/*.ts", "src/api/x.ts", true},
		{"./src/*.ts", "src/x.ts", true},
		{"src/*.ts", "src/a/x.ts", false},
	}
	for _, c := range cases {
		if got := MatchGlob(c.pat, c.file); got != c.want {
			t.Errorf("MatchGlob(%q, %q) = %v, want %v", c.pat, c.file, got, c.want)
		}
	}
}

func TestTokenize(t *testing.T) {
	got := strings.Join(Tokenize("Caches are evicted when the KV-cache races; 缓存驱逐"), " ")
	for _, want := range []string{"cach", "evict", "kv", "rac", "缓存", "驱逐"} {
		if !strings.Contains(" "+got+" ", " "+want+" ") {
			t.Errorf("tokens %q missing %q", got, want)
		}
	}
	if Tokenize("fixes")[0] != Tokenize("fix")[0] {
		t.Errorf("fix/fixes should share a stem: %v %v", Tokenize("fixes"), Tokenize("fix"))
	}
}

func TestSlugify(t *testing.T) {
	if got := Slugify("pnpm test hangs under Node 24 (without --no-isolate)!"); got != "pnpm-test-hangs-under-node-24-without-no-isolate" {
		t.Fatal(got)
	}
	if got := Slugify("Search scans the bundle in-process with BM25 instead of a SQLite FTS5 cache"); got != "search-scans-the-bundle-in-process-with-bm25-instead" {
		t.Fatal(got)
	}
	if got := Slugify("网关 超时 传播"); got != "网关-超时-传播" {
		t.Fatal(got)
	}
}

func TestRedact(t *testing.T) {
	in := "token ghp_abcdefghijklmnopqrstuvwxyz0123456789 and password=hunter2hunter2 and AKIAABCDEFGHIJKLMNOP"
	out, n := Redact(in)
	if n != 3 || strings.Contains(out, "hunter2") || strings.Contains(out, "ghp_") || strings.Contains(out, "AKIA") {
		t.Fatalf("redact: %d %q", n, out)
	}
}

func TestAddVerifyLifecycle(t *testing.T) {
	s := newRepo(t)
	res, err := s.Add(AddInput{
		Type: "learning", Title: "Stream timeout must propagate context cancel",
		Description: "Upstream SGLang stream keeps running unless ctx cancel is forwarded.",
		Body:        "Forward ctx.Done() into the upstream call.", Resources: []string{"internal/gateway/**"},
		Tags: []string{"gateway,grpc"}, Actor: claude,
	})
	if err != nil {
		t.Fatal(err)
	}
	n := res.Node
	if n.ID != "learnings/stream-timeout-must-propagate-context-cancel" {
		t.Fatalf("id %s", n.ID)
	}
	if n.Meta().Generated.Commit == "" || n.Meta().Status != "draft" || n.Meta().StaleAfter != "2026-12-08T12:00:00Z" {
		t.Fatalf("meta %+v", n.Meta())
	}
	if got := s.Trust(n); got.State != StateUnverified || got.Stale {
		t.Fatalf("trust %+v", got)
	}

	// Duplicate rejected.
	_, err = s.Add(AddInput{Type: "learning", Title: "Stream timeout must propagate context cancel", Description: "Upstream SGLang stream keeps running unless ctx cancel is forwarded.", Actor: codex})
	var dup *DuplicateError
	if !errors.As(err, &dup) {
		t.Fatalf("want duplicate error, got %v", err)
	}

	// Same family cannot verify.
	if _, _, err := s.Verify(VerifyInput{Ref: n.ID, Result: ResultConfirmed, Evidence: "x", Actor: Actor{Producer: "cursor", Model: "claude-sonnet-4.5"}}); err == nil || !strings.Contains(err.Error(), "self-verification") {
		t.Fatalf("want self-verification rejection, got %v", err)
	}
	// Evidence required.
	if _, _, err := s.Verify(VerifyInput{Ref: n.ID, Result: ResultConfirmed, Actor: codex}); err == nil {
		t.Fatal("want evidence error")
	}
	// Cross-family confirm by short ref.
	n2, tr, err := s.Verify(VerifyInput{Ref: "stream-timeout", Result: ResultConfirmed, Evidence: "read stream.go; test passes", Actor: codex})
	if err != nil {
		t.Fatal(err)
	}
	if tr.State != StateCorroborated || n2.Meta().Status != "stable" {
		t.Fatalf("after confirm: %+v status=%s", tr, n2.Meta().Status)
	}

	// Code change under resources makes it stale.
	write(t, filepath.Join(s.CodeRoot, "internal/gateway/stream.go"), "package gateway\n// changed\n")
	gitRun(t, s, "commit", "-qam", "change gateway")
	s.Invalidate()
	n3, _ := s.Get(n.ID)
	if tr := s.Trust(n3); !tr.Stale || !strings.Contains(tr.StaleReason, "internal/gateway/stream.go") {
		t.Fatalf("want code-stale, got %+v", tr)
	}

	// Dispute from a third family -> disputed; human resolves.
	_, tr, err = s.Verify(VerifyInput{Ref: n.ID, Result: ResultDisputed, Evidence: "cancel no longer forwarded after refactor", Actor: Actor{Producer: "antigravity", Model: "gemini-3"}})
	if err != nil || tr.State != StateDisputed {
		t.Fatalf("dispute: %v %+v", err, tr)
	}
	if _, _, err := s.Resolve(n.ID, true, "", codex); err == nil {
		t.Fatal("agents must not resolve")
	}
	_, tr, err = s.Resolve(n.ID, true, "checked: still forwarded", human)
	if err != nil || tr.State != StateReviewed || tr.Stale {
		t.Fatalf("resolve: %v %+v", err, tr)
	}

	// Lint is clean after regenerating the index.
	if _, err := s.WriteIndex(); err != nil {
		t.Fatal(err)
	}
	issues, err := s.Lint(LintOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, i := range issues {
		if i.Severity == "error" {
			t.Errorf("lint: %s", i)
		}
	}
	log, _ := os.ReadFile(filepath.Join(s.Dir, "log.md"))
	if strings.Count(string(log), "\n- [") != 4 {
		t.Errorf("log entries:\n%s", log)
	}
}

func TestSupersedeAndSearch(t *testing.T) {
	s := newRepo(t)
	old, err := s.Add(AddInput{Type: "decision", Title: "Use Prisma for the ORM", Description: "Prisma chosen for type safety.", Actor: claude})
	if err != nil {
		t.Fatal(err)
	}
	nw, err := s.Add(AddInput{Type: "decision", Title: "Use Drizzle instead of Prisma", Description: "Drizzle replaces Prisma: faster cold starts.", Supersedes: []string{old.Node.ID}, Actor: codex})
	if err != nil {
		t.Fatal(err)
	}
	s.Invalidate()
	o, _ := s.Get(old.Node.ID)
	if tr := s.Trust(o); tr.State != StateDeprecated || tr.SupersededBy != nw.Node.ID {
		t.Fatalf("superseded: %+v", tr)
	}
	_, err = s.Add(AddInput{Type: "fact", Title: "Gateway forwards gRPC deadlines", Description: "Deadlines propagate to upstream workers.", Resources: []string{"internal/gateway/**"}, Actor: claude})
	if err != nil {
		t.Fatal(err)
	}
	hits, err := s.Search(SearchQuery{Text: "prisma orm"})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].Node.ID != nw.Node.ID {
		t.Fatalf("deprecated should be hidden: %+v", hits)
	}
	hits, _ = s.Search(SearchQuery{Text: "prisma", IncludeDeprecated: true})
	if len(hits) != 2 {
		t.Fatalf("include deprecated: %d", len(hits))
	}
	hits, _ = s.Search(SearchQuery{Files: []string{"internal/gateway/stream.go"}})
	if len(hits) != 1 || hits[0].PathMatch == "" || !strings.HasPrefix(hits[0].Node.ID, "facts/") {
		t.Fatalf("path search: %+v", hits)
	}
	hits, _ = s.Search(SearchQuery{Text: "deadline", TrustedOnly: true})
	if len(hits) != 0 {
		t.Fatalf("trusted-only should hide unverified: %d", len(hits))
	}
}

func TestReviewFlow(t *testing.T) {
	s := newRepo(t)
	if _, err := s.Add(AddInput{Type: "fact", Title: "Gateway forwards gRPC deadlines", Description: "Deadlines propagate upstream.", Resources: []string{"internal/gateway/**"}, Actor: codex}); err != nil {
		t.Fatal(err)
	}
	gitRun(t, s, "add", "-A")
	gitRun(t, s, "commit", "-qm", "kb")
	write(t, filepath.Join(s.CodeRoot, "internal/gateway/stream.go"), "package gateway\nfunc X() {}\n")
	gitRun(t, s, "commit", "-qam", "feature")

	rv, err := s.ReviewSubmit(SubmitInput{Title: "Refactor stream timeout", Summary: "Forward ctx cancel.", Base: "HEAD~1", Assumptions: []string{"cancel aborts upstream"}, Actor: claude})
	if err != nil {
		t.Fatal(err)
	}
	m := rv.Meta()
	if m.Review == nil || len(m.Review.Files) != 1 || m.Review.Files[0] != "internal/gateway/stream.go" || m.Review.ReviewerMustDifferFrom != "anthropic" {
		t.Fatalf("packet: %+v", m.Review)
	}
	if len(m.Review.Concepts) != 1 {
		t.Fatalf("expected anchored concept attached: %+v", m.Review.Concepts)
	}
	if items, _ := s.Reviews(ReviewFilter{ForMe: claude}); len(items) != 0 {
		t.Fatal("author must not see own review as waiting")
	}
	if items, _ := s.Reviews(ReviewFilter{ForMe: codex}); len(items) != 1 {
		t.Fatal("codex should see the review")
	}
	if _, _, err := s.ReviewVerdict(VerdictInput{Ref: rv.ID, Approve: true, Actor: claude}); err == nil {
		t.Fatal("self-review must be rejected")
	}
	_, st, err := s.ReviewVerdict(VerdictInput{Ref: rv.ID, Approve: false, Findings: []string{"timeout_test.go does not cover cancel"}, Actor: codex})
	if err != nil || st != ReviewChanges {
		t.Fatalf("verdict: %v %s", err, st)
	}
	write(t, filepath.Join(s.CodeRoot, "internal/gateway/stream_test.go"), "package gateway\n")
	gitRun(t, s, "add", "-A")
	gitRun(t, s, "commit", "-qm", "fix")
	n, err := s.ReviewRevise(rv.ID, "", "added cancel test", claude)
	if err != nil {
		t.Fatal(err)
	}
	if st, _ := s.ReviewState(n); st != ReviewPending || len(n.Meta().Review.Files) < 2 {
		t.Fatalf("revise: %s %+v", st, n.Meta().Review)
	}
	_, st, err = s.ReviewVerdict(VerdictInput{Ref: rv.ID, Approve: true, Summary: "cancel covered now", Actor: codex})
	if err != nil || st != ReviewApproved {
		t.Fatalf("approve: %v %s", err, st)
	}
	if !strings.Contains(n.Doc.Body, "revised by") {
		t.Fatalf("body: %s", n.Doc.Body)
	}
}

func TestLintCatchesProblems(t *testing.T) {
	s := newRepo(t)
	write(t, filepath.Join(s.Dir, "facts/bad.md"), "---\ntype: Fact\ntitle: Bad\ngenerated: {by: x, at: 2026-11-08T00:00:00}\n---\nSee [missing](nope.md) and [^s1]. Retired publish.sh here.\n")
	write(t, filepath.Join(s.Dir, "facts/broken.md"), "no frontmatter\n")
	s.Cfg.Lint.RetiredTerms = []RetiredTerm{{Term: "publish.sh", Hint: "use make release"}}
	issues, err := s.Lint(LintOptions{})
	if err != nil {
		t.Fatal(err)
	}
	all := ""
	for _, i := range issues {
		all += i.String() + "\n"
	}
	for _, want := range []string{"no timezone offset", "broken link", "footnote [^s1]", "retired term", "missing YAML frontmatter", "out of date"} {
		if !strings.Contains(all, want) {
			t.Errorf("lint missing %q in:\n%s", want, all)
		}
	}
}

func TestImmutableStagedLint(t *testing.T) {
	s := newRepo(t)
	res, err := s.Add(AddInput{Type: "learning", Title: "Vitest hangs on Node 24", Description: "Use --no-isolate.", Actor: claude})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.WriteIndex(); err != nil {
		t.Fatal(err)
	}
	gitRun(t, s, "add", "-A")
	gitRun(t, s, "commit", "-qm", "kb")
	// Tool-made verification is allowed.
	if _, _, err := s.Verify(VerifyInput{Ref: res.Node.ID, Result: ResultConfirmed, Evidence: "reproduced", Actor: codex}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.WriteIndex(); err != nil {
		t.Fatal(err)
	}
	gitRun(t, s, "add", "-A")
	issues, _ := s.Lint(LintOptions{Staged: true})
	for _, i := range issues {
		if strings.Contains(i.Msg, "immutable") {
			t.Fatalf("verification should not trip immutability: %s", i)
		}
	}
	// Hand edit of the body is not.
	data, _ := os.ReadFile(res.Node.Path)
	write(t, res.Node.Path, strings.Replace(string(data), "Use --no-isolate.", "Pin Node 22.", 1))
	gitRun(t, s, "add", "-A")
	issues, _ = s.Lint(LintOptions{Staged: true})
	found := false
	for _, i := range issues {
		if strings.Contains(i.Msg, "immutable") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected immutability error, got %v", issues)
	}
}

func TestIndexDeterministic(t *testing.T) {
	s := newRepo(t)
	for _, title := range []string{"Beta fact", "Alpha fact"} {
		if _, err := s.Add(AddInput{Type: "fact", Title: title, Description: "d " + title, Actor: claude}); err != nil {
			t.Fatal(err)
		}
	}
	a, _ := s.IndexFiles()
	s.Now = func() time.Time { return time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC) }
	s.Invalidate()
	b, _ := s.IndexFiles()
	for k := range a {
		if string(a[k]) != string(b[k]) {
			t.Fatalf("index %s depends on time", k)
		}
	}
	idx := string(a["facts/index.md"])
	if strings.Index(idx, "Alpha") > strings.Index(idx, "Beta") {
		t.Fatalf("not sorted:\n%s", idx)
	}
}

func TestStatusBrief(t *testing.T) {
	s := newRepo(t)
	res, _ := s.Add(AddInput{Type: "fact", Title: "Claim one", Description: "x y z", Actor: claude})
	if _, _, err := s.Verify(VerifyInput{Ref: res.Node.ID, Result: ResultDisputed, Evidence: "wrong", Actor: codex}); err != nil {
		t.Fatal(err)
	}
	r, err := s.Status(codex)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Disputed) != 1 || !strings.Contains(r.Brief(), "disputed") {
		t.Fatalf("status: %+v brief=%q", r, r.Brief())
	}
}
