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
	Content string   // when set, import this part of the file instead of all of it
	// Prepared: Content is the file as its agent loads it (frontmatter and
	// comments removed, imports expanded), even when that leaves nothing.
	Prepared bool
	// Applies makes every rule from this file conditional on matching files.
	Applies *okf.Applicability
	// Unsupported: the file has a condition kb cannot reproduce, so nothing
	// is imported from it. It is still a source: rules it gave earlier,
	// when it had no such condition, must not go on applying more widely
	// than the file now says.
	Unsupported string
}

// ImportOptions controls an import run.
type ImportOptions struct {
	Command  []string // override extractor command
	DryRun   bool     // print the prompt instead of calling the model
	Force    bool     // re-import even if the file is unchanged
	Projects []string // hub: only import what belongs to these projects
	Out      io.Writer
	// AcceptConflicts settles import conflicts after a merge by hand: the
	// entries as they are now become the baseline for their sources.
	AcceptConflicts bool
}

func (o ImportOptions) wants(project string) bool {
	if len(o.Projects) == 0 {
		return true
	}
	for _, p := range o.Projects {
		if p == project {
			return true
		}
	}
	return false
}

var kbBlockRe = regexp.MustCompile(`(?s)<!-- kb:begin.*?<!-- kb:end -->\n?`)

