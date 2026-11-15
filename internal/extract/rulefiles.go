package extract

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/xuehaipeng/agent-kb/internal/kb"
	"github.com/xuehaipeng/agent-kb/internal/okf"
)

// Rule files are read the way their own agent loads them, so that sharing
// them never changes what they mean:
//
//   - a path-scoped rule (Claude Code `paths:` frontmatter, a rule file in a
//     subdirectory) stays conditional: it becomes a rule with applies_to and
//     is never delivered as an always-on rule;
//   - a conditional file whose condition cannot be reproduced exactly
//     (frontmatter that does not parse, a pattern this matcher cannot
//     express, Cursor's own matching) is not imported, and says so;
//   - `@path` imports are followed inside the file's own trust boundary
//     only, block-level HTML comments are dropped, and kb's own generated
//     blocks never come back in.

const maxImportDepth = 4 // hops, as Claude Code follows them

// rulePrep describes how one rule file is to be read.
type rulePrep struct {
	path     string
	agent    string
	project  string // kb project for the entries ("" current, "global")
	boundary string // imports and links are followed inside this directory only
	rulesDir bool   // lives in a rules directory: frontmatter may scope it
	base     string // repo-relative directory its patterns are relative to
	nested   bool   // loads only for files under base (a subdirectory's rules)
	repo     bool   // found in a repository: what it gives is shared with the team
}

// RuleFileSources finds rule files written for a single agent: user-level
// instruction files (shared as global rules) and, inside a repository,
// agent-specific rule files the other agents don't read. AGENTS.md is left
// out: Codex, Cursor and Antigravity already read it natively. The notes
// name everything that was found but deliberately not imported.
func RuleFileSources(s *kb.Store, user, project bool) ([]FileSource, []string) {
	var preps []rulePrep
	var notes []string
	if user {
		g := globalOrCurrent(s)
		// Paths stay as the user knows them; only the trust boundary is
		// compared in resolved form.
		cd, cdReal := claudeDir(), kb.RealPath(claudeDir())
		preps = append(preps, rulePrep{path: filepath.Join(cd, "CLAUDE.md"), agent: "claude-code", project: g, boundary: cdReal})
		for _, p := range walkRules(filepath.Join(cd, "rules"), cdReal, ".md", &notes) {
			preps = append(preps, rulePrep{path: p, agent: "claude-code", project: g, boundary: cdReal, rulesDir: true})
		}
		preps = append(preps, rulePrep{path: filepath.Join(codexDir(), "AGENTS.md"), agent: "codex", project: g, boundary: kb.RealPath(codexDir())})
		gd := filepath.Join(homeDir(), ".gemini")
		preps = append(preps, rulePrep{path: filepath.Join(gd, "GEMINI.md"), agent: "antigravity", project: g, boundary: kb.RealPath(gd)})
	}
	if project && s.CodeRoot != "" {
		root := kb.RealPath(s.CodeRoot)
		at := func(rel, agent string) rulePrep {
			return rulePrep{path: filepath.Join(root, filepath.FromSlash(rel)), agent: agent, boundary: root, repo: true}
		}
		preps = append(preps, at("CLAUDE.md", "claude-code"), at(".claude/CLAUDE.md", "claude-code"), at("GEMINI.md", "antigravity"),
			at(".windsurfrules", "windsurf"), at(".github/copilot-instructions.md", "copilot"))
		for _, d := range []struct{ dir, agent, ext string }{
			{".claude/rules", "claude-code", ".md"}, {".cursor/rules", "cursor", ".mdc"}, {".cursor/rules", "cursor", ".md"}, {".agents/rules", "antigravity", ".md"},
		} {
			for _, p := range walkRules(filepath.Join(root, filepath.FromSlash(d.dir)), root, d.ext, &notes) {
				preps = append(preps, rulePrep{path: p, agent: d.agent, boundary: root, rulesDir: true, repo: true})
			}
		}
		// A subdirectory's CLAUDE.md and rules load only once files there are
		// worked on: they are conditional on that directory.
		for _, rel := range nestedClaudeFiles(root) {
			dir := rel[:strings.LastIndex(rel, "/")]
			p := rulePrep{path: filepath.Join(root, filepath.FromSlash(rel)), agent: "claude-code", boundary: root, nested: true, repo: true}
			switch {
			case strings.Contains(rel, "/.claude/rules/"):
				p.base, p.rulesDir = rel[:strings.Index(rel, "/.claude/rules/")], true
			case strings.HasSuffix(dir, "/.claude") || dir == ".claude":
				p.base = strings.TrimSuffix(strings.TrimSuffix(dir, ".claude"), "/")
			default:
				p.base = dir
			}
			preps = append(preps, p)
		}
		for _, rel := range append([]string{"CLAUDE.local.md"}, localClaudeFiles(root)...) {
			if fileExists(filepath.Join(root, filepath.FromSlash(rel))) {
				notes = append(notes, rel+": not imported (personal instructions that are not committed stay private)")
			}
		}
	}
	var out []FileSource
	seen := map[string]bool{}
	for _, p := range preps {
		if seen[p.path] {
			continue
		}
		seen[p.path] = true
		if st, err := os.Stat(p.path); err != nil || st.IsDir() {
			continue
		}
		// A repository's instruction file that is a link to somewhere
		// outside it is not the repository's content: what it says would
		// be sent to the model and shared with everyone using the bundle.
		// This holds wherever the file was found, not only in a rules
		// directory.
		if p.repo {
			if target, err := filepath.EvalSymlinks(p.path); err != nil || !within(p.boundary, target) {
				notes = append(notes, displayPath(p.path)+": not imported (a link to a location outside "+displayPath(p.boundary)+")")
				continue
			}
		}
		src, note := prepareRuleFile(s, p)
		if note != "" {
			notes = append(notes, note)
		}
		if src != nil {
			out = append(out, *src)
		}
	}
	return out, uniqueStrings(notes)
}

