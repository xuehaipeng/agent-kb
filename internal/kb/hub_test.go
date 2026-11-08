package kb

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

// newHub creates a git-backed hub at $KB_HOME and returns its path.
func newHub(t *testing.T) string {
	t.Helper()
	hub := filepath.Join(t.TempDir(), "hub")
	write(t, filepath.Join(hub, ConfigFile), HubConfigYAML())
	write(t, filepath.Join(hub, ".gitignore"), LocalDir+"/\n")
	runGit(t, hub, "init", "-q", "-b", "main")
	runGit(t, hub, "add", "-A")
	runGit(t, hub, "commit", "-qm", "hub")
	t.Setenv("KB_HOME", hub)
	t.Setenv("KB_ROOT", "")
	t.Setenv("GIT_AUTHOR_NAME", "t")
	t.Setenv("GIT_AUTHOR_EMAIL", "t@t")
	t.Setenv("GIT_COMMITTER_NAME", "t")
	t.Setenv("GIT_COMMITTER_EMAIL", "t@t")
	return hub
}

// newCodeRepo creates a repo with an origin remote and one file.
func newCodeRepo(t *testing.T, remote string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "repo")
	write(t, filepath.Join(dir, "pkg", "main.go"), "package main\n")
	runGit(t, dir, "init", "-q", "-b", "main")
	runGit(t, dir, "remote", "add", "origin", remote)
	runGit(t, dir, "add", "-A")
	runGit(t, dir, "commit", "-qm", "init")
	return dir
}

func openAt(t *testing.T, dir string) *Store {
	t.Helper()
	s, err := DiscoverAt("", dir)
	if err != nil {
		t.Fatal(err)
	}
	s.Now = func() time.Time { return time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC) }
	return s
}