// ImportFile turns a memory or rule file into knowledge entries.
func ImportFile(s *kb.Store, src FileSource, o ImportOptions) (*Result, error) {
	res := &Result{}
	if src.Unsupported != "" {
		res.Skipped = "not imported"
		if o.DryRun {
			return res, nil
		}
		retired, err := retireFrom(s, src, o)
		res.Retired = retired
		return res, err
	}
	data, err := os.ReadFile(src.Path)
	if err != nil {
		return nil, err
	}
	if len(data) > maxImportBytes {
		data = data[:maxImportBytes]
	}
	if src.Content != "" || src.Prepared {
		data = []byte(src.Content)
	}
	// The source's own modification time, not the time of this import.
	var sourceDate time.Time
	if st, err := os.Stat(src.Path); err == nil {
		sourceDate = st.ModTime().UTC()
	}
	// Never re-import kb's own managed blocks (pointer, delivered rules).
	content := strings.TrimSpace(kbBlockRe.ReplaceAllString(string(data), ""))
	hash := importHash(content, src.Applies)
	abs, _ := filepath.Abs(src.Path)
	st := loadState(s, abs)
	// A project-filtered run only takes part of the file, so it neither
	// skips on, nor records, the content hash.
	filtered := len(o.Projects) > 0
	if !o.Force && !filtered && st.Hash == hash {
		res.Skipped = "unchanged since last import"
		return res, nil
	}
	if filtered && src.Project != "" && src.Project != kb.GlobalProject && !o.wants(src.Project) {
		res.Skipped = "not in the selected projects"
		return res, nil
	}
	srcID := sourceID(src)
	minLen := minImportContentLen
	if src.Prepared {
		// Frontmatter and comments are already gone: a one-line rule file
		// is a whole rule, not noise.
		minLen = 10
	}
	if len(content) < minLen {
		// An emptied file states no rules and no condition. The rules it
		// gave earlier keep the condition they have: taking "no condition"
		// from a file with nothing in it would make them apply everywhere.
		res.Skipped = "nothing to import"
		if !o.DryRun {
			return res, saveHash(s, abs, hash)
		}
		return res, nil
	}
	if src.Kind == KindRules && !o.DryRun {
		// The file says which files its rules are for. Rules it gave
		// earlier follow a changed condition before anything is added.
		rescoped, err := reconcileApplies(s, src, srcID, o)
		if err != nil {
			return nil, err
		}
		res.Rescoped = rescoped
	}
	prompt := importPrompt(s, src, content)
	if filtered {
		prompt = strings.Replace(prompt, "\nFile ", fmt.Sprintf("\nOnly extract notes about these projects: %s. Ignore everything else.\n\nFile ", strings.Join(o.Projects, ", ")), 1)
	}
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
		if filtered && !o.wants(projectOr(s, project)) {
			continue
		}
		typ := c.Type
		if src.Kind == KindRules || typ == "" {
			typ = "convention"
		}
		in := kb.AddInput{
			Type: typ, Title: c.Title, Description: c.Description, Body: strings.TrimSpace(c.Body),
			Tags: c.Tags, Resources: c.Resources, Project: project, Actor: src.Agent,
			Evidence: evidence, NoDocCheck: src.Kind == KindRules, AppliesTo: src.Applies,
			// The default project comes from a real repository (or is global).
			NewProject: project == src.Project,
			Sources: []okf.Source{{ID: srcID, Resource: displayPath(src.Path), Author: src.Agent.String(),
				LastModified: sourceDate.Format(time.RFC3339), IngestedAt: s.Now().UTC().Format(time.RFC3339)}},
			// Ingested, not checked against the code now: no commit, and the
			// deadline counts from the file's own date.
			SourceOnly: true, SourceDate: sourceDate, SourceDateBasis: kb.BasisSourceMtime,
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
	if filtered {
		return res, nil
	}
	return res, saveHash(s, abs, hash)
}

// importHash identifies what an import took from a file: its content and,
// for a conditional rule file, the condition, which lives in frontmatter the
// content no longer has.
func importHash(content string, applies *okf.Applicability) string {
	if applies != nil && len(applies.Paths) > 0 {
		content += "\x00applies_to\x00" + applies.Base + "\x00" + applies.Origin + "\x00" + strings.Join(applies.Paths, "\x00")
	}
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}

// sourceID names a file's kind and agent on the entries it gives.
func sourceID(src FileSource) string { return src.Kind + "-" + kb.Slugify(src.Agent.Producer) }

// rulesFrom lists the rules in force that a file gave earlier.
func rulesFrom(s *kb.Store, src FileSource, o ImportOptions) ([]*kb.Node, error) {
	nodes, err := s.Nodes()
	if err != nil {
		return nil, err
	}
	id, resource := sourceID(src), displayPath(src.Path)
	var out []*kb.Node
	for _, n := range nodes {
		if !s.Cfg.AlwaysOn(n.TypeKey) || s.Trust(n).State == kb.StateDeprecated {
			continue
		}
		if len(o.Projects) > 0 && !o.wants(n.Project) {
			continue
		}
		for _, from := range n.Meta().Sources {
			if from.ID == id && from.Resource == resource {
				out = append(out, n)
				break
			}
		}
	}
	return out, nil
}

// retireFrom retires the rules a file gave earlier, now that the file has
// a condition kb cannot reproduce. Keeping them would apply them where the
// file no longer says they apply; kb cannot narrow them the same way, so
// they stop being delivered. It returns the entries it retired.
func retireFrom(s *kb.Store, src FileSource, o ImportOptions) ([]string, error) {
	rules, err := rulesFrom(s, src, o)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, n := range rules {
		reason := "its rule file (" + displayPath(src.Path) + ") now has a condition kb cannot reproduce: " + src.Unsupported
		if _, err := s.Deprecate(n.ID, reason, src.Agent); err != nil {
			return out, fmt.Errorf("%s: %w", n.ID, err)
		}
		out = append(out, n.ID)
	}
	return out, nil
}

// reconcileApplies gives the rules a file produced earlier the condition
// the file has now. It returns the entries it changed.
func reconcileApplies(s *kb.Store, src FileSource, srcID string, o ImportOptions) ([]string, error) {
	rules, err := rulesFrom(s, src, o)
	if err != nil {
		return nil, err
	}
	resource := displayPath(src.Path)
	var stale []*kb.Node
	for _, n := range rules {
		if !kb.SameApplies(n.Meta().AppliesTo, src.Applies) {
			stale = append(stale, n)
		}
	}
	var date time.Time
	if st, err := os.Stat(src.Path); err == nil {
		date = st.ModTime().UTC()
	}
	var out []string
	for _, n := range stale {
		reason := "its rule file no longer limits it to particular files"
		if src.Applies != nil && len(src.Applies.Paths) > 0 {
			reason = "its rule file now applies " + kb.AppliesNote(src.Applies)
		}
		// The condition is the user's own, like the rule: a rule in force
		// stays in force instead of waiting for a confirmation. One that was
		// not trusted before is not confirmed by a change of its condition.
		evidence := ""
		if s.Trust(n).Trusted() {
			evidence = fmt.Sprintf("user rule written for %s in %s", src.Agent.Producer, resource)
		}
		if _, _, err := s.Update(kb.UpdateInput{Ref: n.ID, Reason: reason, Actor: src.Agent, AppliesTo: src.Applies, SetAppliesTo: true, Evidence: evidence,
			SourceOnly: true, SourceDate: date, SourceDateBasis: kb.BasisSourceMtime}); err != nil {
			return out, fmt.Errorf("%s: %w", n.ID, err)
		}
		out = append(out, n.ID)
	}
	return out, nil
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
		// A rule that exists for other files only does not cover this file.
		if src.Applies != nil && len(src.Applies.Paths) > 0 {
			fmt.Fprintf(&b, "- The rules in this file apply only %s. An existing rule marked for other files is not a repeat: return it for this file too.\n", kb.AppliesNote(src.Applies))
		} else {
			b.WriteString("- The rules in this file apply everywhere. An existing rule marked \"only ...\" covers those files alone and is not a repeat: return it.\n")
		}
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
		title := n.Meta().Title
		if rulesOnly && kb.Conditional(n.Meta()) {
			title += " (only " + kb.AppliesNote(n.Meta().AppliesTo) + ")"
		}
		out = append(out, title)
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

// MemorySources finds the memory files of agents without a typed format:
// "codex", "hermes", "antigravity", or "all". They go through the model.
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
	// Claude Code's auto memory has its own adapter (ImportClaudeMemory).
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
			// Any registered checkout of a project (a worktree has its own
			// Claude project directory), not just the baseline one.
			for id := range s.ProjectFiles() {
				for _, c := range s.Checkouts(id) {
					if claudeProjectDirName(c.Path) == name {
						return id, true
					}
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

// ImportAll imports a list of sources and returns one report line each.
func ImportAll(s *kb.Store, srcs []FileSource, o ImportOptions) []string {
	var report []string
	for _, src := range srcs {
		label := displayPath(src.Path)
		r, err := ImportFile(s, src, o)
		switch {
		case err != nil:
			report = append(report, fmt.Sprintf("%s: error: %v", label, err))
		case src.Unsupported != "":
			// Why it is not imported is already in the notes.
			if len(r.Retired) > 0 {
				report = append(report, fmt.Sprintf("%s: it can no longer be imported, so %d rule(s) it gave earlier were retired instead of staying in force more widely than the file says %v", label, len(r.Retired), r.Retired))
			}
		case r.Skipped != "":
			report = append(report, fmt.Sprintf("%s: skipped (%s)", label, r.Skipped))
		case o.DryRun:
			report = append(report, fmt.Sprintf("%s: would import (%d-char prompt)", label, len(r.Prompt)))
		default:
			report = append(report, fmt.Sprintf("%s: added %d, duplicates %d, already documented %d %v", label, len(r.Added), r.Dupes, r.Documented, r.Added))
		}
		if err == nil && len(r.Rescoped) > 0 {
			report = append(report, fmt.Sprintf("%s: its condition changed; %d rule(s) follow it %v", label, len(r.Rescoped), r.Rescoped))
		}
	}
	if len(report) == 0 && len(srcs) == 0 {
		report = append(report, "no files found")
	}
	return report
}
