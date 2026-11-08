package kb

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/xuehaipeng/agent-kb/internal/okf"
)

// Issue is one lint finding.
type Issue struct {
	Severity string // error | warn
	Path     string // bundle-relative
	Msg      string
}

func (i Issue) String() string { return fmt.Sprintf("%s: %s: %s", i.Severity, i.Path, i.Msg) }

// LintOptions controls optional checks.
type LintOptions struct {
	Staged bool // also check immutability against git HEAD
}

var (
	mdLink     = regexp.MustCompile(`\]\(([^)\s]+)\)`)
	footnoteRe = regexp.MustCompile(`\[\^([^\]]+)\]`)
)

// mutableKeys are frontmatter keys the tool may change on immutable types.
var mutableKeys = map[string]bool{"verified": true, "status": true, "stale_after": true}

// Lint runs deterministic, LLM-free checks. It reports; it never invents
// content (auto-created stubs are how dead links grow under weekly lint).
func (s *Store) Lint(opt LintOptions) ([]Issue, error) {
	s.Invalidate()
	nodes, err := s.Nodes()
	if err != nil {
		return nil, err
	}
	var issues []Issue
	add := func(sev, path, format string, a ...any) {
		issues = append(issues, Issue{sev, path, fmt.Sprintf(format, a...)})
	}
	for _, pe := range s.ParseErrors() {
		add("error", s.rel(pe.Path), "%v", pe.Err)
	}
	sup := s.supersededIndex()
	titles := map[string]string{}
	for _, n := range nodes {
		p := n.ID + ".md"
		m := n.Meta()
		if m.Type == "" {
			add("error", p, "missing required key 'type'")
		} else if k, ok := s.Cfg.TypeKey(m.Type); !ok {
			add("error", p, "unknown type %q", m.Type)
		} else if k != n.TypeKey {
			add("error", p, "type %q does not belong in %s/", m.Type, s.Cfg.Types[n.TypeKey].Dir)
		}
		if strings.TrimSpace(m.Title) == "" {
			add("error", p, "missing 'title'")
		}
		if strings.TrimSpace(m.Description) == "" {
			add("warn", p, "missing one-line 'description' (search results show it)")
		}
		if m.Generated.By == "" || m.Generated.At == "" {
			add("error", p, "missing 'generated: {by, at}' provenance")
		}
		checkTime := func(field, v string) {
			if v == "" {
				return
			}
			ts, err := parseTime(v)
			if err != nil {
				add("error", p, "%s: %v", field, err)
				return
			}
			if !hasOffset(v) {
				add("error", p, "%s %q has no timezone offset", field, v)
			}
			_ = ts
		}
		checkTime("generated.at", m.Generated.At)
		checkTime("stale_after", m.StaleAfter)
		for i, e := range m.Verified {
			f := fmt.Sprintf("verified[%d]", i)
			if e.By == "" || e.At == "" {
				add("error", p, "%s: needs 'by' and 'at'", f)
			}
			checkTime(f+".at", e.At)
			switch e.Result {
			case "", ResultConfirmed, ResultDisputed, ResultApproved, ResultChanges:
			default:
				add("error", p, "%s: unknown result %q", f, e.Result)
			}
		}
		if m.Status != "" {
			switch n.TypeKey {
			case "review":
				switch m.Status {
				case ReviewPending, ReviewChanges, ReviewApproved, ReviewClosed:
				default:
					add("error", p, "review status %q invalid", m.Status)
				}
			default:
				t := s.TrustStatic(n, sup)
				ok := false
				for _, want := range ExpectedStatus(t) {
					if m.Status == want {
						ok = true
					}
				}
				if !ok {
					add("warn", p, "status %q disagrees with derived trust state %q", m.Status, t.State)
				}
			}
		}
		if n.TypeKey == "review" && m.Review == nil {
			add("error", p, "review is missing its 'review:' block")
		}
		// Sources and footnotes.
		ids := map[string]bool{}
		for _, src := range m.Sources {
			if src.ID == "" {
				add("error", p, "source without id")
			} else if ids[src.ID] {
				add("error", p, "duplicate source id %q", src.ID)
			}
			ids[src.ID] = true
		}
		for _, fm := range footnoteRe.FindAllStringSubmatch(n.Doc.Body, -1) {
			if !ids[fm[1]] && !strings.Contains(n.Doc.Body, "[^"+fm[1]+"]:") {
				add("error", p, "footnote [^%s] has no matching source", fm[1])
			}
		}
		// Supersedes targets.
		for _, ref := range m.Supersedes {
			if _, err := s.Get(ref); err != nil {
				add("error", p, "supersedes %q: not found", ref)
			}
		}
		// Relative links.
		for _, lm := range mdLink.FindAllStringSubmatch(n.Doc.Body, -1) {
			target := lm[1]
			if strings.Contains(target, "://") || strings.HasPrefix(target, "#") || strings.HasPrefix(target, "mailto:") {
				continue
			}
			target = strings.SplitN(target, "#", 2)[0]
			var abs string
			if strings.HasPrefix(target, "/") {
				abs = filepath.Join(s.Dir, filepath.FromSlash(target))
			} else {
				abs = filepath.Join(filepath.Dir(n.Path), filepath.FromSlash(target))
			}
			if !fileExists(abs) {
				add("error", p, "broken link %q", lm[1])
			}
		}
		// Resource globs should point at something that exists.
		if len(m.Resources) > 0 && s.InGit() && n.TypeKey != "review" && s.TrustStatic(n, sup).State != StateDeprecated {
			files := s.trackedFiles()
			for _, g := range m.Resources {
				found := false
				for _, f := range files {
					if MatchGlob(g, f) {
						found = true
						break
					}
				}
				if !found {
					add("warn", p, "resource %q matches no tracked file", g)
				}
			}
		}
		// Size, secrets, retired terms.
		if max := s.Cfg.Lint.MaxBodyBytes; max > 0 && len(n.Doc.Body) > max && n.TypeKey != "review" {
			add("warn", p, "body is %d bytes (> %d): split into atomic claims", len(n.Doc.Body), max)
		}
		raw, _ := os.ReadFile(n.Path)
		if ContainsSecret(string(raw)) {
			add("error", p, "possible secret: remove it (never store credentials in knowledge)")
		}
		lower := strings.ToLower(string(raw))
		for _, rt := range s.Cfg.Lint.RetiredTerms {
			if rt.Term != "" && strings.Contains(lower, strings.ToLower(rt.Term)) && s.TrustStatic(n, sup).State != StateDeprecated {
				msg := fmt.Sprintf("mentions retired term %q", rt.Term)
				if rt.Hint != "" {
					msg += " (" + rt.Hint + ")"
				}
				add("error", p, "%s", msg)
			}
		}
		if n.TypeKey != "review" {
			key := strings.ToLower(strings.TrimSpace(m.Title))
			if prev, dup := titles[key]; dup && key != "" && s.TrustStatic(n, sup).State != StateDeprecated {
				add("warn", p, "same title as %s", prev)
			} else if s.TrustStatic(n, sup).State != StateDeprecated {
				titles[key] = p
			}
		}
		if opt.Staged && s.Cfg.Types[n.TypeKey].Immutable {
			s.checkImmutable(n, add)
		}
	}
	// Index drift.
	files, err := s.IndexFiles()
	if err == nil {
		for rel, want := range files {
			got, err := os.ReadFile(filepath.Join(s.Dir, rel))
			if err != nil || string(got) != string(want) {
				add("error", filepath.ToSlash(rel), "out of date: run 'kb index'")
			}
		}
	}
	sort.SliceStable(issues, func(i, j int) bool {
		if issues[i].Severity != issues[j].Severity {
			return issues[i].Severity == "error"
		}
		return issues[i].Path < issues[j].Path
	})
	return issues, nil
}

