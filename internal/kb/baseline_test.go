package kb

import (
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xuehaipeng/agent-kb/internal/okf"
)

// repoAt creates a repo at dir with an origin remote and one committed file
// whose content is unique to the repo (so two repos never share a commit).
func repoAt(t *testing.T, dir, remote string) string {
	t.Helper()
	write(t, filepath.Join(dir, "pkg", "main.go"), "package main\n// "+remote+"\n")
	runGit(t, dir, "init", "-q", "-b", "main")
	runGit(t, dir, "remote", "add", "origin", remote)
	runGit(t, dir, "add", "-A")
	runGit(t, dir, "commit", "-qm", "init "+remote)
	return realPath(dir)
}

func commitFile(t *testing.T, dir, rel, content string) {
	t.Helper()
	write(t, filepath.Join(dir, rel), content)
	runGit(t, dir, "add", "-A")
	runGit(t, dir, "commit", "-qm", "change "+rel)
}

func anchored(t *testing.T, s *Store, title string, resources ...string) *Node {
	t.Helper()
	r, err := s.Add(AddInput{Type: "learning", Title: title, Description: title + " (details).", Resources: resources, Evidence: "read the code", Actor: claude, Force: true})
	if err != nil {
		t.Fatal(err)
	}
	return r.Node
}

func trustOf(t *testing.T, s *Store, id string) Trust {
	t.Helper()
	n, err := s.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	return s.Trust(n)
}

