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
	changed map[string][]string // commit -> files changed between commit and HEAD
	bad     map[string]bool     // commits that cannot be resolved
	files   []string            // tracked files (lazy)
}

func (s *Store) gitc() *gitCache {
	if s.git != nil {
		return s.git
	}
	g := &gitCache{changed: map[string][]string{}, bad: map[string]bool{}}
	if top, err := Git(s.CodeRoot, "rev-parse", "--show-toplevel"); err == nil {
		g.ok = true
		g.top = realPath(top)
		if h, err := Git(s.CodeRoot, "rev-parse", "--short=12", "HEAD"); err == nil {
			g.head = h
		}
	}
	s.git = g
	return g
}

// HeadCommit returns the short HEAD commit of the code work tree, or "".
func (s *Store) HeadCommit() string { return s.gitc().head }

// InGit reports whether the code root is a git work tree.
func (s *Store) InGit() bool { return s.gitc().ok }

// GitTop returns the top-level directory of the code work tree.
func (s *Store) GitTop() string { return s.gitc().top }

// ChangedSince lists repo-relative files that differ between commit and HEAD.
// ok is false when the commit is unknown (e.g. a shallow clone).
func (s *Store) ChangedSince(commit string) (files []string, ok bool) {
	g := s.gitc()
	if !g.ok || g.head == "" || commit == "" || g.bad[commit] {
		return nil, false
	}
	if f, hit := g.changed[commit]; hit {
		return f, true
	}
	out, err := Git(s.CodeRoot, "diff", "--name-only", commit, "HEAD", "--")
	if err != nil {
		g.bad[commit] = true
		return nil, false
	}
	var list []string
	for _, l := range strings.Split(out, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			list = append(list, l)
		}
	}
	g.changed[commit] = list
	return list, true
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
