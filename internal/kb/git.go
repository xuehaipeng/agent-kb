package kb

import (
	"bytes"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
)

// Git runs git in dir and returns trimmed stdout.
func Git(dir string, args ...string) (string, error) {
	out, err := GitRaw(dir, args...)
	return strings.TrimSpace(out), err
}

// GitRaw runs git and returns stdout without trimming leading whitespace.
func GitRaw(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errb.String())
		if msg == "" {
			msg = err.Error()
		}
		return strings.TrimRight(out.String(), "\n"), fmt.Errorf("git %s: %s", strings.Join(args, " "), msg)
	}
	return strings.TrimRight(out.String(), "\n"), nil
}

// CodePathspec limits a git diff to code, excluding the bundle itself.
func (s *Store) CodePathspec() []string {
	if rel := s.bundleRelToTop(); rel != "" {
		return []string{"--", ".", ":(exclude)" + rel}
	}
	return []string{"--"}
}

type gitCache struct {
	ok      bool
	top     string
	head    string
	changed map[string][]string // "commit..target" -> files changed between them
	bad     map[string]bool     // comparisons that cannot be made
	revs    map[string]string   // rev -> short commit ("" when it does not resolve)
	files   []string            // tracked files (lazy)
}

// gitFor returns the (cached) git view of a work tree; root "" yields a
// cache that reports no repository.
func (s *Store) gitFor(root string) *gitCache {
	if s.gits == nil {
		s.gits = map[string]*gitCache{}
	}
	if g, ok := s.gits[root]; ok {
		return g
	}
	g := &gitCache{changed: map[string][]string{}, bad: map[string]bool{}, revs: map[string]string{}}
	if root != "" {
		if top, err := Git(root, "rev-parse", "--show-toplevel"); err == nil {
			g.ok = true
			g.top = realPath(top)
			if h, err := Git(root, "rev-parse", "--short=12", "HEAD"); err == nil {
				g.head = h
			}
		}
	}
	s.gits[root] = g
	return g
}

func (s *Store) gitc() *gitCache { return s.gitFor(s.CodeRoot) }

// HeadCommit returns the short HEAD commit of the current work tree, or "".
func (s *Store) HeadCommit() string { return s.gitc().head }

// headFor returns the commit recorded with a write to project: the HEAD of
// the work tree its resources refer to, or "" when that is not established.
func (s *Store) headFor(project string) string {
	if s.Hub && project == GlobalProject {
		return ""
	}
	return s.gitFor(s.anchorCheckoutFor(project)).head
}

// resolveIn resolves a ref or commit to a short commit in a work tree.
func (s *Store) resolveIn(root, rev string) (string, bool) {
	g := s.gitFor(root)
	if !g.ok || rev == "" {
		return "", false
	}
	if c, hit := g.revs[rev]; hit {
		return c, c != ""
	}
	c, err := Git(root, "rev-parse", "--verify", "--quiet", "--short=12", rev+"^{commit}")
	if err != nil {
		c = ""
	}
	g.revs[rev] = c
	return c, c != ""
}

// commitIn reports whether a work tree's repository contains commit.
func (s *Store) commitIn(root, commit string) bool {
	_, ok := s.resolveIn(root, commit)
	return ok
}

// InGit reports whether the current work tree is a git repository.
func (s *Store) InGit() bool { return s.gitc().ok }

// GitTop returns the top-level directory of the current work tree.
func (s *Store) GitTop() string { return s.gitc().top }

// ChangedSince lists files that differ between commit and HEAD in the
// current work tree. ok is false when the commit is unknown.
func (s *Store) ChangedSince(commit string) ([]string, bool) {
	return s.changedIn(s.CodeRoot, commit, "HEAD")
}

// changedIn lists files that differ between commit and target in a work
// tree. ok is false when the comparison cannot be made (no repository, or
// either side does not resolve there): the answer is unknown, not "nothing
// changed".
func (s *Store) changedIn(root, commit, target string) ([]string, bool) {
	g := s.gitFor(root)
	key := commit + ".." + target
	if !g.ok || g.head == "" || commit == "" || g.bad[key] {
		return nil, false
	}
	if f, hit := g.changed[key]; hit {
		return f, true
	}
	out, err := Git(root, "diff", "--name-only", commit, target, "--")
	if err != nil {
		g.bad[key] = true
		return nil, false
	}
	var list []string
	for _, l := range strings.Split(out, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			list = append(list, l)
		}
	}
	g.changed[key] = list
	return list, true
}

// trackedIn lists tracked files of a work tree (cached).
func (s *Store) trackedIn(root string) []string {
	g := s.gitFor(root)
	if !g.ok {
		return nil
	}
	if g.files != nil {
		return g.files
	}
	g.files = []string{}
	if out, err := Git(root, "ls-files"); err == nil {
		for _, l := range strings.Split(out, "\n") {
			if l = strings.TrimSpace(l); l != "" {
				g.files = append(g.files, l)
			}
		}
	}
	return g.files
}

// RelToCode converts a path (absolute or relative to cwd) into a path
// relative to the git top-level, the frame resource globs use.
func (s *Store) RelToCode(p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return ""
	}
	base := s.GitTop()
	if base == "" {
		base = s.CodeRoot
	}
	if filepath.IsAbs(p) {
		p = realPath(p)
		if rel, err := filepath.Rel(base, p); err == nil && !strings.HasPrefix(rel, "..") {
			return filepath.ToSlash(rel)
		}
		return filepath.ToSlash(p)
	}
	// Prefer cwd-relative when that file exists; otherwise treat the path as
	// already relative to the repository root (what agents usually pass).
	if abs, err := filepath.Abs(p); err == nil && fileExists(abs) {
		if rel, err := filepath.Rel(base, realPath(abs)); err == nil && !strings.HasPrefix(rel, "..") {
			return filepath.ToSlash(rel)
		}
	}
	return normGlobPath(p)
}