func TestBaselineSelectionIgnoresRegistrationOrder(t *testing.T) {
	hub := newHub(t)
	base := t.TempDir()
	const repo = "github.com/acme/api"
	main := repoAt(t, filepath.Join(base, "api"), "git@github.com:Acme/api.git")
	entry := anchored(t, openAt(t, main), "Handlers must check the tenant header", "pkg/**")

	// A worktree ahead of main, registered after it: the old registry put it
	// first and compared every outside lookup with it.
	wt := filepath.Join(base, "wt-feature")
	runGit(t, main, "worktree", "add", "-q", "-b", "feature", wt)
	wt = realPath(wt)
	commitFile(t, wt, "pkg/main.go", "package main\n// feature work\n")
	inWT := openAt(t, wt)

	outside := openAt(t, t.TempDir())
	if sel := outside.SelectCheckout("api", repo); sel.Path != main || sel.Why != "primary worktree" {
		t.Fatalf("outside selection: %+v", sel)
	}
	tr := trustOf(t, outside, entry.ID)
	if tr.CodeFreshness != FreshUnchanged || tr.Stale || tr.Baseline == nil || tr.Baseline.Path != main || tr.Baseline.Repo != repo || tr.Baseline.Head == "" {
		t.Fatalf("outside: %+v baseline %+v", tr, tr.Baseline)
	}
	if tr.Badge() != "verified,code-unchanged" {
		t.Fatalf("badge: %s", tr.Badge())
	}
	// Inside the worktree the current checkout is the baseline, and says so.
	tr = trustOf(t, inWT, entry.ID)
	if tr.CodeFreshness != FreshChanged || !tr.Stale || tr.Baseline.Path != wt || tr.Baseline.Selection != "current checkout" || tr.Badge() != "verified,code-changed,stale" {
		t.Fatalf("inside worktree: %+v baseline %+v", tr, tr.Baseline)
	}

	// A registry written by an older kb (paths only, newest first) is read by
	// inspecting the checkouts, and the identities are remembered.
	regPath := filepath.Join(hub, LocalDir, "registry.json")
	write(t, regPath, `{"projects":{"api":["`+wt+`","`+main+`","/nonexistent/old-worktree"]}}`)
	legacy := openAt(t, t.TempDir())
	cos := legacy.Checkouts("api")
	if len(cos) != 3 {
		t.Fatalf("checkouts: %+v", cos)
	}
	for _, c := range cos {
		switch {
		case c.Path == "/nonexistent/old-worktree":
			if c.Exists || c.Repo != "" {
				t.Fatalf("a missing path must not get a guessed identity: %+v", c)
			}
		case c.Repo != repo || c.CommonDir == "" || !c.Exists:
			t.Fatalf("migrated checkout: %+v", c)
		}
	}
	if sel := legacy.SelectCheckout("api", repo); sel.Path != main {
		t.Fatalf("legacy registry selection: %+v", sel)
	}
	if data, _ := os.ReadFile(filepath.Join(hub, LocalDir, "checkouts.json")); !strings.Contains(string(data), `"common_dir"`) {
		t.Fatalf("identities not persisted:\n%s", data)
	}
	// registry.json keeps the format older versions read and rewrite.
	if data, _ := os.ReadFile(regPath); strings.Contains(string(data), "common_dir") || !strings.Contains(string(data), `"projects"`) {
		t.Fatalf("registry.json format changed:\n%s", data)
	}

	// An independent clone: two candidates, no arbitrary pick.
	clone := filepath.Join(base, "api-clone")
	runGit(t, base, "clone", "-q", main, clone)
	runGit(t, clone, "remote", "set-url", "origin", "https://github.com/acme/api")
	clone = realPath(clone)
	commitFile(t, clone, "pkg/main.go", "package main\n// clone work\n")
	openAt(t, clone)
	outside = openAt(t, t.TempDir())
	sel := outside.SelectCheckout("api", repo)
	if sel.Path != "" || sel.Code != UnknownAmbiguousCo || !strings.Contains(sel.Reason, "kb project checkout set api --repo "+repo) {
		t.Fatalf("two clones: %+v", sel)
	}
	tr = trustOf(t, outside, entry.ID)
	if tr.CodeFreshness != FreshUnknown || tr.FreshnessCode != UnknownAmbiguousCo || tr.Stale || tr.Badge() != "verified,unknown-freshness" {
		t.Fatalf("ambiguous clones must be unknown, not fresh and not stale: %+v", tr)
	}
	if out := outside.RenderCheckouts("api"); !strings.Contains(out, "baseline: none") || !strings.Contains(out, "missing") {
		t.Fatalf("checkout list:\n%s", out)
	}

	// An explicit preference settles it, on this machine only.
	if _, _, err := outside.SetPreferredCheckout("api", "github.com/acme/other", clone); err == nil {
		t.Fatal("a preference for the wrong repository was accepted")
	}
	if _, _, err := outside.SetPreferredCheckout("api", "", t.TempDir()); err == nil {
		t.Fatal("a preference for a non-git directory was accepted")
	}
	if id, top, err := outside.SetPreferredCheckout("api", "", clone); err != nil || id != repo || top != clone {
		t.Fatalf("set: %v %s %s", err, id, top)
	}
	fresh := openAt(t, t.TempDir())
	if sel := fresh.SelectCheckout("api", repo); sel.Path != clone || sel.Why != "preferred checkout" {
		t.Fatalf("preferred: %+v", sel)
	}
	if tr := trustOf(t, fresh, entry.ID); tr.CodeFreshness != FreshChanged || tr.Baseline.Path != clone {
		t.Fatalf("preferred baseline: %+v", tr)
	}
	if st := strings.TrimSpace(runGit(t, hub, "status", "--porcelain")); st != "" {
		t.Fatalf("the preference leaked into shared hub files:\n%s", st)
	}
	// An older kb registering a checkout rewrites registry.json with only
	// the fields it knows; the preference must survive that.
	write(t, regPath, `{"projects":{"api":["`+clone+`","`+wt+`","`+main+`"]}}`)
	if sel := openAt(t, t.TempDir()).SelectCheckout("api", repo); sel.Path != clone || sel.Why != "preferred checkout" {
		t.Fatalf("preference lost after a legacy rewrite: %+v", sel)
	}
	if _, err := fresh.ClearPreferredCheckout("api", ""); err != nil {
		t.Fatal(err)
	}
	if sel := openAt(t, t.TempDir()).SelectCheckout("api", repo); sel.Code != UnknownAmbiguousCo {
		t.Fatalf("after clear: %+v", sel)
	}
	// A preferred checkout that disappears is not used; with the clone gone
	// the single remaining clone's primary worktree is unambiguous again.
	if _, _, err := fresh.SetPreferredCheckout("api", repo, clone); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(clone); err != nil {
		t.Fatal(err)
	}
	if sel := openAt(t, t.TempDir()).SelectCheckout("api", repo); sel.Path != main || sel.Why != "primary worktree" {
		t.Fatalf("after the preferred clone vanished: %+v", sel)
	}
}