// PrepareExplicitRuleFile reads a rule file named on the command line. Its
// base directory is unknown, so a conditional file is not imported.
func PrepareExplicitRuleFile(s *kb.Store, path string) (*FileSource, string) {
	abs, _ := filepath.Abs(path)
	return prepareRuleFile(s, rulePrep{path: abs, agent: "user", boundary: filepath.Dir(kb.RealPath(abs)), rulesDir: true, base: "\x00unknown"})
}

func fileExists(p string) bool { _, err := os.Stat(p); return err == nil }

func uniqueStrings(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, v := range in {
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	return out
}

// walkRules lists rule files under dir, recursively. A link is followed only
// when it stays inside boundary: a linked file elsewhere is someone else's
// content until the user approves it in their own tool.
func walkRules(dir, boundary, ext string, notes *[]string) []string {
	out := walkRulesOnce(dir, boundary, ext, notes, map[string]bool{})
	sort.Strings(out)
	return out
}

// walkRulesOnce reads each directory once, whatever leads to it: a link to
// a directory already read (itself, an ancestor, a sibling) adds nothing.
func walkRulesOnce(dir, boundary, ext string, notes *[]string, seen map[string]bool) []string {
	var out []string
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			real, err := filepath.EvalSymlinks(p)
			if err != nil {
				real = p
			}
			if seen[real] {
				return filepath.SkipDir
			}
			seen[real] = true
			return nil
		}
		if d.Type()&fs.ModeSymlink != 0 {
			target, err := filepath.EvalSymlinks(p)
			if err != nil || !within(boundary, target) {
				*notes = append(*notes, displayPath(p)+": not imported (a link to a location outside "+displayPath(boundary)+")")
				return nil
			}
			st, err := os.Stat(target)
			if err != nil {
				return nil
			}
			if st.IsDir() {
				out = append(out, walkRulesOnce(target, boundary, ext, notes, seen)...)
				return nil
			}
		}
		if strings.HasSuffix(d.Name(), ext) {
			out = append(out, p)
		}
		return nil
	})
	sort.Strings(out)
	return out
}

// nestedClaudeFiles lists tracked Claude instruction files below the root
// directory (repo-relative, slash separated).
func nestedClaudeFiles(root string) []string {
	out, err := kb.Git(root, "ls-files", "--", "*CLAUDE.md", "*/.claude/rules/*.md")
	if err != nil {
		return nil
	}
	var files []string
	for _, l := range strings.Split(out, "\n") {
		l = strings.TrimSpace(l)
		switch {
		case l == "" || l == "CLAUDE.md" || l == ".claude/CLAUDE.md" || strings.HasPrefix(l, ".claude/rules/"):
		case strings.HasSuffix(l, "/CLAUDE.md") || strings.Contains(l, "/.claude/rules/"):
			files = append(files, l)
		}
	}
	sort.Strings(files)
	return files
}

func localClaudeFiles(root string) []string {
	var out []string
	matches, _ := filepath.Glob(filepath.Join(root, "*", "CLAUDE.local.md"))
	for _, m := range matches {
		if rel, err := filepath.Rel(root, m); err == nil {
			out = append(out, filepath.ToSlash(rel))
		}
	}
	return out
}

