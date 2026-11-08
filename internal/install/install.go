// Package install scaffolds knowledge bases (a multi-project hub or a
// single-repo bundle) and wires them into each agent's native, always-loaded
// context: pointer text, skills and hooks, either user-wide or per repo.
package install

import (
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/xuehaipeng/agent-kb/internal/kb"
)

//go:embed templates/pointer.md
var pointerTmpl string

//go:embed templates/bundle-readme.md
var readmeTmpl string

//go:embed templates/hub-readme.md
var hubReadmeTmpl string

//go:embed templates/skill.md
var skillTmpl string

//go:embed templates/hermes-skill.md
var hermesSkillTmpl string

const (
	mdBegin = "<!-- kb:begin (managed by 'kb install'; re-run it to update, edits here are overwritten) -->"
	mdEnd   = "<!-- kb:end -->"
	shBegin = "# kb:begin (managed by 'kb install')"
	shEnd   = "# kb:end"
)

const gitattributes = "# Append-only and generated files merge by union; 'kb index' then rewrites indexes.\nlog.md merge=union\nindex.md merge=union\n**/index.md merge=union\n"

// Result lists what an installer changed plus instructions to show the user.
type Result struct {
	Changed []string
	Notes   []string
}

func (r *Result) changed(path string, did bool) {
	if did {
		r.Changed = append(r.Changed, path)
	}
}

// Pointer is the always-on pointer text (same for every agent and mode).
func Pointer() string { return pointerTmpl }

// Skill is the progressive-disclosure skill.
func Skill() string { return skillTmpl }

// Init creates a single-repo bundle at <repo>/<name> (repo mode).
func Init(repoRoot, name string) (*kb.Store, *Result, error) {
	if name == "" {
		name = kb.DefaultBundleDir
	}
	dir := filepath.Join(repoRoot, name)
	res := &Result{}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, nil, err
	}
	cfgPath := filepath.Join(dir, kb.ConfigFile)
	if _, err := os.Stat(cfgPath); os.IsNotExist(err) {
		if err := os.WriteFile(cfgPath, []byte(kb.DefaultConfigYAML), 0o644); err != nil {
			return nil, nil, err
		}
		res.Changed = append(res.Changed, cfgPath)
	}
	s, err := kb.Open(dir)
	if err != nil {
		return nil, nil, err
	}
	if err := scaffold(s, dir, readmeTmpl, res); err != nil {
		return nil, nil, err
	}
	return s, res, nil
}

// scaffold writes the shared files of a bundle or hub and its indexes.
func scaffold(s *kb.Store, dir, readme string, res *Result) error {
	for _, b := range s.Bundles() {
		for _, tk := range s.Cfg.TypeNames() {
			if err := os.MkdirAll(filepath.Join(b.Dir, s.Cfg.Types[tk].Dir), 0o755); err != nil {
				return err
			}
		}
	}
	files := map[string]string{
		"README.md":      readme,
		"POINTER.md":     Pointer(),
		".gitignore":     kb.LocalDir + "/\n",
		".gitattributes": gitattributes,
	}
	for rel, content := range files {
		p := filepath.Join(dir, rel)
		if rel != "POINTER.md" && fileExists(p) {
			continue // user-owned after creation
		}
		did, err := kb.WriteFileIfChanged(p, []byte(content))
		if err != nil {
			return err
		}
		res.changed(p, did)
	}
	logPath := filepath.Join(dir, "log.md")
	if !fileExists(logPath) {
		if err := os.WriteFile(logPath, []byte("# Knowledge log\n\nAppend-only record of changes made through `kb`. Newest last. Do not edit.\n\n"), 0o644); err != nil {
			return err
		}
		res.Changed = append(res.Changed, logPath)
	}
	changed, err := s.WriteIndex()
	if err != nil {
		return err
	}
	for _, c := range changed {
		res.Changed = append(res.Changed, filepath.Join(dir, c))
	}
	return nil
}

