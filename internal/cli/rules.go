package cli

import (
	"errors"
	"strings"

	"github.com/xuehaipeng/agent-kb/internal/extract"
	"github.com/xuehaipeng/agent-kb/internal/install"
	"github.com/xuehaipeng/agent-kb/internal/kb"
)

func init() {
	// Keep the rule blocks in agents' instruction files current whenever a
	// rule changes (user-level blocks only for the hub that wrote them).
	kb.ChangeHook = func(s *kb.Store) { _, _ = install.RefreshPointer(s) }
}

func (a *App) cmdRules(args []string) error {
	if len(args) > 0 && args[0] == "import" {
		return a.cmdRulesImport(args[1:])
	}
	f := newFlags("rules")
	a.globals(f)
	var projects []string
	var all, quiet bool
	f.list(&projects, true, "project(s) (hub)", "p", "project")
	f.boolean(&all, "every project (hub)", "all")
	f.boolean(&quiet, "rules only, no pending list or hints", "quiet", "q")
	if _, err := f.parse(args); err != nil {
		return err
	}
	s, err := a.store()
	if err != nil {
		return err
	}
	rs, err := s.Rules(projects, all)
	if err != nil {
		return err
	}
	out := s.RenderRules(rs, !quiet)
	if out == "" && !quiet {
		out = "no rules yet. When the user states a lasting rule or preference: kb add rule \"<instruction>\" -d \"<why>\" -e \"user said so\" [--global]\n" +
			"Import rules written for one agent (CLAUDE.md, .cursor/rules, ~/.codex/AGENTS.md, ...): kb rules import\n"
	}
	a.out("%s", out)
	return nil
}

func (a *App) cmdRulesImport(args []string) error {
	f := newFlags("rules import")
	a.globals(f)
	var userOnly, projectOnly bool
	var o extract.ImportOptions
	var cmdStr string
	f.boolean(&userOnly, "only user-level files (~/.claude/CLAUDE.md, ~/.codex/AGENTS.md, ~/.gemini/GEMINI.md)", "user")
	f.boolean(&projectOnly, "only this repo's agent-specific rule files", "repo")
	f.boolean(&o.Force, "re-import unchanged files", "force")
	f.boolean(&o.DryRun, "print prompts instead of calling the model", "dry-run")
	f.str(&cmdStr, "", "override extractor command (space separated)", "cmd")
	pos, err := f.parse(args)
	if err != nil {
		return err
	}
	if userOnly && projectOnly {
		return errors.New("--user and --repo are exclusive")
	}
	s, err := a.store()
	if err != nil {
		return err
	}
	if cmdStr != "" {
		o.Command = strings.Fields(cmdStr)
	}
	o.Out = a.Stderr
	var srcs []extract.FileSource
	if len(pos) > 0 {
		// Explicit files: written for an unknown agent, scoped like the repo files.
		for _, p := range pos {
			srcs = append(srcs, extract.FileSource{Path: p, Kind: extract.KindRules, Agent: kb.Actor{Producer: "user"}})
		}
	} else {
		srcs = extract.RuleFileSources(s, !projectOnly, !userOnly)
	}
	for _, line := range extract.ImportAll(s, srcs, o) {
		a.out("%s\n", line)
	}
	return nil
}