func TestUnknownFreshnessIsNotFresh(t *testing.T) {
	newHub(t)
	dir := filepath.Join(t.TempDir(), "api")
	repo := repoAt(t, dir, "git@github.com:Acme/api.git")
	a := openAt(t, repo)
	entry := anchored(t, a, "Retries need idempotency keys", "pkg/**")
	plain, err := a.Add(AddInput{Type: "decision", Title: "Use Postgres for the job queue", Description: "Chosen over Redis for durability.", Evidence: "user said so", Actor: claude})
	if err != nil {
		t.Fatal(err)
	}
	if tr := a.Trust(plain.Node); tr.CodeFreshness != FreshNotApplicable || tr.Badge() != "verified,code-not-applicable" {
		t.Fatalf("no resources: %+v", tr)
	}

	// The anchor commit disappears (history rewritten, or a different clone).
	if err := os.RemoveAll(filepath.Join(repo, ".git")); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(dir, "REWRITTEN"), "new history\n")
	repoAt(t, dir, "git@github.com:Acme/api.git")
	s := openAt(t, repo)
	tr := trustOf(t, s, entry.ID)
	if tr.CodeFreshness != FreshUnknown || tr.FreshnessCode != UnknownAnchor || !strings.Contains(tr.FreshnessReason, "not available in") {
		t.Fatalf("missing anchor: %+v", tr)
	}
	if tr.Stale || tr.StaleReason != "" || tr.State != StateVerified || !tr.Trusted() {
		t.Fatalf("unknown alone must not set the legacy stale flag or change trust: %+v", tr)
	}
	if tr.Badge() != "verified,unknown-freshness" || tr.Ack() != "verified,unknown-freshness" {
		t.Fatalf("badge %q ack %q", tr.Badge(), tr.Ack())
	}
	if w := trustWeight(tr); math.Abs(w-1.15*0.9) > 1e-9 {
		t.Fatalf("unknown weight: %v", w)
	}
	// Still searchable, and still eligible for --trusted.
	hits, _ := s.Search(SearchQuery{Text: "idempotency retries", TrustedOnly: true})
	if len(hits) != 1 || hits[0].Trust.CodeFreshness != FreshUnknown {
		t.Fatalf("trusted search: %+v", hits)
	}
	if out := s.RenderHits(hits, SearchQuery{Text: "idempotency retries", NoDocs: true}); !strings.Contains(out, "[verified,unknown-freshness] Retries need") {
		t.Fatalf("hit line:\n%s", out)
	}
	n, _ := s.Get(entry.ID)
	if out := s.RenderNode(n); !strings.Contains(out, "code freshness: unknown - anchor commit") || !strings.Contains(out, "compared with: "+repo) || !strings.Contains(out, "(unknown freshness:") {
		t.Fatalf("show:\n%s", out)
	}
	st, _ := s.StatusFor(StatusOptions{})
	if st.Stale != 0 || st.UnknownFreshness != 1 || len(st.UnknownIDs) != 1 || st.UnknownReasons[UnknownAnchor] != 1 || st.CodeFreshness[FreshNotApplicable] != 1 {
		t.Fatalf("status: %+v", st)
	}
	if b := st.Brief(); strings.Count(b, "\n") != 0 || !strings.Contains(b, "1 entr(ies) in api + global whose code could not be compared") || strings.Contains(b, entry.ID) {
		t.Fatalf("brief: %q", b)
	}
	if out := RenderStatus(st); !strings.Contains(out, "code freshness: unchanged 0, changed 0, unknown 1, not applicable 1") || !strings.Contains(out, "1 whose anchor commit is not in the checkout") {
		t.Fatalf("status text:\n%s", out)
	}

	// Past its deadline too: both are reported, the penalty applies once.
	s.Now = func() time.Time { return time.Date(2027, 6, 1, 0, 0, 0, 0, time.UTC) }
	tr = trustOf(t, s, entry.ID)
	if !tr.Overdue || !tr.Stale || tr.CodeFreshness != FreshUnknown || !strings.Contains(tr.StaleReason, "past stale_after") {
		t.Fatalf("overdue and unknown: %+v", tr)
	}
	if tr.Badge() != "verified,unknown-freshness,stale" {
		t.Fatalf("badge: %s", tr.Badge())
	}
	if w := trustWeight(tr); math.Abs(w-1.15*0.8) > 1e-9 {
		t.Fatalf("overdue+unknown weight must be 0.8 once: %v", w)
	}
	if st, _ := s.StatusFor(StatusOptions{}); st.Stale != 1 || st.Overdue != 1 || st.UnknownFreshness != 1 {
		t.Fatalf("status when overdue: %+v", st)
	}

	// An entry with resources but no recorded commit is unknown as well.
	h := openAt(t, t.TempDir())
	if _, err := h.Add(AddInput{Type: "learning", Title: "Nightly export needs 4 GB", Description: "Batch exceeds the default heap.", Resources: []string{"jobs/**"}, Project: "ops", NewProject: true, Actor: claude}); err != nil {
		t.Fatal(err)
	}
	if tr := trustOf(t, h, "ops/learnings/nightly-export-needs-4-gb"); tr.CodeFreshness != FreshUnknown || tr.FreshnessCode != UnknownNoAnchor {
		t.Fatalf("no anchor: %+v", tr)
	}
}