// HubInit creates (or completes) a multi-project hub as a git repository.
func HubInit(dir, remote string) (*Result, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	res := &Result{}
	if err := os.MkdirAll(filepath.Join(abs, "projects"), 0o755); err != nil {
		return nil, err
	}
	cfgPath := filepath.Join(abs, kb.ConfigFile)
	if !fileExists(cfgPath) {
		if err := os.WriteFile(cfgPath, []byte(kb.HubConfigYAML()), 0o644); err != nil {
			return nil, err
		}
		res.Changed = append(res.Changed, cfgPath)
	}
	keep := filepath.Join(abs, "projects", ".gitkeep")
	if !fileExists(keep) {
		_ = os.WriteFile(keep, nil, 0o644)
	}
	s, err := kb.Open(abs)
	if err != nil {
		return nil, err
	}
	if !s.Hub {
		return nil, fmt.Errorf("%s has a kb.yaml without 'mode: hub'", cfgPath)
	}
	if err := scaffold(s, abs, hubReadmeTmpl, res); err != nil {
		return nil, err
	}
	if _, err := kb.Git(abs, "rev-parse", "--git-dir"); err != nil {
		if _, err := kb.Git(abs, "init", "-q", "-b", "main"); err != nil {
			return nil, err
		}
	}
	_, _ = kb.Git(abs, "add", "-A")
	if _, err := kb.Git(abs, "diff", "--cached", "--quiet"); err != nil {
		if _, err := kb.Git(abs, "commit", "-q", "--no-verify", "-m", "kb: initialise knowledge hub"); err != nil {
			res.Notes = append(res.Notes, "could not commit (set git user.name/user.email): "+err.Error())
		}
	}
	if remote != "" {
		if _, err := kb.Git(abs, "remote", "get-url", "origin"); err == nil {
			_, err = kb.Git(abs, "remote", "set-url", "origin", remote)
			if err != nil {
				return nil, err
			}
		} else if _, err := kb.Git(abs, "remote", "add", "origin", remote); err != nil {
			return nil, err
		}
		res.Notes = append(res.Notes, "remote origin = "+remote+"; run 'kb sync' to push")
	}
	return res, nil
}

// HubClone puts an existing hub on this machine (laptop or Hermes host).
func HubClone(url, dir string) error {
	if entries, err := os.ReadDir(dir); err == nil && len(entries) > 0 {
		return fmt.Errorf("%s already exists and is not empty", dir)
	}
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return err
	}
	if _, err := kb.Git(filepath.Dir(dir), "clone", "-q", url, dir); err != nil {
		return err
	}
	s, err := kb.Open(dir)
	if err != nil {
		return err
	}
	if !s.Hub {
		return fmt.Errorf("%s is not a kb hub (kb.yaml lacks 'mode: hub')", url)
	}
	return nil
}

// HubImport copies a repo bundle's entries into a hub project.
func HubImport(s *kb.Store, bundleDir, project string) (int, string, error) {
	if !s.Hub {
		return 0, "", errors.New("import needs a hub (kb hub init)")
	}
	src, err := filepath.Abs(bundleDir)
	if err != nil {
		return 0, "", err
	}
	if !fileExists(filepath.Join(src, kb.ConfigFile)) {
		return 0, "", fmt.Errorf("%s is not a kb bundle (no kb.yaml)", src)
	}
	if project == "" {
		project = s.Project
	}
	if project == "" {
		return 0, "", errors.New("cannot tell the project: pass -p <id>")
	}
	if err := s.EnsureProject(project); err != nil {
		return 0, "", err
	}
	dst := s.BundleDir(project)
	n := 0
	for _, tk := range s.Cfg.TypeNames() {
		dir := s.Cfg.Types[tk].Dir
		root := filepath.Join(src, dir)
		err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				if errors.Is(err, fs.ErrNotExist) {
					return nil
				}
				return err
			}
			if d.IsDir() || !strings.HasSuffix(d.Name(), ".md") || strings.EqualFold(d.Name(), "index.md") {
				return nil
			}
			rel, _ := filepath.Rel(src, p)
			target := filepath.Join(dst, rel)
			if fileExists(target) {
				return nil
			}
			data, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			if _, err := kb.WriteFileIfChanged(target, data); err != nil {
				return err
			}
			n++
			return nil
		})
		if err != nil {
			return n, project, err
		}
	}
	s.Invalidate()
	return n, project, s.RecordChange("import", project, kb.Actor{Producer: "kb"}, fmt.Sprintf("%d entries from %s", n, src))
}

