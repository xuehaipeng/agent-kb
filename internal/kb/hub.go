package kb

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// ProjectFile describes a hub project: which repositories belong to it.
// One project may span several repos (backend + frontend), and forks or
// renamed remotes can be linked to the same project.
type ProjectFile struct {
	Name        string   `yaml:"name"`
	Description string   `yaml:"description,omitempty"`
	Repos       []string `yaml:"repos"`
}

const projectFileName = "project.yaml"

// registry maps projects to local checkouts on this machine (gitignored),
// so staleness can be checked even when kb runs outside the repo.
type registry struct {
	Projects map[string][]string `json:"projects"`
}

// NormalizeRemote turns a git remote URL into a stable key:
// git@github.com:Org/Repo.git and https://github.com/org/repo -> github.com/org/repo.
func NormalizeRemote(url string) string {
	u := strings.TrimSpace(url)
	if u == "" {
		return ""
	}
	for _, p := range []string{"https://", "http://", "ssh://", "git://", "git+ssh://"} {
		u = strings.TrimPrefix(u, p)
	}
	if i := strings.Index(u, "@"); i >= 0 && (strings.Index(u, "/") < 0 || i < strings.Index(u, "/")) {
		u = u[i+1:]
	}
	// scp-like host:path
	if i := strings.Index(u, ":"); i >= 0 && (strings.Index(u, "/") < 0 || i < strings.Index(u, "/")) {
		host, rest := u[:i], u[i+1:]
		// drop a port number
		if j := strings.Index(rest, "/"); j > 0 && isDigits(rest[:j]) {
			rest = rest[j+1:]
		}
		u = host + "/" + rest
	}
	u = strings.TrimSuffix(strings.TrimSuffix(u, "/"), ".git")
	return strings.ToLower(u)
}

func isDigits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return s != ""
}

// attachProject detects the repo containing cwd and maps it to a project.
func (s *Store) attachProject(cwd string) {
	if cwd == "" {
		return
	}
	top, err := Git(cwd, "rev-parse", "--show-toplevel")
	if err != nil {
		return
	}
	top = realPath(top)
	if hubTop, err := Git(s.Dir, "rev-parse", "--show-toplevel"); err == nil && realPath(hubTop) == top {
		return // inside the hub repo itself
	}
	key := ""
	if url, err := Git(top, "remote", "get-url", "origin"); err == nil {
		key = NormalizeRemote(url)
	} else if remotes, err := Git(top, "remote"); err == nil && remotes != "" {
		if url, err := Git(top, "remote", "get-url", strings.Fields(remotes)[0]); err == nil {
			key = NormalizeRemote(url)
		}
	}
	if key == "" {
		key = "path:" + top
	}
	s.CodeRoot = top
	s.RepoKey = key
	s.Project = s.projectForRepo(key, top)
	s.registerCheckout(s.Project, top)
}

// projectForRepo finds the project whose project.yaml lists the repo, or
// derives an id from the repo name (owner-name on collision).
func (s *Store) projectForRepo(key, top string) string {
	projects := s.ProjectFiles()
	ids := make([]string, 0, len(projects))
	for id := range projects {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		for _, r := range projects[id].Repos {
			if NormalizeRemote(r) == key || r == key {
				return id
			}
		}
	}
	name := key
	if strings.HasPrefix(key, "path:") {
		name = filepath.Base(top)
	}
	parts := strings.Split(name, "/")
	id := Slugify(parts[len(parts)-1])
	if id == "" || id == GlobalProject {
		id = "repo-" + id
	}
	// A same-named project is only "taken" if it already belongs to other
	// repos; one created without repos (e.g. by Hermes with -p <name>) is
	// claimed by the repo of that name.
	if pf, taken := projects[id]; taken && len(pf.Repos) > 0 && len(parts) >= 2 {
		id = Slugify(parts[len(parts)-2] + "-" + parts[len(parts)-1])
	}
	return id
}

