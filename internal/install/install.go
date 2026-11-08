// Package install scaffolds a knowledge bundle and wires it into each agent's
// native, always-loaded context (pointer files, skills, hooks).
package install

import (
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
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

// Pointer renders the always-on pointer text for a bundle path such as "knowledge".
func Pointer(bundleRel string) string {
	return strings.ReplaceAll(pointerTmpl, "{{BUNDLE}}", bundleRel)
}

// Skill renders the progressive-disclosure skill.
func Skill(bundleRel string) string {
	return strings.ReplaceAll(skillTmpl, "{{BUNDLE}}", bundleRel)
}

// Init creates a bundle at <repo>/<name> with config, README, pointer, local
// gitignore and union-merge attributes for the append-only/generated files.
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
	for _, tk := range s.Cfg.TypeNames() {
		if err := os.MkdirAll(filepath.Join(dir, s.Cfg.Types[tk].Dir), 0o755); err != nil {
			return nil, nil, err
		}
	}
	files := map[string]string{
		"README.md":      readmeTmpl,
		"POINTER.md":     Pointer(filepath.ToSlash(bundleRel(s))),
		".gitignore":     kb.LocalDir + "/\n",
		".gitattributes": "# Append-only and generated files merge by union; 'kb index' then rewrites indexes.\nlog.md merge=union\nindex.md merge=union\n*/index.md merge=union\n",
	}
	for rel, content := range files {
		p := filepath.Join(dir, rel)
		if rel == "README.md" || rel == ".gitattributes" || rel == ".gitignore" {
			if _, err := os.Stat(p); err == nil {
				continue // user-owned after creation
			}
		}
		did, err := kb.WriteFileIfChanged(p, []byte(content))
		if err != nil {
			return nil, nil, err
		}
		res.changed(p, did)
	}
	logPath := filepath.Join(dir, "log.md")
	if _, err := os.Stat(logPath); os.IsNotExist(err) {
		if err := os.WriteFile(logPath, []byte("# Knowledge log\n\nAppend-only record of changes made through `kb`. Newest last. Do not edit.\n\n"), 0o644); err != nil {
			return nil, nil, err
		}
		res.Changed = append(res.Changed, logPath)
	}
	changed, err := s.WriteIndex()
	if err != nil {
		return nil, nil, err
	}
	for _, c := range changed {
		res.Changed = append(res.Changed, filepath.Join(dir, c))
	}
	return s, res, nil
}

// bundleRel is the bundle path relative to the code root ("knowledge").
func bundleRel(s *kb.Store) string {
	base := s.GitTop()
	if base == "" {
		base = s.CodeRoot
	}
	if rel, err := filepath.Rel(base, s.Dir); err == nil && !strings.HasPrefix(rel, "..") {
		return filepath.ToSlash(rel)
	}
	return filepath.Base(s.Dir)
}

func repoRoot(s *kb.Store) string {
	if top := s.GitTop(); top != "" {
		return top
	}
	return s.CodeRoot
}

// Targets lists supported install targets.
var Targets = []string{"claude", "codex", "cursor", "antigravity", "hermes", "git-hooks"}

// Run installs one target.
func Run(s *kb.Store, target string, opts map[string]string) (*Result, error) {
	switch target {
	case "claude", "claude-code":
		return Claude(s)
	case "codex":
		return Codex(s)
	case "cursor":
		return Cursor(s)
	case "antigravity", "gemini":
		return Antigravity(s)
	case "hermes":
		return Hermes(s, opts["skills-dir"])
	case "git-hooks", "hooks":
		return GitHooks(s)
	}
	return nil, fmt.Errorf("unknown target %q (one of: %s)", target, strings.Join(Targets, ", "))
}

// RefreshPointer rewrites POINTER.md and any existing AGENTS.md block.
func RefreshPointer(s *kb.Store) (*Result, error) {
	res := &Result{}
	rel := bundleRel(s)
	did, err := kb.WriteFileIfChanged(filepath.Join(s.Dir, "POINTER.md"), []byte(Pointer(rel)))
	if err != nil {
		return nil, err
	}
	res.changed(filepath.Join(s.Dir, "POINTER.md"), did)
	agents := filepath.Join(repoRoot(s), "AGENTS.md")
	if data, err := os.ReadFile(agents); err == nil && strings.Contains(string(data), mdBegin) {
		did, err := upsertBlock(agents, Pointer(rel), mdBegin, mdEnd)
		if err != nil {
			return nil, err
		}
		res.changed(agents, did)
	}
	return res, nil
}

// Claude wires Claude Code: CLAUDE.md import, skill, hooks, permissions.
func Claude(s *kb.Store) (*Result, error) {
	res := &Result{}
	root := repoRoot(s)
	rel := bundleRel(s)
	if _, err := RefreshPointer(s); err != nil {
		return nil, err
	}
	did, err := upsertBlock(filepath.Join(root, "CLAUDE.md"), "@"+rel+"/POINTER.md", mdBegin, mdEnd)
	if err != nil {
		return nil, err
	}
	res.changed(filepath.Join(root, "CLAUDE.md"), did)
	skill := filepath.Join(root, ".claude", "skills", "kb", "SKILL.md")
	did, err = kb.WriteFileIfChanged(skill, []byte(Skill(rel)))
	if err != nil {
		return nil, err
	}
	res.changed(skill, did)
	settings := filepath.Join(root, ".claude", "settings.json")
	did, err = mergeClaudeSettings(settings)
	if err != nil {
		return nil, err
	}
	res.changed(settings, did)
	res.Notes = append(res.Notes,
		"Claude Code hooks: SessionStart prints reviews/disputes waiting; PreCompact and SessionEnd run 'kb extract' in the background (needs the 'claude' CLI; see extract.commands in kb.yaml).",
		"Optional MCP instead of the CLI: claude mcp add kb -- kb mcp --as claude-code")
	return res, nil
}