func TestBriefBudget(t *testing.T) {
	r := &StatusReport{
		Scope: "api + global", ReviewsForMe: []string{"r1"}, ReviewsChanges: []string{"r2"}, Disputed: []string{"d1"},
		PendingRules: []string{"p1"}, Stale: 2, StaleIDs: []string{"s1", "s2"}, UnknownFreshness: 40,
	}
	b := r.Brief()
	lines := strings.Split(b, "\n")
	if len(lines) != 3 || !strings.Contains(lines[0], "waiting for you") || !strings.Contains(lines[2], "disputed") || !strings.HasSuffix(lines[2], "(+3 more: kb status)") {
		t.Fatalf("brief:\n%s", b)
	}
	// Lower priority lines appear once the higher ones are gone, the
	// aggregate unknown line last and without ids.
	r = &StatusReport{Scope: "api + global", PendingRules: []string{"p1"}, Stale: 1, StaleIDs: []string{"s1"}, UnknownFreshness: 40}
	lines = strings.Split(r.Brief(), "\n")
	if len(lines) != 3 || !strings.Contains(lines[0], "proposed rule") || !strings.Contains(lines[1], "stale") || !strings.Contains(lines[2], "40 entr(ies)") {
		t.Fatalf("brief order:\n%s", r.Brief())
	}
}