func (s *Store) checkImmutable(n *Node, add func(sev, path, format string, a ...any)) {
	rel := s.relToTop(n.Path)
	if rel == "" {
		return
	}
	old, err := Git(s.CodeRoot, "show", "HEAD:"+rel)
	if err != nil {
		return // new file
	}
	prev, err := okf.Parse([]byte(old + "\n"))
	if err != nil {
		return
	}
	cur := n.Doc
	// Compare what is about to be committed (the index), not the work tree.
	if staged, err := Git(s.CodeRoot, "show", ":"+rel); err == nil {
		if d, err := okf.Parse([]byte(staged + "\n")); err == nil {
			cur = d
		}
	}
	if prev.ContentKey(mutableKeys) != cur.ContentKey(mutableKeys) {
		add("error", n.ID+".md", "%s entries are immutable: supersede it ('kb add --supersedes %s') instead of editing", n.TypeKey, n.ID)
	}
}

func (s *Store) trackedFiles() []string {
	g := s.gitc()
	if g.files != nil {
		return g.files
	}
	out, err := Git(s.CodeRoot, "ls-files")
	g.files = []string{}
	if err == nil {
		for _, l := range strings.Split(out, "\n") {
			if l = strings.TrimSpace(l); l != "" {
				g.files = append(g.files, l)
			}
		}
	}
	return g.files
}

func (s *Store) rel(p string) string {
	if r, err := filepath.Rel(s.Dir, p); err == nil {
		return filepath.ToSlash(r)
	}
	return p
}

func (s *Store) relToTop(p string) string {
	top := s.GitTop()
	if top == "" {
		return ""
	}
	r, err := filepath.Rel(top, p)
	if err != nil || strings.HasPrefix(r, "..") {
		return ""
	}
	return filepath.ToSlash(r)
}

func hasOffset(v string) bool {
	v = strings.TrimSpace(v)
	if strings.HasSuffix(v, "Z") || strings.HasSuffix(v, "z") {
		return true
	}
	if len(v) < 6 {
		return false
	}
	tail := v[len(v)-6:]
	return (tail[0] == '+' || tail[0] == '-') && tail[3] == ':'
}