var claudeHookEvents = []string{"SessionStart", "PreCompact", "SessionEnd"}

const claudeHookCmd = "kb hook claude"

var claudeAllow = []string{"Bash(kb search:*)", "Bash(kb show:*)", "Bash(kb status:*)", "Bash(kb add:*)", "Bash(kb verify:*)", "Bash(kb review:*)"}

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

func agentsMD(s *kb.Store, res *Result) error {
	p := filepath.Join(repoRoot(s), "AGENTS.md")
	did, err := upsertBlock(p, Pointer(bundleRel(s)), mdBegin, mdEnd)
	if err != nil {
		return err
	}
	res.changed(p, did)
	return nil
}

// Codex wires Codex: AGENTS.md pointer block and a repo skill.
func Codex(s *kb.Store) (*Result, error) {
	res := &Result{}
	if err := agentsMD(s, res); err != nil {
		return nil, err
	}
	skill := filepath.Join(repoRoot(s), ".codex", "skills", "kb", "SKILL.md")
	did, err := kb.WriteFileIfChanged(skill, []byte(Skill(bundleRel(s))))
	if err != nil {
		return nil, err
	}
	res.changed(skill, did)
	res.Notes = append(res.Notes,
		"Attribute Codex writes: add to ~/.codex/config.toml\n    [shell_environment_policy]\n    set = { KB_ACTOR = \"codex\" }",
		"Optional MCP instead of the CLI, in ~/.codex/config.toml:\n    [mcp_servers.kb]\n    command = \"kb\"\n    args = [\"mcp\", \"--as\", \"codex\"]",
		"Capture: Codex has no session hooks, so schedule the importer, e.g. crontab:\n    17 2 * * * cd "+repoRoot(s)+" && kb extract --scan codex >> "+s.LocalPath("extract.log")+" 2>&1",
		"The skill was written to .codex/skills/kb/; check your Codex version's skills directory.")
	return res, nil
}

// Cursor wires Cursor: it reads AGENTS.md natively.
func Cursor(s *kb.Store) (*Result, error) {
	res := &Result{}
	if err := agentsMD(s, res); err != nil {
		return nil, err
	}
	res.Notes = append(res.Notes,
		"Cursor reads AGENTS.md natively; no .mdc rule is needed (it would duplicate the pointer).",
		"Set KB_ACTOR for Cursor's terminal (e.g. KB_ACTOR=cursor/<model>) so writes are attributed to the right model family.",
		"Optional MCP: add to .cursor/mcp.json\n    {\"mcpServers\": {\"kb\": {\"command\": \"kb\", \"args\": [\"mcp\", \"--as\", \"cursor\"]}}}")
	return res, nil
}

// Antigravity wires Antigravity: it reads AGENTS.md as an always-on rule.
func Antigravity(s *kb.Store) (*Result, error) {
	res := &Result{}
	if err := agentsMD(s, res); err != nil {
		return nil, err
	}
	res.Notes = append(res.Notes,
		"Antigravity reads AGENTS.md; set KB_ACTOR=antigravity/<model> in its terminal environment.",
		"Optional MCP: register 'kb mcp --as antigravity' in Antigravity's MCP settings.")
	return res, nil
}

// Hermes writes the Hermes skill and prints the sync/cron setup.
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
		"On the Hermes host: clone the repo, install kb, and export KB_ROOT="+s.Dir+" and KB_ACTOR=hermes/<model> in Hermes' environment.",
		"Keep it in sync with cron:\n    */15 * * * * cd "+repoRoot(s)+" && KB_ACTOR=hermes kb sync >> "+s.LocalPath("sync.log")+" 2>&1",
		"Add one line to ~/.hermes/memories/MEMORY.md: 'Use the kb skill (kb search / kb show / kb add) before answering project questions.'",
		"Optional MCP in Hermes config: mcp_servers: {kb: {command: kb, args: [mcp, --as, hermes]}}")
	return res, nil
}

// GitHooks installs pre-commit lint and post-merge reindex hooks.
func GitHooks(s *kb.Store) (*Result, error) {
	res := &Result{}
	if !s.InGit() {
		return nil, errors.New("not a git repository")
	}
	if runtime.GOOS == "windows" {
		return nil, errors.New("git hooks installer supports unix shells only")
	}
	hooksDir, err := kb.Git(s.CodeRoot, "rev-parse", "--git-path", "hooks")
	if err != nil {
		return nil, err
	}
	if !filepath.IsAbs(hooksDir) {
		hooksDir = filepath.Join(s.CodeRoot, hooksDir)
	}
	rel := bundleRel(s)
	hooks := map[string]string{
		"pre-commit": fmt.Sprintf("if command -v kb >/dev/null 2>&1 && ! git diff --cached --quiet -- %q; then\n  kb lint --staged --quiet || { echo 'kb lint failed (run: kb lint; kb index)'; exit 1; }\nfi", rel),
		"post-merge": "command -v kb >/dev/null 2>&1 && kb index --quiet || true",
	}
	for name, body := range hooks {
		p := filepath.Join(hooksDir, name)
		if _, err := os.Stat(p); os.IsNotExist(err) {
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