// prepareRuleFile returns the source to import, or nil with the reason it is
// not imported.
func prepareRuleFile(s *kb.Store, p rulePrep) (*FileSource, string) {
	data, err := os.ReadFile(p.path)
	if err != nil {
		return nil, ""
	}
	if len(data) > maxImportBytes {
		data = data[:maxImportBytes]
	}
	label := displayPath(p.path)
	text := kbBlockRe.ReplaceAllString(strings.ReplaceAll(strings.TrimPrefix(string(data), "\xef\xbb\xbf"), "\r\n", "\n"), "")
	var applies *okf.Applicability
	if p.rulesDir {
		fm, body, has, perr := splitFrontmatter(text)
		switch {
		case perr != nil:
			return nil, label + ": not imported (its frontmatter does not parse, so whether it is path-scoped is unknown; importing it could turn a conditional rule into an always-on one)"
		case has:
			paths, cond, why := ruleCondition(p.agent, fm)
			switch {
			case why != "":
				return nil, label + ": not imported (" + why + ")"
			case cond && p.base == "\x00unknown":
				return nil, label + ": not imported (it is path-scoped, and its base directory is unknown here: import it from its repository with 'kb rules import --repo')"
			case cond:
				applies = &okf.Applicability{Paths: paths, Base: p.base, Origin: kb.OriginClaudeRules}
				if err := kb.ValidateApplies(applies); err != nil {
					return nil, fmt.Sprintf("%s: not imported (%v; importing it without the condition would widen it)", label, err)
				}
			}
			text = body
		}
	}
	if applies == nil && p.nested {
		applies = &okf.Applicability{Paths: []string{"**"}, Base: p.base, Origin: kb.OriginClaudeRules}
	}
	var notes []string
	text = expandImports(s, stripBlockComments(text), filepath.Dir(p.path), p.boundary, map[string]bool{kb.RealPath(p.path): true}, 0, &notes)
	src := &FileSource{Path: p.path, Kind: KindRules, Agent: kb.Actor{Producer: p.agent}, Project: p.project, Content: text, Prepared: true, Applies: applies}
	note := ""
	if len(notes) > 0 {
		note = label + ": " + strings.Join(uniqueStrings(notes), "; ")
	}
	return src, note
}

// splitFrontmatter separates YAML frontmatter from the body.
func splitFrontmatter(text string) (fm map[string]yaml.Node, body string, has bool, err error) {
	if !strings.HasPrefix(text, "---\n") {
		return nil, text, false, nil
	}
	rest := text[4:]
	end := strings.Index(rest, "\n---\n")
	switch {
	case end >= 0:
		body = rest[end+5:]
	case strings.HasSuffix(rest, "\n---"):
		end = len(rest) - 4
	case strings.HasPrefix(rest, "---\n"):
		return nil, rest[4:], true, nil
	default:
		return nil, text, false, nil // a leading rule line, not frontmatter
	}
	if err := yaml.Unmarshal([]byte(rest[:end]), &fm); err != nil {
		return nil, "", true, err
	}
	return fm, body, true, nil
}

// ruleCondition reads a rule file's frontmatter: its path patterns when it
// is conditional in a way kb can reproduce, or why it cannot be imported.
func ruleCondition(agent string, fm map[string]yaml.Node) (paths []string, conditional bool, unsupported string) {
	list := func(key string) ([]string, bool, bool) {
		n, ok := fm[key]
		if !ok {
			return nil, false, true
		}
		var out []string
		switch n.Kind {
		case yaml.ScalarNode:
			out = kb.SplitPatterns(n.Value)
		case yaml.SequenceNode:
			for _, c := range n.Content {
				if c.Kind != yaml.ScalarNode {
					return nil, true, false
				}
				if v := strings.TrimSpace(c.Value); v != "" {
					out = append(out, v)
				}
			}
		default:
			return nil, true, false
		}
		return out, true, true
	}
	switch agent {
	case "claude-code":
		ps, present, ok := list("paths")
		if !ok {
			return nil, false, "its `paths` field is not a list of patterns"
		}
		return ps, present && len(ps) > 0, ""
	case "cursor":
		if n, ok := fm["alwaysApply"]; ok && n.Kind == yaml.ScalarNode && strings.EqualFold(n.Value, "true") {
			return nil, false, ""
		}
		return nil, false, "a Cursor rule that is not `alwaysApply: true` applies only when Cursor attaches it; kb cannot reproduce that"
	default:
		for _, key := range []string{"globs", "trigger"} {
			if _, ok := fm[key]; ok {
				return nil, false, "it is conditional (`" + key + "`) in a way kb cannot reproduce for this agent"
			}
		}
		// `paths` is read as Claude Code's rule syntax, the only one kb matches.
		ps, present, ok := list("paths")
		if !ok {
			return nil, false, "its `paths` field is not a list of patterns"
		}
		return ps, present && len(ps) > 0, ""
	}
}

var fenceRe = regexp.MustCompile("^ {0,3}(`{3,}|~{3,})(.*)$")

// fenceState follows fenced code blocks line by line, as Markdown defines
// them: a block opened by a run of backticks or tildes is closed only by a
// run of the same character that is at least as long and has nothing after
// it. A shorter or different fence inside it is content of the block (a
// four-backtick block showing Markdown that has its own fences).
type fenceState struct {
	char byte
	n    int
}