func TestOwnershipInMultiRepoProject(t *testing.T) {
	newHub(t)
	base := t.TempDir()
	const beID, feID, forkID = "github.com/acme/backend", "github.com/acme/frontend", "github.com/fork/backend"
	backend := repoAt(t, filepath.Join(base, "backend"), "git@github.com:Acme/backend.git")
	frontend := repoAt(t, filepath.Join(base, "frontend"), "git@github.com:Acme/frontend.git")
	b := openAt(t, backend)
	if err := b.LinkProject("shop"); err != nil {
		t.Fatal(err)
	}
	if err := openAt(t, frontend).LinkProject("shop"); err != nil {
		t.Fatal(err)
	}
	b = openAt(t, backend)
	f := openAt(t, frontend)
	if got := strings.Join(f.ProjectRepos("shop"), " "); got != beID+" "+feID {
		t.Fatalf("repos: %s", got)
	}
	be := anchored(t, b, "Orders API paginates with cursors", "pkg/**")
	fe := anchored(t, f, "Cart page debounces quantity changes", "pkg/**")

	// Both repositories have pkg/main.go. A change in the frontend must not
	// make the backend's entry look changed, from wherever kb runs.
	commitFile(t, frontend, "pkg/main.go", "package main\n// frontend change\n")
	for name, dir := range map[string]string{"outside": t.TempDir(), "frontend": frontend, "backend": backend} {
		s := openAt(t, dir)
		tr := trustOf(t, s, be.ID)
		if tr.CodeFreshness != FreshUnchanged || tr.Baseline.Repo != beID || tr.Baseline.Path != backend || !strings.Contains(tr.Baseline.Selection, "anchor commit found only in "+beID) {
			t.Fatalf("%s: backend entry: %+v baseline %+v", name, tr, tr.Baseline)
		}
		if tr := trustOf(t, s, fe.ID); tr.CodeFreshness != FreshChanged || tr.Baseline.Repo != feID {
			t.Fatalf("%s: frontend entry: %+v baseline %+v", name, tr, tr.Baseline)
		}
	}

	// An import stamps the HEAD of whatever checkout it ran in; that commit
	// is not evidence of where the claim belongs.
	imp, err := b.Add(AddInput{Type: "learning", Title: "Checkout flow retries payment twice", Description: "Saved from an old session.", Resources: []string{"pkg/**"}, Evidence: "saved in codex's memory", Actor: codex,
		Sources: []okf.Source{{ID: "memory-codex", Resource: "~/.codex/memories/MEMORY.md"}}})
	if err != nil || imp.Node.Meta().Generated.Commit == "" {
		t.Fatalf("import: %v %+v", err, imp)
	}
	out := openAt(t, t.TempDir())
	if tr := trustOf(t, out, imp.Node.ID); tr.CodeFreshness != FreshUnknown || tr.FreshnessCode != UnknownAmbiguousRep || !strings.Contains(tr.FreshnessReason, "stamped by an import") {
		t.Fatalf("import-created anchor: %+v", tr)
	}
	// Once someone checks it against the code, that check's commit counts.
	if _, _, err := b.Verify(VerifyInput{Ref: imp.Node.ID, Result: ResultConfirmed, Evidence: "read pkg/main.go", Actor: claude}); err != nil {
		t.Fatal(err)
	}
	if tr := trustOf(t, openAt(t, t.TempDir()), imp.Node.ID); tr.CodeFreshness != FreshUnchanged || tr.Baseline.Repo != beID {
		t.Fatalf("checked import: %+v baseline %+v", tr, tr.Baseline)
	}

	// A fork shares the backend's history: the anchor commit is in both, so
	// containment alone does not name the owner.
	fork := filepath.Join(base, "fork")
	runGit(t, base, "clone", "-q", backend, fork)
	runGit(t, fork, "remote", "set-url", "origin", "git@github.com:Fork/backend.git")
	fork = realPath(fork)
	commitFile(t, fork, "pkg/main.go", "package main\n// fork change\n")
	if err := openAt(t, fork).LinkProject("shop"); err != nil {
		t.Fatal(err)
	}
	out = openAt(t, t.TempDir())
	tr := trustOf(t, out, be.ID)
	if tr.CodeFreshness != FreshUnknown || tr.FreshnessCode != UnknownAmbiguousRep || !strings.Contains(tr.FreshnessReason, beID) || !strings.Contains(tr.FreshnessReason, forkID) {
		t.Fatalf("shared history must be ambiguous outside: %+v", tr)
	}
	// Inside one of them the comparison is contextual, and labelled so.
	if tr := trustOf(t, openAt(t, fork), be.ID); tr.CodeFreshness != FreshChanged || tr.Baseline.Repo != forkID || !strings.Contains(tr.Baseline.Selection, "current checkout") {
		t.Fatalf("inside the fork: %+v baseline %+v", tr, tr.Baseline)
	}
	if tr := trustOf(t, openAt(t, backend), be.ID); tr.CodeFreshness != FreshUnchanged || tr.Baseline.Repo != beID {
		t.Fatalf("inside the backend: %+v baseline %+v", tr, tr.Baseline)
	}
	// An explicit checkout gives the same answer from any directory.
	for _, dir := range []string{t.TempDir(), fork, frontend} {
		s := openAt(t, dir)
		if err := s.SetBaseline(backend, ""); err != nil {
			t.Fatal(err)
		}
		if tr := trustOf(t, s, be.ID); tr.CodeFreshness != FreshUnchanged || tr.Baseline.Path != backend || !strings.Contains(tr.Baseline.Selection, "explicit --checkout") {
			t.Fatalf("explicit checkout from %s: %+v baseline %+v", dir, tr, tr.Baseline)
		}
	}

	// A repository that is not on this machine: its entry's commit is found
	// nowhere, which is "unknown", not proof that another repository owns it.
	if err := os.RemoveAll(frontend); err != nil {
		t.Fatal(err)
	}
	tr = trustOf(t, openAt(t, t.TempDir()), fe.ID)
	if tr.CodeFreshness != FreshUnknown || tr.FreshnessCode != UnknownNoCheckout || !strings.Contains(tr.FreshnessReason, "not checked out: "+feID) {
		t.Fatalf("missing repository: %+v", tr)
	}
	// Writes from outside a multi-repository project record no commit
	// rather than the HEAD of a guessed repository.
	res, err := openAt(t, t.TempDir()).Add(AddInput{Type: "fact", Title: "Shop deploys on Tuesdays", Description: "Release train.", Project: "shop", Actor: claude})
	if err != nil || res.Node.Meta().Generated.Commit != "" {
		t.Fatalf("outside write: %v commit=%q", err, res.Node.Meta().Generated.Commit)
	}
}

