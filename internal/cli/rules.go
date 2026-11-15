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
	var all, quiet, uncapped bool
	f.list(&projects, true, "project(s) (hub)", "p", "project")
	f.boolean(&all, "every project (hub)", "all")
	f.boolean(&quiet, "rules only, no pending list or hints", "quiet", "q")
	f.boolean(&uncapped, "list every rule that applies, including those a delivery cap leaves out and conditional ones", "all-rules")
	var files []string
	f.list(&files, true, "file you will work on: show the rules that apply only to it", "f", "file")
	var delivery bool
	f.boolean(&delivery, "how rules reach Claude Code on this machine, and what recent sessions received", "delivery")
	if _, err := f.parse(args); err != nil {
		return err
	}
	s, err := a.store()
	if err != nil {
		return err
	}
	if delivery {
		a.out("%s", s.RenderDeliveryDiagnostics())
		return nil
	}
	if len(files) > 0 {
		var rel []string
		for _, p := range files {
			rel = append(rel, s.RelToCode(p))
		}
		rules, err := s.RulesForFiles(rel, projects, all)
		if err != nil {
			return err
		}
		out := s.RenderFileRules(rules, rel)
		noteRulesDelivered(s, rules)
		if out == "" && !quiet {
			out = "no rule applies only to " + strings.Join(rel, ", ") + " (the always-on rules still do: kb rules)\n"
		}
		a.out("%s", out)
		return nil
	}
	get := s.Rules
	if uncapped {
		get = s.AllRules
	}
	rs, err := get(projects, all)
	if err != nil {
		return err
	}
	out := s.RenderRules(rs, !quiet)
	if !all {
		// An agent that runs 'kb rules' receives them, like one a hook
		// hands them to. Every project's rules at once is an inspection.
		noteRulesDelivered(s, append(append([]*kb.Node{}, rs.Scoped...), rs.Global...))
	}
	if out == "" && !quiet {
		out = "no rules yet. When the user states a lasting rule or preference: kb add rule \"<instruction>\" -d \"<why>\" -e \"user said so\" [--global]\n" +
			"Import rules written for one agent (CLAUDE.md, .cursor/rules, ~/.codex/AGENTS.md, ...): kb rules import\n"
	}
	a.out("%s", out)
	if !quiet && s.Hub {
		switch {
		case all:
			a.out("(every project's rules, for inspection; a session gets only the rules of its own project plus the global ones that apply to it)\n")
		case len(projects) == 0 && s.Project == "":
			a.out("(not inside a project: these are the global rules that apply everywhere; kb rules -p <project> adds a project's, --all lists every project's)\n")
		}
	}
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
	var notes []string
	if len(pos) > 0 {
		// Explicit files: written for an unknown agent, scoped like the repo files.
		for _, p := range pos {
			src, note := extract.PrepareExplicitRuleFile(s, p)
			if note != "" {
				notes = append(notes, note)
			}
			if src != nil {
				srcs = append(srcs, *src)
			}
		}
	} else {
		srcs, notes = extract.RuleFileSources(s, !projectOnly, !userOnly)
	}
	if len(srcs) > 0 || len(notes) == 0 {
		for _, line := range extract.ImportAll(s, srcs, o) {
			a.out("%s\n", line)
		}
	}
	// What was found but deliberately left out, and why.
	for _, n := range notes {
		a.out("note: %s\n", n)
	}
	return nil
}

// noteRulesDelivered counts rules handed to whoever ran the command in the
// local access statistics.
func noteRulesDelivered(s *kb.Store, rules []*kb.Node) {
	ids := make([]string, 0, len(rules))
	for _, n := range rules {
		ids = append(ids, n.ID)
	}
	s.NoteAccess(kb.AccessDelivered, ids)
}
