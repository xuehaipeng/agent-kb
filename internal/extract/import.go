package extract

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/xuehaipeng/agent-kb/internal/kb"
	"github.com/xuehaipeng/agent-kb/internal/okf"
)

// Whole-file sources: what agents saved on purpose (memory files) and what
// the user wrote for one agent (rule files). Unlike transcripts, these are
// already distilled and their author is known, so imported entries are
// recorded as verified by the source agent, with the file as evidence.
const (
	KindMemory = "memory"
	KindRules  = "rules"

	maxImportBytes      = 200 << 10
	maxMemoryClaims     = 15
	maxRuleClaims       = 30
	minImportContentLen = 40
)

// FileSource is one memory or rule file.
type FileSource struct {
	Path    string
	Kind    string   // KindMemory | KindRules
	Agent   kb.Actor // whose memory, or which agent the rule file was written for
	Project string   // default target: "" = current project, "global" = shared
}

// ImportOptions controls an import run.
type ImportOptions struct {
	Command []string // override extractor command
	DryRun  bool     // print the prompt instead of calling the model
	Force   bool     // re-import even if the file is unchanged
	Out     io.Writer
}

var kbBlockRe = regexp.MustCompile(`(?s)<!-- kb:begin.*?<!-- kb:end -->\n?`)

// ImportFile turns a memory or rule file into knowledge entries.
func ImportFile(s *kb.Store, src FileSource, o ImportOptions) (*Result, error) {
	res := &Result{}
	data, err := os.ReadFile(src.Path)
	if err != nil {
		return nil, err
	}
	if len(data) > maxImportBytes {
		data = data[:maxImportBytes]
	}
	// Never re-import kb's own managed blocks (pointer, delivered rules).
	content := strings.TrimSpace(kbBlockRe.ReplaceAllString(string(data), ""))
	sum := sha256.Sum256([]byte(content))
	hash := hex.EncodeToString(sum[:])
	abs, _ := filepath.Abs(src.Path)
	st := loadState(s, abs)
	if !o.Force && st.Hash == hash {
		res.Skipped = "unchanged since last import"
		return res, nil
	}
	if len(content) < minImportContentLen {
		res.Skipped = "nothing to import"
		if !o.DryRun {
			return res, saveHash(s, abs, hash)
		}
		return res, nil
	}
	prompt := importPrompt(s, src, content)
	if o.DryRun {
		res.Prompt = prompt
		return res, nil
	}
	cmd := o.Command
	if len(cmd) == 0 {
		cmd = s.Cfg.Extract.Commands[src.Kind]
	}
	if len(cmd) == 0 {
		cmd = s.Cfg.Extract.Commands["text"]
	}
	if len(cmd) == 0 {
		return nil, fmt.Errorf("no extractor command (set extract.commands.%s or .text in kb.yaml)", src.Kind)
	}
	out, err := runModel(cmd, prompt, time.Duration(s.Cfg.Extract.TimeoutSec)*time.Second)
	if err != nil {
		return nil, err
	}
	claims, err := ParseClaims(out)
	if err != nil {
		return nil, err
	}
	limit := maxMemoryClaims
	if src.Kind == KindRules {
		limit = maxRuleClaims
	}
	if len(claims) > limit {
		claims = claims[:limit]
	}
	known := s.ProjectFiles()
	srcID := src.Kind + "-" + kb.Slugify(src.Agent.Producer)
	evidence := fmt.Sprintf("saved in %s's memory (%s)", src.Agent.Producer, displayPath(src.Path))
	if src.Kind == KindRules {
		evidence = fmt.Sprintf("user rule written for %s in %s", src.Agent.Producer, displayPath(src.Path))
	}
	for _, c := range claims {
		project := src.Project
		if s.Hub {
			if p := strings.TrimSpace(c.Project); p == kb.GlobalProject {
				project = p
			} else if _, ok := known[p]; ok && p != "" {
				project = p
			}
			if project == "" && s.Project == "" {
				project = kb.GlobalProject
			}
		}
		typ := c.Type
		if src.Kind == KindRules || typ == "" {
			typ = "convention"
		}
		in := kb.AddInput{
			Type: typ, Title: c.Title, Description: c.Description, Body: strings.TrimSpace(c.Body),
			Tags: c.Tags, Resources: c.Resources, Project: project, Actor: src.Agent,
			Evidence: evidence, NoDocCheck: src.Kind == KindRules,
			Sources: []okf.Source{{ID: srcID, Resource: displayPath(src.Path), Author: src.Agent.String(), LastModified: time.Now().UTC().Format(time.RFC3339)}},
		}
		if in.Description == "" {
			in.Description = c.Title
		}
		if in.Body != "" {
			in.Body += fmt.Sprintf(" [^%s]", srcID)
		} else {
			in.Body = in.Description + fmt.Sprintf(" [^%s]", srcID)
		}
		if src.Kind == KindMemory {
			if ms := s.DocCoverage(projectOr(s, project), c.Title+" "+c.Description); len(ms) > 0 && ms[0].Coverage >= kb.DocWarnCoverage {
				res.Documented++
				if o.Out != nil {
					fmt.Fprintf(o.Out, "skip %q: already documented in %s\n", c.Title, ms[0].Section.Ref())
				}
				continue
			}
		}
		r, err := s.Add(in)
		var dup *kb.DuplicateError
		var documented *kb.DocCoveredError
		switch {
		case errors.As(err, &dup):
			res.Dupes++
		case errors.As(err, &documented):
			res.Documented++
		case err != nil:
			if o.Out != nil {
				fmt.Fprintf(o.Out, "skip %q: %v\n", c.Title, err)
			}
		default:
			res.Added = append(res.Added, r.Node.ID)
		}
	}
	return res, saveHash(s, abs, hash)
}