func repoRoot(s *kb.Store) string {
	if top := s.GitTop(); top != "" {
		return top
	}
	return s.CodeRoot
}

// bundleRel is a repo bundle's path relative to its repo ("knowledge").
func bundleRel(s *kb.Store) string {
	base := repoRoot(s)
	if rel, err := filepath.Rel(base, s.Dir); err == nil && !strings.HasPrefix(rel, "..") {
		return filepath.ToSlash(rel)
	}
	return filepath.Base(s.Dir)
}

// Targets lists supported install targets.
var Targets = []string{"claude", "codex", "cursor", "antigravity", "hermes", "git-hooks"}

// Run installs one target. opts["scope"] is "global", "repo" or "" (hub
// stores install user-wide, repo bundles into the repo).
func Run(s *kb.Store, target string, opts map[string]string) (*Result, error) {
	global := s.Hub
	switch opts["scope"] {
	case "global":
		global = true
	case "repo":
		global = false
	}
	if !global && s.Hub && s.CodeRoot == "" {
		return nil, errors.New("--repo needs to run inside a repository")
	}
	switch target {
	case "claude", "claude-code":
		return Claude(s, global)
	case "codex":
		return Codex(s, global)
	case "cursor":
		return Cursor(s, global)
	case "antigravity", "gemini":
		return Antigravity(s, global)
	case "hermes":
		return Hermes(s, opts["skills-dir"])
	case "git-hooks", "hooks":
		return GitHooks(s)
	}
	return nil, fmt.Errorf("unknown target %q (one of: %s)", target, strings.Join(Targets, ", "))
}

func claudeDir() string {
	if d := os.Getenv("CLAUDE_CONFIG_DIR"); d != "" {
		return d
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".claude")
}

func codexDir() string {
	if d := os.Getenv("CODEX_HOME"); d != "" {
		return d
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".codex")
}

func geminiDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".gemini")
}

// RefreshPointer rewrites POINTER.md and every managed pointer block that
// already exists (repo AGENTS.md, user-level AGENTS.md / GEMINI.md).
func RefreshPointer(s *kb.Store) (*Result, error) {
	res := &Result{}
	p := filepath.Join(s.Dir, "POINTER.md")
	did, err := kb.WriteFileIfChanged(p, []byte(Pointer()))
	if err != nil {
		return nil, err
	}
	res.changed(p, did)
	candidates := []string{filepath.Join(codexDir(), "AGENTS.md"), filepath.Join(geminiDir(), "GEMINI.md")}
	if !s.Hub && repoRoot(s) != "" {
		candidates = append(candidates, filepath.Join(repoRoot(s), "AGENTS.md"))
	}
	for _, f := range candidates {
		if data, err := os.ReadFile(f); err == nil && strings.Contains(string(data), mdBegin) {
			did, err := upsertBlock(f, Pointer(), mdBegin, mdEnd)
			if err != nil {
				return nil, err
			}
			res.changed(f, did)
		}
	}
	return res, nil
}

