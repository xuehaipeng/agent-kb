package kb

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Code freshness says whether the code an entry depends on still matches
// what was checked. It is separate from trust (who confirmed the claim) and
// from the stale_after deadline, and it is computed for live output only:
// it depends on this machine's checkouts, so it never reaches a file.
const (
	FreshUnchanged     = "unchanged"      // compared: no resource changed since the anchor
	FreshChanged       = "changed"        // compared: a resource changed since the anchor
	FreshUnknown       = "unknown"        // could not be compared; never read this as fresh
	FreshNotApplicable = "not_applicable" // the claim does not depend on code
)

// Why freshness is unknown (machine-readable; the reason text has details).
const (
	UnknownNoAnchor     = "no_anchor"            // no commit recorded with the claim
	UnknownNoCheckout   = "no_checkout"          // the repository is not on this machine
	UnknownAmbiguousCo  = "ambiguous_checkout"   // several clones and no preference
	UnknownAmbiguousRep = "ambiguous_repository" // multi-repo project, owner not established
	UnknownAnchor       = "anchor_unresolved"    // the anchor commit is not in the checkout
	UnknownRef          = "ref_unresolved"       // the explicit --ref does not resolve
)

// Baseline is the code state an entry was compared against, and why that
// checkout was the one chosen.
type Baseline struct {
	Repo      string `json:"repo,omitempty"`      // repository identity (normalized remote)
	Path      string `json:"path,omitempty"`      // checkout on this machine
	Ref       string `json:"ref,omitempty"`       // explicit --ref, when given
	Head      string `json:"head,omitempty"`      // commit compared against
	Selection string `json:"selection,omitempty"` // rule that chose the checkout
}

// Checkout is a local work tree of one of a project's repositories.
type Checkout struct {
	Path      string
	Repo      string // repository identity; "" when it could not be read
	CommonDir string // git common directory: equal for worktrees of one clone
	Exists    bool
	Preferred bool
}

type checkoutInfo struct {
	Repo      string `json:"repo,omitempty"`
	CommonDir string `json:"common_dir,omitempty"`
}

// remoteKey is the normalized origin (or first) remote of a work tree, or "".
func remoteKey(top string) string {
	if url, err := Git(top, "remote", "get-url", "origin"); err == nil {
		return NormalizeRemote(url)
	}
	if remotes, err := Git(top, "remote"); err == nil && remotes != "" {
		if url, err := Git(top, "remote", "get-url", strings.Fields(remotes)[0]); err == nil {
			return NormalizeRemote(url)
		}
	}
	return ""
}

// repoIDOf normalizes a repository reference from project.yaml.
func repoIDOf(r string) string {
	if strings.HasPrefix(r, "path:") {
		return r
	}
	return NormalizeRemote(r)
}

// commonDirOf returns the git common directory of a work tree: the same for
// every worktree of one clone, different for independent clones.
func commonDirOf(top string) string {
	out, err := Git(top, "rev-parse", "--git-common-dir")
	if err != nil || out == "" {
		return ""
	}
	if !filepath.IsAbs(out) {
		out = filepath.Join(top, out)
	}
	return realPath(out)
}

// inspectCheckout reads a work tree's top-level and identity from git. A
// repository without a remote is identified by its primary worktree.
func inspectCheckout(path string) (top string, info checkoutInfo, ok bool) {
	t, err := Git(path, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", checkoutInfo{}, false
	}
	top = realPath(t)
	info.CommonDir = commonDirOf(top)
	info.Repo = remoteKey(top)
	if info.Repo == "" {
		info.Repo = localRepoID(top, info.CommonDir)
	}
	return top, info, true
}

func localRepoID(top, commonDir string) string {
	if filepath.Base(commonDir) == ".git" {
		return "path:" + filepath.Dir(commonDir)
	}
	return "path:" + top
}

// mainWorktree returns the primary worktree of the clone that path belongs
// to (from git's own metadata), or "" when the clone is bare.
func mainWorktree(path string) string {
	out, err := Git(path, "worktree", "list", "--porcelain")
	if err != nil {
		return ""
	}
	main := ""
	for _, l := range strings.Split(out, "\n") {
		l = strings.TrimSpace(l)
		switch {
		case l == "":
			return main // end of the first (primary) record
		case strings.HasPrefix(l, "worktree "):
			main = realPath(strings.TrimPrefix(l, "worktree "))
		case l == "bare":
			return ""
		}
	}
	return main
}