// ProjectFiles reads every hub project's project.yaml (missing files yield
// a ProjectFile with just the name).
func (s *Store) ProjectFiles() map[string]ProjectFile {
	out := map[string]ProjectFile{}
	if !s.Hub {
		return out
	}
	entries, _ := os.ReadDir(filepath.Join(s.Dir, "projects"))
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		pf := ProjectFile{Name: e.Name()}
		if data, err := os.ReadFile(filepath.Join(s.Dir, "projects", e.Name(), projectFileName)); err == nil {
			_ = yaml.Unmarshal(data, &pf)
		}
		out[e.Name()] = pf
	}
	return out
}

// ensureProject creates the project directory and project.yaml on first
// write, recording the current repo remote.
func (s *Store) ensureProject(id string) error {
	if !s.Hub || id == GlobalProject {
		return os.MkdirAll(s.BundleDir(id), 0o755)
	}
	if err := validProjectID(id); err != nil {
		return err
	}
	dir := s.BundleDir(id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	p := filepath.Join(dir, projectFileName)
	pf := ProjectFile{Name: id}
	if data, err := os.ReadFile(p); err == nil {
		_ = yaml.Unmarshal(data, &pf)
		if len(pf.Repos) > 0 {
			return nil
		}
	}
	// Record the current repo as the project's first repository.
	if id == s.Project && s.RepoKey != "" && !strings.HasPrefix(s.RepoKey, "path:") {
		pf.Repos = []string{s.RepoKey}
	}
	if fileExists(p) && len(pf.Repos) == 0 {
		return nil
	}
	return writeProjectFile(p, pf)
}

func writeProjectFile(p string, pf ProjectFile) error {
	data, err := yaml.Marshal(pf)
	if err != nil {
		return err
	}
	_, err = WriteFileIfChanged(p, append([]byte("# Repositories whose knowledge lives in this project (kb project link adds more).\n"), data...))
	return err
}

func validProjectID(id string) error {
	if id == "" || strings.ContainsAny(id, "/\\ ") || strings.HasPrefix(id, ".") || id == "projects" {
		return fmt.Errorf("invalid project id %q", id)
	}
	return nil
}

// LinkProject maps the current repo to project id (creating it if needed),
// e.g. to group several repos or a fork under one project.
func (s *Store) LinkProject(id string) error {
	if !s.Hub {
		return errors.New("projects exist only in hub mode (kb hub init)")
	}
	if s.RepoKey == "" {
		return errors.New("not inside a git repository")
	}
	if err := validProjectID(id); err != nil {
		return err
	}
	unlock, err := s.Lock()
	if err != nil {
		return err
	}
	defer unlock()
	dir := s.BundleDir(id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	p := filepath.Join(dir, projectFileName)
	pf := ProjectFile{Name: id}
	if data, err := os.ReadFile(p); err == nil {
		_ = yaml.Unmarshal(data, &pf)
	}
	// Remove the repo from any other project first.
	for other, opf := range s.ProjectFiles() {
		if other == id {
			continue
		}
		kept := opf.Repos[:0]
		changed := false
		for _, r := range opf.Repos {
			if NormalizeRemote(r) == s.RepoKey {
				changed = true
				continue
			}
			kept = append(kept, r)
		}
		if changed {
			opf.Repos = kept
			if err := writeProjectFile(filepath.Join(s.BundleDir(other), projectFileName), opf); err != nil {
				return err
			}
		}
	}
	if !containsStr(pf.Repos, s.RepoKey) {
		pf.Repos = append(pf.Repos, s.RepoKey)
	}
	if err := writeProjectFile(p, pf); err != nil {
		return err
	}
	s.Project = id
	s.registerCheckout(id, s.CodeRoot)
	s.Invalidate()
	return s.recordChange("project-link", id, Actor{Producer: "kb"}, s.RepoKey)
}

func (s *Store) loadRegistry() *registry {
	if s.registry != nil {
		return s.registry
	}
	r := &registry{Projects: map[string][]string{}}
	if data, err := os.ReadFile(s.LocalPath("registry.json")); err == nil {
		_ = json.Unmarshal(data, r)
		if r.Projects == nil {
			r.Projects = map[string][]string{}
		}
	}
	s.registry = r
	return r
}

func (s *Store) registerCheckout(project, path string) {
	if project == "" || path == "" {
		return
	}
	r := s.loadRegistry()
	if containsStr(r.Projects[project], path) {
		return
	}
	r.Projects[project] = append([]string{path}, r.Projects[project]...)
	if err := os.MkdirAll(s.LocalPath(), 0o755); err != nil {
		return
	}
	data, _ := json.MarshalIndent(r, "", "  ")
	_, _ = WriteFileIfChanged(s.LocalPath("registry.json"), data)
}

// CheckoutFor returns a local work tree for project on this machine, or "".
func (s *Store) CheckoutFor(project string) string {
	if !s.Hub {
		return s.CodeRoot
	}
	if project == s.Project && s.CodeRoot != "" {
		return s.CodeRoot
	}
	for _, p := range s.loadRegistry().Projects[project] {
		if fileExists(filepath.Join(p, ".git")) {
			return p
		}
	}
	return ""
}

// codeRootFor returns the work tree a node's resources refer to.
func (s *Store) codeRootFor(n *Node) string {
	if n.Project == GlobalProject {
		return ""
	}
	return s.CheckoutFor(n.Project)
}

func (s *Store) autocommit() bool {
	return s.Cfg.Hub.Autocommit == nil || *s.Cfg.Hub.Autocommit
}

// commitHub commits all hub changes; best effort (a failure leaves the
// change on disk, and 'kb status' reports uncommitted knowledge).
func (s *Store) commitHub(msg string) {
	top, err := Git(s.Dir, "rev-parse", "--show-toplevel")
	if err != nil {
		return
	}
	if _, err := Git(top, "add", "-A", "--", "."); err != nil {
		return
	}
	if _, err := Git(top, "diff", "--cached", "--quiet"); err == nil {
		return
	}
	_, _ = Git(top, "commit", "--quiet", "--no-verify", "-m", msg)
}

// KnowledgeTop is the git work tree that holds the knowledge files (the hub
// repo, or the code repo in repo mode).
func (s *Store) KnowledgeTop() string {
	top, err := Git(s.Dir, "rev-parse", "--show-toplevel")
	if err != nil {
		return ""
	}
	return realPath(top)
}

// SyncState reports knowledge not yet shared: uncommitted files and commits
// not pushed to the upstream.
func (s *Store) SyncState() (uncommitted int, unpushed int, upstream string) {
	top := s.KnowledgeTop()
	if top == "" {
		return 0, 0, ""
	}
	rel := "."
	if r, err := filepath.Rel(top, s.Dir); err == nil {
		rel = filepath.ToSlash(r)
	}
	if out, err := Git(top, "status", "--porcelain", "--", rel); err == nil && out != "" {
		uncommitted = len(strings.Split(out, "\n"))
	}
	upstream, _ = Git(top, "rev-parse", "--abbrev-ref", "--symbolic-full-name", "@{upstream}")
	if upstream != "" {
		if out, err := Git(top, "rev-list", "--count", "@{upstream}..HEAD", "--", rel); err == nil {
			fmt.Sscan(out, &unpushed)
		}
	}
	return uncommitted, unpushed, upstream
}

// EnsureProject creates a project's directory and project.yaml if missing.
func (s *Store) EnsureProject(id string) error { return s.ensureProject(id) }

// RecordChange logs a change, regenerates indexes and autocommits (hub).
func (s *Store) RecordChange(op, id string, actor Actor, detail string) error {
	return s.recordChange(op, id, actor, detail)
}

// ForDir returns a store on the same hub with the project of dir attached.
func (s *Store) ForDir(dir string) *Store {
	c := &Store{Dir: s.Dir, Hub: s.Hub, Cfg: s.Cfg, Now: s.Now, gits: map[string]*gitCache{}}
	if !s.Hub {
		c.CodeRoot = s.CodeRoot
		return c
	}
	c.attachProject(dir)
	return c
}