// importPath is the CLAUDE.md @-import of the pointer file.
func importPath(s *kb.Store, global bool) string {
	p := filepath.Join(s.Dir, "POINTER.md")
	if !global {
		return "@" + bundleRel(s) + "/POINTER.md"
	}
	if home, err := os.UserHomeDir(); err == nil {
		if rel, err := filepath.Rel(kb.RealPath(home), p); err == nil && !strings.HasPrefix(rel, "..") {
			return "@~/" + filepath.ToSlash(rel)
		}
	}
	return "@" + p
}

// Claude wires Claude Code: CLAUDE.md import, skill, hooks, permissions.
func Claude(s *kb.Store, global bool) (*Result, error) {
	res := &Result{}
	if r, err := RefreshPointer(s); err == nil {
		res.Changed = append(res.Changed, r.Changed...)
	} else {
		return nil, err
	}
	base := claudeDir()
	claudeMD := filepath.Join(base, "CLAUDE.md")
	settings := filepath.Join(base, "settings.json")
	if !global {
		base = filepath.Join(repoRoot(s), ".claude")
		claudeMD = filepath.Join(repoRoot(s), "CLAUDE.md")
		settings = filepath.Join(base, "settings.json")
	}
	did, err := upsertBlock(claudeMD, importPath(s, global), mdBegin, mdEnd)
	if err != nil {
		return nil, err
	}
	res.changed(claudeMD, did)
	skill := filepath.Join(base, "skills", "kb", "SKILL.md")
	did, err = kb.WriteFileIfChanged(skill, []byte(Skill()))
	if err != nil {
		return nil, err
	}
	res.changed(skill, did)
	did, err = mergeClaudeSettings(settings)
	if err != nil {
		return nil, err
	}
	res.changed(settings, did)
	scope := "every repository (user-wide)"
	if !global {
		scope = "this repository"
	}
	res.Notes = append(res.Notes,
		"Claude Code now loads the kb pointer in "+scope+". Hooks: SessionStart prints reviews/disputes/stale entries; PreCompact and SessionEnd run 'kb extract' in the background (needs the 'claude' CLI; see extract.commands in kb.yaml).",
		"Optional MCP instead of the CLI: claude mcp add kb -- kb mcp --as claude-code")
	return res, nil
}

var claudeHookEvents = []string{"SessionStart", "PreCompact", "SessionEnd"}

const claudeHookCmd = "kb hook claude"

var claudeAllow = []string{"Bash(kb search:*)", "Bash(kb show:*)", "Bash(kb status:*)", "Bash(kb add:*)", "Bash(kb update:*)", "Bash(kb verify:*)", "Bash(kb deprecate:*)", "Bash(kb review:*)", "Bash(kb project:*)"}

func mergeClaudeSettings(path string) (bool, error) {
	root := map[string]any{}
	if data, err := os.ReadFile(path); err == nil && len(strings.TrimSpace(string(data))) > 0 {
		if err := json.Unmarshal(data, &root); err != nil {
			return false, fmt.Errorf("%s: %w (fix the JSON or add the hooks by hand)", path, err)
		}
	}
	hooks, _ := root["hooks"].(map[string]any)
	if hooks == nil {
		hooks = map[string]any{}
	}
	for _, ev := range claudeHookEvents {
		groups, _ := hooks[ev].([]any)
		present := false
		for _, g := range groups {
			gm, _ := g.(map[string]any)
			hs, _ := gm["hooks"].([]any)
			for _, h := range hs {
				hm, _ := h.(map[string]any)
				if c, _ := hm["command"].(string); strings.HasPrefix(c, claudeHookCmd) {
					present = true
				}
			}
		}
		if !present {
			groups = append(groups, map[string]any{"hooks": []any{map[string]any{"type": "command", "command": claudeHookCmd, "timeout": 30}}})
		}
		hooks[ev] = groups
	}
	root["hooks"] = hooks
	perms, _ := root["permissions"].(map[string]any)
	if perms == nil {
		perms = map[string]any{}
	}
	allow, _ := perms["allow"].([]any)
	have := map[string]bool{}
	for _, a := range allow {
		if s, ok := a.(string); ok {
			have[s] = true
		}
	}
	for _, a := range claudeAllow {
		if !have[a] {
			allow = append(allow, a)
		}
	}
	perms["allow"] = allow
	root["permissions"] = perms
	out, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return false, err
	}
	return kb.WriteFileIfChanged(path, append(out, '\n'))
}