// Checkouts lists the registered work trees of a project, identities filled
// in. Paths recorded by older versions are inspected once and remembered;
// one that no longer exists keeps an empty identity rather than a guess.
func (s *Store) Checkouts(project string) []Checkout {
	if c, ok := s.coCache[project]; ok {
		return c
	}
	r := s.loadRegistry()
	learned := map[string]checkoutInfo{}
	var out []Checkout
	for _, p := range r.Projects[project] {
		c := Checkout{Path: p, Exists: fileExists(filepath.Join(p, ".git"))}
		info := r.Checkouts[p]
		if (info.Repo == "" || info.CommonDir == "") && c.Exists {
			if _, got, ok := inspectCheckout(p); ok {
				info = got
				learned[p] = got
			}
		}
		c.Repo, c.CommonDir = info.Repo, info.CommonDir
		c.Preferred = c.Repo != "" && r.Preferred[project][c.Repo] == p
		out = append(out, c)
	}
	if len(learned) > 0 {
		s.updateRegistry(func(r *registry) bool {
			for p, info := range learned {
				r.Checkouts[p] = info
			}
			return true
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Repo != out[j].Repo {
			return out[i].Repo < out[j].Repo
		}
		return out[i].Path < out[j].Path
	})
	if s.coCache == nil {
		s.coCache = map[string][]Checkout{}
	}
	s.coCache[project] = out
	return out
}

// ProjectRepos lists the repository identities known for a project: those in
// its project.yaml, those of its registered checkouts, and the current one.
func (s *Store) ProjectRepos(project string) []string {
	if r, ok := s.repoCache[project]; ok {
		return r
	}
	seen := map[string]bool{}
	if pf, ok := s.ProjectFiles()[project]; ok {
		for _, r := range pf.Repos {
			if id := repoIDOf(r); id != "" {
				seen[id] = true
			}
		}
	}
	for _, c := range s.Checkouts(project) {
		if c.Repo != "" {
			seen[c.Repo] = true
		}
	}
	if s.Project == project && s.repoID != "" {
		seen[s.repoID] = true
	}
	out := sortedKeys(seen)
	if s.repoCache == nil {
		s.repoCache = map[string][]string{}
	}
	s.repoCache[project] = out
	return out
}

// Selection is the checkout chosen as the baseline for one repository of a
// project, or the reason none could be chosen.
type Selection struct {
	Path   string // chosen checkout ("" when none)
	Why    string // rule that chose it
	Code   string // Unknown* code when nothing was chosen
	Reason string
}

// SelectCheckout chooses the baseline checkout for a repository of a
// project. Precedence: an explicit --checkout; the current checkout when it
// is that repository; a preferred checkout (kb project checkout set); the
// primary worktree when the repository has a single clone here. Anything
// else is reported as ambiguous or unavailable. Registration order never
// decides.
func (s *Store) SelectCheckout(project, repo string) Selection {
	key := project + "\x00" + repo
	if sel, ok := s.selCache[key]; ok {
		return sel
	}
	sel := s.selectCheckout(project, repo)
	if s.selCache == nil {
		s.selCache = map[string]Selection{}
	}
	s.selCache[key] = sel
	return sel
}

func (s *Store) selectCheckout(project, repo string) Selection {
	if s.baselinePath != "" && s.baselineRepo == repo {
		return Selection{Path: s.baselinePath, Why: "explicit --checkout"}
	}
	if s.CodeRoot != "" && s.Project == project && s.repoID == repo {
		return Selection{Path: s.CodeRoot, Why: "current checkout"}
	}
	var live []Checkout
	for _, c := range s.Checkouts(project) {
		if c.Repo == repo && c.Exists {
			live = append(live, c)
		}
	}
	for _, c := range live {
		if c.Preferred {
			return Selection{Path: c.Path, Why: "preferred checkout"}
		}
	}
	if len(live) == 0 {
		return Selection{Code: UnknownNoCheckout, Reason: repo + " is not checked out on this machine"}
	}
	clones := map[string]bool{}
	for _, c := range live {
		clones[c.CommonDir] = true
	}
	hint := fmt.Sprintf("; choose one: kb project checkout set %s --repo %s --path <checkout>", project, repo)
	if len(clones) > 1 {
		return Selection{Code: UnknownAmbiguousCo, Reason: fmt.Sprintf("%d independent clones of %s on this machine%s", len(clones), repo, hint)}
	}
	if main := mainWorktree(live[0].Path); main != "" && fileExists(filepath.Join(main, ".git")) {
		return Selection{Path: main, Why: "primary worktree"}
	}
	if len(live) == 1 {
		return Selection{Path: live[0].Path, Why: "only checkout"}
	}
	return Selection{Code: UnknownAmbiguousCo, Reason: fmt.Sprintf("%d worktrees of %s and no primary worktree%s", len(live), repo, hint)}
}

// repoHasCommit reports whether any clone of repo on this machine contains
// commit; checked is false when no clone is available to look in.
func (s *Store) repoHasCommit(project, repo, commit string) (has, checked bool) {
	seen := map[string]bool{}
	try := func(path, commonDir string) {
		if path == "" || seen[commonDir] || has {
			return
		}
		seen[commonDir] = true
		checked = true
		has = s.commitIn(path, commit)
	}
	if s.baselinePath != "" && s.baselineRepo == repo {
		try(s.baselinePath, "explicit")
	}
	if s.CodeRoot != "" && s.Project == project && s.repoID == repo {
		try(s.CodeRoot, "current")
	}
	for _, c := range s.Checkouts(project) {
		if c.Repo == repo && c.Exists {
			try(c.Path, c.CommonDir)
		}
	}
	return has, checked
}

// ownerRepo establishes which repository of a project an entry's resources
// refer to. Entries do not record it (structured anchors are a later stage),
// so in a multi-repository project it is inferred from the anchor commit:
// the one repository that contains it. Containment is only a hint (forks and
// mirrors share commits, a missing object proves nothing), so several
// matches are settled only by an explicit --checkout or the repository kb is
// running in, and an anchor stamped by an import is not used at all.
func (s *Store) ownerRepo(project, anchor string, imported bool) (repo, basis, code, reason string) {
	repos := s.ProjectRepos(project)
	switch len(repos) {
	case 0:
		return "", "", UnknownNoCheckout, fmt.Sprintf("no repository of project %s is checked out on this machine", project)
	case 1:
		return repos[0], "", "", ""
	}
	if imported {
		return "", "", UnknownAmbiguousRep, fmt.Sprintf("project %s spans %d repositories and this entry's commit was stamped by an import, not by a check of the code", project, len(repos))
	}
	var matches, absent []string
	for _, r := range repos {
		has, checked := s.repoHasCommit(project, r, anchor)
		switch {
		case !checked:
			absent = append(absent, r)
		case has:
			matches = append(matches, r)
		}
	}
	switch {
	case len(matches) == 1:
		return matches[0], "anchor commit found only in " + matches[0], "", ""
	case len(matches) > 1:
		shared := "anchor commit is in " + strings.Join(matches, " and ")
		if s.baselinePath != "" && containsStr(matches, s.baselineRepo) {
			return s.baselineRepo, shared, "", ""
		}
		if s.Project == project && containsStr(matches, s.repoID) {
			return s.repoID, shared, "", ""
		}
		return "", "", UnknownAmbiguousRep, fmt.Sprintf("anchor commit %s is in several repositories of project %s (%s): run kb inside one of them or pass --checkout", short(anchor), project, strings.Join(matches, ", "))
	case len(absent) > 0:
		return "", "", UnknownNoCheckout, fmt.Sprintf("anchor commit %s is not in the repositories of project %s checked out here; not checked out: %s", short(anchor), project, strings.Join(absent, ", "))
	}
	return "", "", UnknownAnchor, fmt.Sprintf("anchor commit %s is not in any repository of project %s on this machine", short(anchor), project)
}

// baselineFor resolves the checkout an entry is compared against.
func (s *Store) baselineFor(n *Node, anchor string, imported bool) (*Baseline, string, string) {
	if !s.Hub {
		if s.baselinePath != "" {
			return &Baseline{Path: s.baselinePath, Selection: "explicit --checkout"}, "", ""
		}
		if s.CodeRoot == "" {
			return nil, UnknownNoCheckout, "the bundle has no code repository"
		}
		return &Baseline{Path: s.CodeRoot, Selection: "repository of this bundle"}, "", ""
	}
	repo, basis, code, reason := s.ownerRepo(n.Project, anchor, imported)
	if code != "" {
		return nil, code, reason
	}
	sel := s.SelectCheckout(n.Project, repo)
	if sel.Code != "" {
		return &Baseline{Repo: repo}, sel.Code, sel.Reason
	}
	why := sel.Why
	if basis != "" {
		why += "; " + basis
	}
	return &Baseline{Repo: repo, Path: sel.Path, Selection: why}, "", ""
}

// codeFreshness compares an entry's resources with the baseline checkout.
func (s *Store) codeFreshness(n *Node, anchor string, imported bool) (state, code, reason string, b *Baseline) {
	m := n.Meta()
	if n.TypeKey == "review" || len(m.Resources) == 0 {
		return FreshNotApplicable, "", "", nil
	}
	if s.Hub && n.Project == GlobalProject {
		// Global entries are not tied to one repository; their resources are
		// path hints across projects.
		return FreshNotApplicable, "", "", nil
	}
	if anchor == "" {
		return FreshUnknown, UnknownNoAnchor, "no commit was recorded when this was written or last confirmed", nil
	}
	b, code, reason = s.baselineFor(n, anchor, imported)
	if code != "" {
		return FreshUnknown, code, reason, b
	}
	g := s.gitFor(b.Path)
	if !g.ok || g.head == "" {
		return FreshUnknown, UnknownNoCheckout, b.Path + " is not a git work tree with commits", b
	}
	target := "HEAD"
	b.Head = g.head
	if s.baselineRef != "" {
		c, ok := s.resolveIn(b.Path, s.baselineRef)
		if !ok {
			return FreshUnknown, UnknownRef, fmt.Sprintf("ref %q does not resolve in %s", s.baselineRef, b.Path), b
		}
		b.Ref, b.Head, target = s.baselineRef, c, c
	}
	files, ok := s.changedIn(b.Path, anchor, target)
	if !ok {
		return FreshUnknown, UnknownAnchor, fmt.Sprintf("anchor commit %s is not available in %s", short(anchor), b.Path), b
	}
	for _, f := range files {
		if MatchAny(m.Resources, f) {
			return FreshChanged, "", fmt.Sprintf("code changed since %s: %s", short(anchor), f), b
		}
	}
	return FreshUnchanged, "", "", b
}

// SetBaseline overrides baseline selection for this invocation: checkout is
// used for the repository it belongs to, and ref (instead of HEAD) is the
// comparison target in every selected checkout.
func (s *Store) SetBaseline(checkout, ref string) error {
	s.baselinePath, s.baselineRepo, s.baselineRef = "", "", strings.TrimSpace(ref)
	if checkout != "" {
		top, info, ok := inspectCheckout(checkout)
		if !ok {
			return fmt.Errorf("--checkout %s: not a git work tree", checkout)
		}
		s.baselinePath, s.baselineRepo = top, info.Repo
	}
	s.selCache = nil
	return nil
}

// CheckoutFor returns a local work tree of project on this machine, or "":
// the current checkout inside the project, otherwise the selected baseline
// of its first repository that has one.
func (s *Store) CheckoutFor(project string) string {
	if !s.Hub {
		return s.CodeRoot
	}
	if project == s.Project && s.CodeRoot != "" {
		return s.CodeRoot
	}
	if project == "" || project == GlobalProject {
		return ""
	}
	for _, r := range s.ProjectRepos(project) {
		if sel := s.SelectCheckout(project, r); sel.Path != "" {
			return sel.Path
		}
	}
	return ""
}

// anchorCheckoutFor returns the work tree whose HEAD may be recorded with a
// write to project: the current checkout, or from elsewhere the baseline of
// the project's only repository. A multi-repository project written to from
// outside gets no commit rather than one from a guessed repository.
func (s *Store) anchorCheckoutFor(project string) string {
	if !s.Hub || (project == s.Project && s.CodeRoot != "") {
		return s.CodeRoot
	}
	if project == "" || project == GlobalProject {
		return ""
	}
	if repos := s.ProjectRepos(project); len(repos) == 1 {
		return s.SelectCheckout(project, repos[0]).Path
	}
	return ""
}

// codeRootFor returns the work tree a node's resources refer to, or "".
func (s *Store) codeRootFor(n *Node) string {
	if !s.Hub {
		return s.CodeRoot
	}
	if n.Project == GlobalProject {
		return ""
	}
	t := s.trust(n, nil, false)
	if b, code, _ := s.baselineFor(n, t.Anchor, t.importAnchor); code == "" && b != nil {
		return b.Path
	}
	if n.Project == s.Project {
		return s.CodeRoot
	}
	return ""
}

// SetPreferredCheckout records, on this machine only, which checkout is the
// baseline for a repository of a project.
func (s *Store) SetPreferredCheckout(project, repo, path string) (string, string, error) {
	if !s.Hub {
		return "", "", errors.New("checkout preferences apply to hub mode")
	}
	if _, ok := s.ProjectFiles()[project]; !ok && len(s.Checkouts(project)) == 0 {
		return "", "", fmt.Errorf("unknown project %q (kb project list)", project)
	}
	top, info, ok := inspectCheckout(path)
	if !ok {
		return "", "", fmt.Errorf("%s is not a git work tree", path)
	}
	repos := s.ProjectRepos(project)
	if repo == "" {
		if !containsStr(repos, info.Repo) {
			return "", "", fmt.Errorf("%s is a checkout of %s, which is not a repository of project %s (%s)", top, info.Repo, project, strings.Join(repos, ", "))
		}
		repo = info.Repo
	}
	repo = repoIDOf(repo)
	if info.Repo != repo {
		return "", "", fmt.Errorf("%s is a checkout of %s, not %s", top, info.Repo, repo)
	}
	if !containsStr(repos, repo) {
		return "", "", fmt.Errorf("%s is not a repository of project %s (%s); 'kb project link %s' inside it adds it", repo, project, strings.Join(repos, ", "), project)
	}
	s.updateRegistry(func(r *registry) bool {
		r.adopt(project, top, info)
		if r.Preferred[project] == nil {
			r.Preferred[project] = map[string]string{}
		}
		r.Preferred[project][repo] = top
		return true
	})
	return repo, top, nil
}

// ClearPreferredCheckout removes a preference; with repo "" it clears the
// project's only one.
func (s *Store) ClearPreferredCheckout(project, repo string) (string, error) {
	prefs := s.loadRegistry().Preferred[project]
	if repo == "" {
		if len(prefs) != 1 {
			return "", fmt.Errorf("project %s has %d checkout preference(s); name one with --repo", project, len(prefs))
		}
		for r := range prefs {
			repo = r
		}
	}
	repo = repoIDOf(repo)
	if prefs[repo] == "" {
		return "", fmt.Errorf("no checkout preference for %s in project %s", repo, project)
	}
	s.updateRegistry(func(r *registry) bool {
		delete(r.Preferred[project], repo)
		if len(r.Preferred[project]) == 0 {
			delete(r.Preferred, project)
		}
		return true
	})
	return repo, nil
}

// RenderCheckouts describes a project's repositories, their checkouts on
// this machine and the baseline each resolves to.
func (s *Store) RenderCheckouts(project string) string {
	var b strings.Builder
	repos := s.ProjectRepos(project)
	cos := s.Checkouts(project)
	fmt.Fprintf(&b, "project %s\n", project)
	if len(repos) == 0 {
		b.WriteString("no repository of this project is known on this machine (run kb inside a checkout to register it)\n")
	}
	home, _ := os.UserHomeDir()
	show := func(p string) string {
		if home != "" && strings.HasPrefix(p, home+string(filepath.Separator)) {
			return "~" + strings.TrimPrefix(p, home)
		}
		return p
	}
	for _, repo := range repos {
		fmt.Fprintf(&b, "repo %s\n", repo)
		sel := s.SelectCheckout(project, repo)
		listed := false
		for _, c := range cos {
			if c.Repo != repo {
				continue
			}
			mark, note := " ", "checkout"
			switch {
			case !c.Exists:
				note = "missing"
			case c.Preferred:
				note = "preferred"
			}
			if c.Path == sel.Path {
				mark = "*"
				if !c.Preferred {
					note = sel.Why
				}
			}
			if c.Path == s.CodeRoot {
				note += ", current"
			}
			fmt.Fprintf(&b, "  %s %s  (%s)\n", mark, show(c.Path), note)
			listed = listed || c.Path == sel.Path
		}
		switch {
		case sel.Path != "" && !listed:
			fmt.Fprintf(&b, "  * %s  (%s)\n  baseline: %s\n", show(sel.Path), sel.Why, sel.Why)
		case sel.Path != "":
			fmt.Fprintf(&b, "  baseline: %s\n", sel.Why)
		default:
			fmt.Fprintf(&b, "  baseline: none - %s\n", sel.Reason)
		}
	}
	var unknown []string
	for _, c := range cos {
		if c.Repo == "" {
			state := "identity unreadable"
			if !c.Exists {
				state = "missing"
			}
			unknown = append(unknown, fmt.Sprintf("  %s  (%s)", show(c.Path), state))
		}
	}
	if len(unknown) > 0 {
		b.WriteString("registered paths with no known repository:\n" + strings.Join(unknown, "\n") + "\n")
	}
	return b.String()
}