func TestExplicitRefBaseline(t *testing.T) {
	newHub(t)
	base := t.TempDir()
	api := repoAt(t, filepath.Join(base, "api"), "git@github.com:Acme/api.git")
	other := repoAt(t, filepath.Join(base, "other"), "git@github.com:Acme/other.git")
	entry := anchored(t, openAt(t, api), "Handlers must check the tenant header", "pkg/**")
	runGit(t, api, "tag", "v1")
	commitFile(t, api, "pkg/main.go", "package main\n// after v1\n")

	if tr := trustOf(t, openAt(t, other), entry.ID); tr.CodeFreshness != FreshChanged || tr.Baseline.Ref != "" {
		t.Fatalf("HEAD baseline: %+v", tr)
	}
	var heads []string
	for _, dir := range []string{t.TempDir(), other, api} {
		s := openAt(t, dir)
		if err := s.SetBaseline(api, "v1"); err != nil {
			t.Fatal(err)
		}
		tr := trustOf(t, s, entry.ID)
		if tr.CodeFreshness != FreshUnchanged || tr.Stale || tr.Baseline.Ref != "v1" || tr.Baseline.Path != api {
			t.Fatalf("--ref v1 from %s: %+v baseline %+v", dir, tr, tr.Baseline)
		}
		heads = append(heads, tr.Baseline.Head)
	}
	if heads[0] == "" || heads[0] != heads[1] || heads[1] != heads[2] {
		t.Fatalf("explicit baseline differs by directory: %v", heads)
	}
	s := openAt(t, other)
	if err := s.SetBaseline("", "no-such-ref"); err != nil {
		t.Fatal(err)
	}
	if tr := trustOf(t, s, entry.ID); tr.CodeFreshness != FreshUnknown || tr.FreshnessCode != UnknownRef {
		t.Fatalf("unresolved ref: %+v", tr)
	}
	if err := s.SetBaseline(t.TempDir(), ""); err == nil {
		t.Fatal("--checkout outside git accepted")
	}
}