func TestNormalizeRemote(t *testing.T) {
	for in, want := range map[string]string{
		"git@github.com:AtlasCloudTeam/aiproxy.git":     "github.com/atlascloudteam/aiproxy",
		"https://github.com/AtlasCloudTeam/aiproxy":     "github.com/atlascloudteam/aiproxy",
		"https://x:tok@github.com/o/r.git":              "github.com/o/r",
		"ssh://git@gitlab.example.com:2222/team/kubedl": "gitlab.example.com/team/kubedl",
	} {
		if got := NormalizeRemote(in); got != want {
			t.Errorf("NormalizeRemote(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestHubProjectsScopeAndStaleness(t *testing.T) {
	hub := newHub(t)
	aiproxy := newCodeRepo(t, "git@github.com:AtlasCloudTeam/aiproxy.git")
	kubedl := newCodeRepo(t, "https://github.com/AtlasCloudTeam/kubedl")

	a := openAt(t, aiproxy)
	if !a.Hub || a.Project != "aiproxy" || a.RepoKey != "github.com/atlascloudteam/aiproxy" {
		t.Fatalf("project detection: hub=%v project=%q key=%q", a.Hub, a.Project, a.RepoKey)
	}
	res, err := a.Add(AddInput{Type: "learning", Title: "Upstream retries need idempotency keys", Description: "Retries duplicate jobs without keys.", Resources: []string{"pkg/**"}, Evidence: "reproduced with pkg/main_test.go", Actor: claude})
	if err != nil {
		t.Fatal(err)
	}
	if res.Node.ID != "aiproxy/learnings/upstream-retries-need-idempotency-keys" || !strings.HasPrefix(res.Node.Path, filepath.Join(realPath(hub), "projects", "aiproxy")) {
		t.Fatalf("placement: %s %s", res.Node.ID, res.Node.Path)
	}
	pf := a.ProjectFiles()["aiproxy"]
	if len(pf.Repos) != 1 || pf.Repos[0] != "github.com/atlascloudteam/aiproxy" {
		t.Fatalf("project.yaml: %+v", pf)
	}
	// Autocommit recorded the write in the hub's history.
	if log := runGit(t, hub, "log", "--oneline"); !strings.Contains(log, "kb: add aiproxy/learnings/upstream-retries") {
		t.Fatalf("hub log:\n%s", log)
	}
	if _, err := a.Add(AddInput{Type: "fact", Title: "aiproxy calls kubedl job API with tenant header", Description: "X-Tenant is required.", Project: "global", Projects: []string{"aiproxy", "kubedl"}, Actor: claude}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Add(AddInput{Type: "convention", Title: "Commit messages use imperative mood", Description: "Applies to all repos.", Project: "global", Actor: claude}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Add(AddInput{Type: "fact", Title: "aiproxy only fact in global", Description: "Scoped to aiproxy.", Project: "global", Projects: []string{"aiproxy"}, Actor: claude}); err != nil {
		t.Fatal(err)
	}

	k := openAt(t, kubedl)
	if k.Project != "kubedl" {
		t.Fatalf("kubedl project: %q", k.Project)
	}
	ids := func(hits []Hit) string {
		var out []string
		for _, h := range hits {
			out = append(out, h.Node.ID)
		}
		return strings.Join(out, " ")
	}
	hits, _ := k.Search(SearchQuery{Text: "idempotency retries tenant header imperative commit aiproxy"})
	got := ids(hits)
	if strings.Contains(got, "aiproxy/learnings") || strings.Contains(got, "aiproxy-only") || !strings.Contains(got, "global/facts/aiproxy-calls-kubedl") || !strings.Contains(got, "global/conventions/commit-messages") {
		t.Fatalf("default scope from kubedl: %s", got)
	}
	hits, _ = k.Search(SearchQuery{Text: "idempotency", AllProjects: true})
	if !strings.Contains(ids(hits), "aiproxy/learnings/upstream-retries") {
		t.Fatalf("--all: %s", ids(hits))
	}
	if n, err := k.Get("aiproxy/learnings/upstream-retries-need-idempotency-keys"); err != nil || n.Project != "aiproxy" {
		t.Fatalf("cross-project get: %v", err)
	}
	// Staleness for another project's entry works through the registry.
	write(t, filepath.Join(aiproxy, "pkg", "main.go"), "package main\n// retry change\n")
	runGit(t, aiproxy, "commit", "-qam", "change")
	k.Refresh()
	n, _ := k.Get("aiproxy/learnings/upstream-retries-need-idempotency-keys")
	if tr := k.Trust(n); !tr.Stale || tr.State != StateVerified {
		t.Fatalf("registry staleness: %+v", tr)
	}

	// Outside any repo: all projects in scope, writes need a target.
	home := openAt(t, t.TempDir())
	if home.Project != "" {
		t.Fatalf("no project expected, got %q", home.Project)
	}
	if hits, _ := home.Search(SearchQuery{Text: "idempotency"}); len(hits) != 1 {
		t.Fatalf("outside repo search: %s", ids(hits))
	}
	if _, err := home.Add(AddInput{Type: "fact", Title: "x", Description: "y", Actor: claude}); err == nil || !strings.Contains(err.Error(), "--project") {
		t.Fatalf("want target error, got %v", err)
	}
	// Inside the hub repo itself there is no project either.
	if openAt(t, hub).Project != "" {
		t.Fatal("hub repo must not be treated as a project")
	}

	// Indexes and lint across the hub.
	if _, err := a.WriteIndex(); err != nil {
		t.Fatal(err)
	}
	root, _ := os.ReadFile(filepath.Join(hub, "index.md"))
	if !strings.Contains(string(root), "[aiproxy (github.com/atlascloudteam/aiproxy)](projects/aiproxy/index.md)") || !strings.Contains(string(root), "[global](global/index.md)") {
		t.Fatalf("hub index:\n%s", root)
	}
	issues, _ := a.Lint(LintOptions{})
	for _, i := range issues {
		if i.Severity == "error" {
			t.Errorf("lint: %s", i)
		}
	}
	st, _ := k.StatusFor(StatusOptions{})
	if st.Scope != "kubedl + global" || st.Total != 2 {
		t.Fatalf("status scope: %+v", st)
	}
}

func TestHubCollisionAndLink(t *testing.T) {
	newHub(t)
	up := newCodeRepo(t, "https://github.com/containerd/containerd")
	fork := newCodeRepo(t, "https://github.com/xuehaipeng/containerd")
	frontend := newCodeRepo(t, "https://github.com/AtlasCloudTeam/skyfable-web")

	u := openAt(t, up)
	if _, err := u.Add(AddInput{Type: "fact", Title: "Snapshotter is overlayfs", Description: "Default snapshotter.", Actor: claude}); err != nil {
		t.Fatal(err)
	}
	f := openAt(t, fork)
	if f.Project != "xuehaipeng-containerd" {
		t.Fatalf("collision id: %q", f.Project)
	}
	// Link the fork to the upstream project so knowledge is shared.
	if err := f.LinkProject("containerd"); err != nil {
		t.Fatal(err)
	}
	if p := openAt(t, fork).Project; p != "containerd" {
		t.Fatalf("after link: %q", p)
	}
	// A second repo can join a project (frontend + backend).
	w := openAt(t, frontend)
	if err := w.LinkProject("skyfable"); err != nil {
		t.Fatal(err)
	}
	if p := openAt(t, frontend).Project; p != "skyfable" {
		t.Fatalf("frontend project: %q", p)
	}
}

func TestHubClaimsRepoLessProject(t *testing.T) {
	newHub(t)
	// Hermes, outside any repo, records knowledge for "kubedl" first.
	h := openAt(t, t.TempDir())
	if _, err := h.Add(AddInput{Type: "learning", Title: "Nightly export OOMs", Description: "Batch exceeds heap.", Project: "kubedl", Actor: Actor{Producer: "hermes"}}); err != nil {
		t.Fatal(err)
	}
	repo := newCodeRepo(t, "git@github.com:AtlasCloudTeam/kubedl.git")
	k := openAt(t, repo)
	if k.Project != "kubedl" {
		t.Fatalf("repo should claim the repo-less project, got %q", k.Project)
	}
	if _, err := k.Add(AddInput{Type: "fact", Title: "Jobs run on volcano", Description: "Scheduler is volcano.", Actor: claude}); err != nil {
		t.Fatal(err)
	}
	if pf := k.ProjectFiles()["kubedl"]; len(pf.Repos) != 1 || pf.Repos[0] != "github.com/atlascloudteam/kubedl" {
		t.Fatalf("repo not recorded: %+v", pf)
	}
}