// pointerBlock writes the pointer into an AGENTS.md-style file.
func pointerBlock(path string, res *Result) error {
	did, err := upsertBlock(path, Pointer(), mdBegin, mdEnd)
	if err != nil {
		return err
	}
	res.changed(path, did)
	return nil
}

// Codex wires Codex: AGENTS.md pointer block and a skill.
func Codex(s *kb.Store, global bool) (*Result, error) {
	res := &Result{}
	agents := filepath.Join(codexDir(), "AGENTS.md")
	skill := filepath.Join(codexDir(), "skills", "kb", "SKILL.md")
	if !global {
		agents = filepath.Join(repoRoot(s), "AGENTS.md")
		skill = filepath.Join(repoRoot(s), ".codex", "skills", "kb", "SKILL.md")
	}
	if err := pointerBlock(agents, res); err != nil {
		return nil, err
	}
	did, err := kb.WriteFileIfChanged(skill, []byte(Skill()))
	if err != nil {
		return nil, err
	}
	res.changed(skill, did)
	scanDir := s.Dir
	if !global {
		scanDir = repoRoot(s)
	}
	res.Notes = append(res.Notes,
		"Attribute Codex writes: add to ~/.codex/config.toml\n    [shell_environment_policy]\n    set = { KB_ACTOR = \"codex\" }",
		"Optional MCP instead of the CLI, in ~/.codex/config.toml:\n    [mcp_servers.kb]\n    command = \"kb\"\n    args = [\"mcp\", \"--as\", \"codex\"]",
		"Capture: Codex has no session hooks, so schedule the importer (it routes each session to its project), e.g. crontab:\n    17 2 * * * cd "+scanDir+" && kb extract --scan codex >> "+s.LocalPath("extract.log")+" 2>&1",
		"Check that your Codex version reads skills from "+filepath.Dir(filepath.Dir(skill))+".")
	return res, nil
}

// Cursor: it reads AGENTS.md natively; user-wide rules are a settings field.
func Cursor(s *kb.Store, global bool) (*Result, error) {
	res := &Result{}
	if global {
		res.Notes = append(res.Notes,
			"Cursor keeps user-wide rules in Settings > Rules (no file to write). Paste the pointer there: kb pointer",
			"Set KB_ACTOR for Cursor's terminal (e.g. KB_ACTOR=cursor/<model>).")
		return res, nil
	}
	if err := pointerBlock(filepath.Join(repoRoot(s), "AGENTS.md"), res); err != nil {
		return nil, err
	}
	res.Notes = append(res.Notes, "Cursor reads AGENTS.md natively; set KB_ACTOR=cursor/<model> in its terminal environment.")
	return res, nil
}

// Antigravity: user-wide via ~/.gemini/GEMINI.md, per repo via AGENTS.md.
func Antigravity(s *kb.Store, global bool) (*Result, error) {
	res := &Result{}
	target := filepath.Join(repoRoot(s), "AGENTS.md")
	if global {
		target = filepath.Join(geminiDir(), "GEMINI.md")
	}
	if err := pointerBlock(target, res); err != nil {
		return nil, err
	}
	res.Notes = append(res.Notes, "Set KB_ACTOR=antigravity/<model> in Antigravity's terminal environment.")
	return res, nil
}