func projectOr(s *kb.Store, p string) string {
	if p == "" {
		return s.Project
	}
	return p
}

func importPrompt(s *kb.Store, src FileSource, content string) string {
	var b strings.Builder
	agent := src.Agent.Producer
	projectField := ""
	if s.Hub {
		var ids []string
		for id := range s.ProjectFiles() {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		def := src.Project
		if def == "" {
			def = s.Project
		}
		if def == "" {
			def = kb.GlobalProject
		}
		projectField = fmt.Sprintf(`, "project": "<one of: %s; default %s>"`, strings.Join(append(ids, kb.GlobalProject), ", "), def)
	}
	switch src.Kind {
	case KindRules:
		fmt.Fprintf(&b, `These are instructions the user wrote for one coding agent (%s) in %s. The other agents (Claude Code, Codex, Hermes, Cursor, ...) never read this file. Split it into atomic rules that every coding agent should follow.

Return ONLY a JSON array (no prose, no code fence) of at most %d objects shaped like:
{"title": "the rule as an instruction, <= 100 chars", "description": "one line of context or the reason"%s}

- Keep only rules that apply to any coding agent; skip instructions about %s's own tools, UI, commands or file locations.
- Keep the user's intent and wording; never invent rules. Skip anything that is not an instruction.
- Do not repeat rules that already exist (listed below). Return [] if nothing qualifies.
`, agent, displayPath(src.Path), maxRuleClaims, projectField, agent)
	default:
		fmt.Fprintf(&b, `These are notes a coding agent (%s) saved in its private memory. The other agents (Claude Code, Codex, Hermes, Cursor, ...) cannot see them. Turn the durable, shareable ones into entries for the knowledge base all agents share.

Return ONLY a JSON array (no prose, no code fence) of at most %d objects shaped like:
{"type": "fact|learning|decision|convention", "title": "specific, <= 90 chars", "description": "one line", "body": "1-4 sentences with the specifics"%s, "tags": ["short"]}

- "convention" = a rule or preference the user wants followed (style, workflow, tools, how to communicate); phrase it as an instruction.
- Skip notes about a single finished task, anything only %s itself can use, the user's identity or contact details, and secrets.
- Do not restate what the repositories' docs already say or entries that already exist (titles below). Return [] if nothing qualifies.
`, agent, maxMemoryClaims, projectField, agent)
	}
	if titles := existingTitles(s, src.Kind == KindRules); len(titles) > 0 {
		b.WriteString("\nExisting entries:\n")
		for _, t := range titles {
			b.WriteString("- " + t + "\n")
		}
	}
	fmt.Fprintf(&b, "\nFile %s:\n\n%s\n", displayPath(src.Path), content)
	return b.String()
}

func existingTitles(s *kb.Store, rulesOnly bool) []string {
	nodes, err := s.Nodes()
	if err != nil {
		return nil
	}
	var out []string
	for _, n := range nodes {
		if n.TypeKey == "review" || (rulesOnly && !s.Cfg.AlwaysOn(n.TypeKey)) || s.Trust(n).State == kb.StateDeprecated {
			continue
		}
		out = append(out, n.Meta().Title)
	}
	sort.Strings(out)
	if len(out) > 150 {
		out = out[len(out)-150:]
	}
	return out
}

func saveHash(s *kb.Store, path, hash string) error {
	p := statePath(s, path)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	data, _ := json.Marshal(state{Path: path, Hash: hash, Updated: time.Now().UTC().Format(time.RFC3339)})
	return os.WriteFile(p, data, 0o644)
}

func displayPath(p string) string {
	if home, err := os.UserHomeDir(); err == nil {
		if rel, err := filepath.Rel(home, p); err == nil && !strings.HasPrefix(rel, "..") {
			return "~/" + filepath.ToSlash(rel)
		}
	}
	return p
}

// --- locating agents' memory and rule files ---

func homeDir() string {
	h, _ := os.UserHomeDir()
	return h
}

func claudeDir() string {
	if d := os.Getenv("CLAUDE_CONFIG_DIR"); d != "" {
		return d
	}
	return filepath.Join(homeDir(), ".claude")
}

func codexDir() string {
	if d := os.Getenv("CODEX_HOME"); d != "" {
		return d
	}
	return filepath.Join(homeDir(), ".codex")
}

// claudeProjectDirName mirrors how Claude Code names per-project folders
// under ~/.claude/projects: the cwd with every non-alphanumeric rune as '-'.
func claudeProjectDirName(path string) string {
	var b strings.Builder
	for _, r := range path {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	return b.String()
}

// MemorySources finds agents' memory files: "claude", "codex", "hermes",
// "antigravity", or "all".
func MemorySources(s *kb.Store, which string) []FileSource {
	var out []FileSource
	add := func(glob string, agent kb.Actor, project string) {
		matches, _ := filepath.Glob(glob)
		for _, m := range matches {
			if st, err := os.Stat(m); err == nil && !st.IsDir() {
				out = append(out, FileSource{Path: m, Kind: KindMemory, Agent: agent, Project: project})
			}
		}
	}
	if which == "claude" || which == "all" {
		// Claude Code auto memory: ~/.claude/projects/<encoded cwd>/memory/*.md
		dirs, _ := filepath.Glob(filepath.Join(claudeDir(), "projects", "*", "memory"))
		for _, d := range dirs {
			project, ok := claudeMemoryProject(s, filepath.Dir(d))
			if !ok {
				continue
			}
			add(filepath.Join(d, "*.md"), kb.Actor{Producer: "claude-code"}, project)
		}
	}
	if which == "codex" || which == "all" {
		add(filepath.Join(codexDir(), "memories", "*.md"), kb.Actor{Producer: "codex"}, globalOrCurrent(s))
	}
	if which == "hermes" || which == "all" {
		for _, f := range []string{"MEMORY.md", "USER.md"} {
			add(filepath.Join(homeDir(), ".hermes", "memories", f), kb.Actor{Producer: "hermes"}, globalOrCurrent(s))
		}
	}
	if which == "antigravity" || which == "all" {
		add(filepath.Join(homeDir(), ".gemini", "antigravity", "knowledge", "*", "artifacts", "*.md"), kb.Actor{Producer: "antigravity"}, globalOrCurrent(s))
	}
	return out
}

// globalOrCurrent: cross-project memory goes to global in a hub (the model
// may route single entries to a project); in repo mode, to the bundle.
func globalOrCurrent(s *kb.Store) string {
	if s.Hub {
		return kb.GlobalProject
	}
	return ""
}

// claudeMemoryProject maps a ~/.claude/projects/<dir> folder to a project:
// first from a transcript's cwd, then by matching registered checkouts.
func claudeMemoryProject(s *kb.Store, projectDir string) (string, bool) {
	cwd := ""
	if ts, _ := filepath.Glob(filepath.Join(projectDir, "*.jsonl")); len(ts) > 0 {
		for _, t := range ts {
			if tr, err := ReadMeta(t); err == nil && tr.CWD != "" {
				cwd = tr.CWD
				break
			}
		}
	}
	name := filepath.Base(projectDir)
	if cwd == "" {
		if s.Hub {
			for id, pf := range s.ProjectFiles() {
				_ = pf
				if co := s.CheckoutFor(id); co != "" && claudeProjectDirName(co) == name {
					return id, true
				}
			}
		} else if s.CodeRoot != "" && claudeProjectDirName(s.CodeRoot) == name {
			return "", true
		}
		return "", false
	}
	if !s.Hub {
		root := s.GitTop()
		if root == "" {
			root = s.CodeRoot
		}
		return "", within(root, cwd)
	}
	t := s.ForDir(cwd)
	return t.Project, t.Project != ""
}

// RuleFileSources finds rule files written for a single agent: user-level
// instruction files (shared as global rules) and, inside a repository,
// agent-specific rule files the other agents don't read. AGENTS.md is left
// out: Codex, Cursor and Antigravity already read it natively.
func RuleFileSources(s *kb.Store, user, project bool) []FileSource {
	var out []FileSource
	add := func(p string, agent string, proj string) {
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			out = append(out, FileSource{Path: p, Kind: KindRules, Agent: kb.Actor{Producer: agent}, Project: proj})
		}
	}
	if user {
		g := globalOrCurrent(s)
		add(filepath.Join(claudeDir(), "CLAUDE.md"), "claude-code", g)
		add(filepath.Join(codexDir(), "AGENTS.md"), "codex", g)
		add(filepath.Join(homeDir(), ".gemini", "GEMINI.md"), "antigravity", g)
	}
	if project && s.CodeRoot != "" {
		root := s.CodeRoot
		add(filepath.Join(root, "CLAUDE.md"), "claude-code", "")
		add(filepath.Join(root, ".claude", "CLAUDE.md"), "claude-code", "")
		add(filepath.Join(root, "GEMINI.md"), "antigravity", "")
		add(filepath.Join(root, ".windsurfrules"), "windsurf", "")
		add(filepath.Join(root, ".github", "copilot-instructions.md"), "copilot", "")
		for _, g := range []struct{ glob, agent string }{
			{".claude/rules/*.md", "claude-code"},
			{".cursor/rules/*.mdc", "cursor"},
			{".cursor/rules/*.md", "cursor"},
			{".agents/rules/*.md", "antigravity"},
		} {
			matches, _ := filepath.Glob(filepath.Join(root, filepath.FromSlash(g.glob)))
			for _, m := range matches {
				add(m, g.agent, "")
			}
		}
	}
	return out
}

// ImportAll imports a list of sources and returns one report line each.
func ImportAll(s *kb.Store, srcs []FileSource, o ImportOptions) []string {
	var report []string
	for _, src := range srcs {
		label := displayPath(src.Path)
		r, err := ImportFile(s, src, o)
		switch {
		case err != nil:
			report = append(report, fmt.Sprintf("%s: error: %v", label, err))
		case r.Skipped != "":
			report = append(report, fmt.Sprintf("%s: skipped (%s)", label, r.Skipped))
		case o.DryRun:
			report = append(report, fmt.Sprintf("%s: would import (%d-char prompt)", label, len(r.Prompt)))
		default:
			report = append(report, fmt.Sprintf("%s: added %d, duplicates %d, already documented %d %v", label, len(r.Added), r.Dupes, r.Documented, r.Added))
		}
	}
	if len(report) == 0 {
		report = append(report, "no files found")
	}
	return report
}