func (f *fenceState) open() bool { return f.n > 0 }

// line takes the next line and reports whether it opens or closes a block.
func (f *fenceState) line(l string) bool {
	m := fenceRe.FindStringSubmatch(l)
	if m == nil {
		return false
	}
	run, rest := m[1], m[2]
	if f.n == 0 {
		if run[0] == '`' && strings.Contains(rest, "`") {
			return false // backticks after the run: inline code, not a fence
		}
		f.char, f.n = run[0], len(run)
		return true
	}
	if run[0] == f.char && len(run) >= f.n && strings.TrimSpace(rest) == "" {
		f.n = 0
		return true
	}
	return false
}

// stripBlockComments removes block-level HTML comments, as Claude Code does
// before loading an instruction file. Comments inside code blocks stay.
func stripBlockComments(text string) string {
	var out []string
	var fence fenceState
	inComment := false
	for _, l := range strings.Split(text, "\n") {
		if inComment {
			if i := strings.Index(l, "-->"); i >= 0 {
				inComment = false
				if rest := strings.TrimSpace(l[i+3:]); rest != "" {
					out = append(out, rest)
				}
			}
			continue
		}
		if fence.line(l) {
			out = append(out, l)
			continue
		}
		if !fence.open() && strings.HasPrefix(strings.TrimLeft(l, " "), "<!--") && len(l)-len(strings.TrimLeft(l, " ")) <= 3 {
			if i := strings.Index(l, "-->"); i >= 0 {
				if rest := strings.TrimSpace(l[i+3:]); rest != "" {
					out = append(out, rest)
				}
			} else {
				inComment = true
			}
			continue
		}
		out = append(out, l)
	}
	return strings.Join(out, "\n")
}

var (
	importRe   = regexp.MustCompile("(?:^|[\\s(])@((?:\\\\ |[^\\s`'\"()<>])+)")
	codeSpanRe = regexp.MustCompile("`[^`]*`")
)

// expandImports appends the files a text imports with @path, the way Claude
// Code loads them next to the importing file. Only existing files inside the
// boundary are followed; kb's own files and AGENTS.md never are.
func expandImports(s *kb.Store, text, dir, boundary string, seen map[string]bool, depth int, notes *[]string) string {
	var extra []string
	var fence fenceState
	for _, l := range strings.Split(text, "\n") {
		if fence.line(l) || fence.open() {
			continue
		}
		for _, m := range importRe.FindAllStringSubmatch(codeSpanRe.ReplaceAllString(l, ""), -1) {
			ref := strings.ReplaceAll(m[1], `\ `, " ")
			target := resolveImport(ref, dir)
			if target == "" {
				if alt := strings.TrimRight(ref, ".,;:!?"); alt != ref {
					target, ref = resolveImport(alt, dir), alt
				}
			}
			if target == "" {
				continue // not a file: an @mention, a package name
			}
			switch {
			case within(kb.RealPath(s.Dir), target):
				// kb's own pointer or bundle: never re-imported.
			case filepath.Base(target) == "AGENTS.md":
				*notes = append(*notes, "@"+ref+" was not followed (other agents read AGENTS.md natively)")
			case !within(boundary, target):
				*notes = append(*notes, "@"+ref+" was not followed (it is outside "+displayPath(boundary)+")")
			case seen[target]:
				// already included, or an import cycle
			case depth >= maxImportDepth:
				*notes = append(*notes, "@"+ref+" was not followed (more than "+fmt.Sprint(maxImportDepth)+" imports deep)")
			default:
				seen[target] = true
				data, err := os.ReadFile(target)
				if err != nil {
					continue
				}
				if len(data) > maxImportBytes {
					data = data[:maxImportBytes]
				}
				body := kbBlockRe.ReplaceAllString(strings.ReplaceAll(string(data), "\r\n", "\n"), "")
				if _, b, has, err := splitFrontmatter(body); err == nil && has {
					body = b
				}
				extra = append(extra, expandImports(s, stripBlockComments(body), filepath.Dir(target), boundary, seen, depth+1, notes))
			}
		}
	}
	if len(extra) == 0 {
		return text
	}
	return strings.TrimRight(text, "\n") + "\n\n" + strings.Join(extra, "\n\n")
}

// resolveImport returns the real path of an imported file, or "".
func resolveImport(ref, dir string) string {
	p := ref
	switch {
	case strings.HasPrefix(p, "~/"):
		p = filepath.Join(homeDir(), p[2:])
	case !filepath.IsAbs(p):
		p = filepath.Join(dir, p)
	}
	st, err := os.Stat(p)
	if err != nil || !st.Mode().IsRegular() {
		return ""
	}
	return kb.RealPath(p)
}