// Hermes writes the Hermes skill and prints the hub sync setup.
func Hermes(s *kb.Store, skillsDir string) (*Result, error) {
	res := &Result{}
	if skillsDir == "" {
		home, _ := os.UserHomeDir()
		skillsDir = filepath.Join(home, ".hermes", "skills")
	}
	p := filepath.Join(skillsDir, "kb", "SKILL.md")
	did, err := kb.WriteFileIfChanged(p, []byte(hermesSkillTmpl))
	if err != nil {
		return nil, err
	}
	res.changed(p, did)
	res.Notes = append(res.Notes,
		"On the Hermes host: install kb, run 'kb hub clone <your hub remote>', and export KB_ACTOR=hermes/<model> in Hermes' environment.",
		"Keep it in sync with cron:\n    */15 * * * * KB_ACTOR=hermes kb sync >> ~/.kb/.kb/sync.log 2>&1",
		"Add one line to ~/.hermes/memories/MEMORY.md: 'Use the kb skill (kb search --all / kb show / kb add -p <project>) for project questions.'",
		"Optional MCP in Hermes config: mcp_servers: {kb: {command: kb, args: [mcp, --as, hermes]}}")
	return res, nil
}

// GitHooks installs pre-commit lint and post-merge reindex hooks in the
// repository that holds the knowledge files (the hub repo in hub mode).
func GitHooks(s *kb.Store) (*Result, error) {
	res := &Result{}
	top := s.KnowledgeTop()
	if top == "" {
		return nil, errors.New("the knowledge files are not in a git repository")
	}
	if runtime.GOOS == "windows" {
		return nil, errors.New("git hooks installer supports unix shells only")
	}
	hooksDir, err := kb.Git(top, "rev-parse", "--git-path", "hooks")
	if err != nil {
		return nil, err
	}
	if !filepath.IsAbs(hooksDir) {
		hooksDir = filepath.Join(top, hooksDir)
	}
	rel := "."
	if r, err := filepath.Rel(top, s.Dir); err == nil {
		rel = filepath.ToSlash(r)
	}
	hooks := map[string]string{
		"pre-commit": fmt.Sprintf("if command -v kb >/dev/null 2>&1 && ! git diff --cached --quiet -- %q; then\n  kb lint --staged --quiet --root %q || { echo 'kb lint failed (run: kb lint; kb index)'; exit 1; }\nfi", rel, s.Dir),
		"post-merge": fmt.Sprintf("command -v kb >/dev/null 2>&1 && kb index --quiet --root %q || true", s.Dir),
	}
	for name, body := range hooks {
		p := filepath.Join(hooksDir, name)
		if !fileExists(p) {
			if err := os.MkdirAll(hooksDir, 0o755); err != nil {
				return nil, err
			}
			if err := os.WriteFile(p, []byte("#!/bin/sh\n"), 0o755); err != nil {
				return nil, err
			}
		}
		did, err := upsertBlock(p, body, shBegin, shEnd)
		if err != nil {
			return nil, err
		}
		if err := os.Chmod(p, 0o755); err != nil {
			return nil, err
		}
		res.changed(p, did)
	}
	return res, nil
}

// upsertBlock inserts or replaces a marker-delimited block in a file.
func upsertBlock(path, content, begin, end string) (bool, error) {
	data, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return false, err
	}
	s := string(data)
	block := begin + "\n" + strings.TrimRight(content, "\n") + "\n" + end + "\n"
	if i := strings.Index(s, begin); i >= 0 {
		j := strings.Index(s[i:], end)
		if j < 0 {
			return false, fmt.Errorf("%s: found %q without %q", path, begin, end)
		}
		after := s[i+j+len(end):]
		after = strings.TrimPrefix(after, "\n")
		s = s[:i] + block + after
	} else {
		if s != "" && !strings.HasSuffix(s, "\n") {
			s += "\n"
		}
		if s != "" && !strings.HasSuffix(s, "\n\n") && !strings.HasPrefix(begin, "#") {
			s += "\n"
		}
		s += block
	}
	return kb.WriteFileIfChanged(path, []byte(s))
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
