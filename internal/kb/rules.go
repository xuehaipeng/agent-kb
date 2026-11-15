package kb

import (
	"fmt"
	"sort"
	"strings"

	"github.com/xuehaipeng/agent-kb/internal/okf"
)

// Rules are always-on entries (conventions by default): unlike facts and
// learnings, which agents pull with kb search, rules are pushed into every
// session, because a rule that must be searched for is not followed.
// Only evidence-backed rules are delivered; unverified ones wait as pending
// so one agent's guess does not steer every other agent.

const maxDeliveredRules = 40

// RuleSet is what applies in a scope.
type RuleSet struct {
	Project string
	Scope   string  // human description of the scope the rules were selected for
	All     bool    // all-project inspection rather than what applies here
	Scoped  []*Node // project rules (repo mode: all rules), grouped by project
	Global  []*Node // global rules that apply here
	Pending []*Node // unverified or disputed rules in scope, not delivered
	Omitted int     // trusted rules left out by the size cap
	// Conditional rules apply only when matching files are worked on. They
	// are trusted and in scope, but never part of always-on delivery.
	Conditional     []*Node
	ShowConditional bool // list them (inspection) instead of counting them
}

// Applicable counts the trusted rules in scope, including those the size cap
// left out of the rendered list.
func (r RuleSet) Applicable() int { return len(r.Scoped) + len(r.Global) + r.Omitted }

// Empty reports whether there is nothing to deliver or confirm.
func (r RuleSet) Empty() bool { return len(r.Scoped)+len(r.Global)+len(r.Pending) == 0 }

// ChangeHook, when set, runs after a rule (always-on entry) changes, so
// installers can refresh the rule blocks in agents' instruction files.
var ChangeHook func(s *Store)

// ruleScope selects the rules that apply, which is narrower than the
// discovery scope of search/status/review: a rule is an instruction, so one
// project's rule must never reach a session working elsewhere.
//
//   - repo mode: every rule of the bundle;
//   - all: every rule of every project (inspection, not delivery);
//   - projects given: those projects' rules plus the global rules that apply
//     to them ("global" itself selects the rules that apply everywhere);
//   - otherwise the current project, or outside any project only the global
//     rules without a `projects:` restriction.
func (s *Store) ruleScope(projects []string, all bool) (func(*Node) bool, string) {
	if !s.Hub {
		return func(*Node) bool { return true }, "repository"
	}
	if all {
		return func(*Node) bool { return true }, "all projects"
	}
	want := map[string]bool{}
	var named []string
	for _, p := range projects {
		if !want[p] {
			want[p] = true
			if p != GlobalProject {
				named = append(named, p)
			}
		}
	}
	if len(want) == 0 && s.Project != "" {
		want[s.Project] = true
		named = []string{s.Project}
	}
	scope := "global only"
	if len(named) > 0 {
		scope = strings.Join(named, ", ") + " + global"
	}
	return func(n *Node) bool {
		if n.Project != GlobalProject {
			return want[n.Project]
		}
		ps := n.Meta().Projects
		if len(ps) == 0 {
			return true
		}
		for _, p := range ps {
			if want[p] {
				return true
			}
		}
		return false
	}, scope
}

// Rules collects the rules that apply in the current scope (or to the given
// projects). Every delivery surface goes through here: 'kb rules', the
// SessionStart hook, MCP instructions, the user-level managed blocks and the
// rule count in 'kb status'. The set is capped at maxDeliveredRules; push
// surfaces also fit it to their size budget (see planDelivery).
func (s *Store) Rules(projects []string, all bool) (RuleSet, error) {
	return s.rules(projects, all, maxDeliveredRules)
}

// AllRules is Rules without the count cap, for listing what a cap left out.
func (s *Store) AllRules(projects []string, all bool) (RuleSet, error) {
	return s.rules(projects, all, 0)
}

func (s *Store) rules(projects []string, all bool, limit int) (RuleSet, error) {
	nodes, err := s.Nodes()
	if err != nil {
		return RuleSet{}, err
	}
	inScope, scope := s.ruleScope(projects, all)
	rs := RuleSet{Project: s.Project, Scope: scope, All: s.Hub && all}
	sup := s.supersededIndex()
	type ranked struct {
		n *Node
		w float64
	}
	var scoped, global []ranked
	for _, n := range nodes {
		if !s.Cfg.AlwaysOn(n.TypeKey) || !inScope(n) {
			continue
		}
		t := s.trust(n, sup, false)
		switch {
		case t.State == StateDeprecated:
			continue
		case !t.Trusted():
			rs.Pending = append(rs.Pending, n)
			continue
		}
		if Conditional(n.Meta()) {
			rs.Conditional = append(rs.Conditional, n)
			continue
		}
		r := ranked{n, trustWeight(t)}
		if n.Project == GlobalProject {
			global = append(global, r)
		} else {
			scoped = append(scoped, r)
		}
	}
	order := func(xs []ranked) []*Node {
		sort.SliceStable(xs, func(i, j int) bool {
			// Keep each project's rules together, the current project first.
			if pi, pj := xs[i].n.Project, xs[j].n.Project; pi != pj {
				if pi == s.Project || pj == s.Project {
					return pi == s.Project
				}
				return pi < pj
			}
			if xs[i].w != xs[j].w {
				return xs[i].w > xs[j].w
			}
			return strings.ToLower(xs[i].n.Meta().Title) < strings.ToLower(xs[j].n.Meta().Title)
		})
		out := make([]*Node, len(xs))
		for i, x := range xs {
			out[i] = x.n
		}
		return out
	}
	rs.Scoped, rs.Global = order(scoped), order(global)
	// Project rules first; cap the total so always-on context stays small.
	if over := len(rs.Scoped) + len(rs.Global) - limit; limit > 0 && over > 0 {
		rs.Omitted = over
		if len(rs.Global) >= over {
			rs.Global = rs.Global[:len(rs.Global)-over]
		} else {
			rest := over - len(rs.Global)
			rs.Global = nil
			rs.Scoped = rs.Scoped[:len(rs.Scoped)-rest]
		}
	}
	sort.Slice(rs.Pending, func(i, j int) bool { return rs.Pending[i].ID < rs.Pending[j].ID })
	sort.Slice(rs.Conditional, func(i, j int) bool { return rs.Conditional[i].ID < rs.Conditional[j].ID })
	rs.ShowConditional = limit == 0
	return rs, nil
}

// RulesForFiles returns the conditional rules in scope that apply to any of
// the given files (paths relative to the repository root).
func (s *Store) RulesForFiles(files []string, projects []string, all bool) ([]*Node, error) {
	rs, err := s.rules(projects, all, 0)
	if err != nil {
		return nil, err
	}
	var out []*Node
	for _, n := range rs.Conditional {
		for _, f := range files {
			if AppliesToFile(n.Meta().AppliesTo, f) {
				out = append(out, n)
				break
			}
		}
	}
	return out, nil
}

// RenderFileRules prints the conditional rules that apply to files. With
// several files, a rule may govern only some of them: each line then names
// the files it is for, so asking about two files at once never stretches a
// rule over both.
func (s *Store) RenderFileRules(rules []*Node, files []string) string {
	if len(rules) == 0 {
		return ""
	}
	var b strings.Builder
	if len(files) > 1 {
		b.WriteString("Rules from kb for these files; each applies only to the files named after it (follow them for work on those files unless the user says otherwise):\n")
	} else {
		fmt.Fprintf(&b, "Rules from kb that apply to %s (follow them for this work unless the user says otherwise):\n", strings.Join(files, ", "))
	}
	for _, n := range rules {
		fmt.Fprintf(&b, "- %s [%s]", ruleText(n), n.ID)
		if len(files) > 1 {
			fmt.Fprintf(&b, " (only for %s)", strings.Join(FilesGoverned(n, files), ", "))
		}
		b.WriteString("\n")
	}
	return b.String()
}

// FilesGoverned returns the files, of those given, a conditional rule
// applies to.
func FilesGoverned(n *Node, files []string) []string {
	var out []string
	for _, f := range files {
		if AppliesToFile(n.Meta().AppliesTo, f) {
			out = append(out, f)
		}
	}
	return out
}

func ruleText(n *Node) string {
	m := n.Meta()
	text := m.Title
	if d := strings.TrimSpace(m.Description); d != "" && !strings.EqualFold(strings.TrimRight(d, "."), strings.TrimRight(text, ".")) {
		text += " - " + d
	}
	return oneLine(text, 220)
}

func appliesNote(a *okf.Applicability) string {
	note := "when working on " + strings.Join(a.Paths, ", ")
	if a.Base != "" {
		note += " under " + a.Base + "/"
	}
	return note
}

// RenderRules prints rules compactly for an agent's context. Each line is
// the rule (title, plus description when it adds something) and its id.
func (s *Store) RenderRules(rs RuleSet, withPending bool) string {
	var b strings.Builder
	if len(rs.Scoped)+len(rs.Global) > 0 {
		b.WriteString("Rules from kb (shared by the user and other agents; follow them unless the user says otherwise):\n")
	}
	line := func(n *Node) {
		m := n.Meta()
		text := ruleText(n)
		only := ""
		if rs.All && n.Project == GlobalProject && len(m.Projects) > 0 {
			only = " (only for " + strings.Join(m.Projects, ", ") + ")"
		}
		fmt.Fprintf(&b, "- %s [%s]%s\n", text, n.ID, only)
	}
	prev := ""
	for _, n := range rs.Scoped {
		if s.Hub && n.Project != prev {
			fmt.Fprintf(&b, "%s:\n", n.Project)
			prev = n.Project
		}
		line(n)
	}
	if len(rs.Global) > 0 {
		if s.Hub {
			b.WriteString("global:\n")
		}
		for _, n := range rs.Global {
			line(n)
		}
	}
	if rs.Omitted > 0 {
		fmt.Fprintf(&b, "(+%d more rule(s) apply here but were left out to keep this short: kb rules --all-rules)\n", rs.Omitted)
	}
	// Conditional rules are named, not delivered: they apply only to
	// matching files and would otherwise act as always-on rules.
	if n := len(rs.Conditional); n > 0 {
		if rs.ShowConditional {
			b.WriteString("Conditional rules (not delivered at session start; they apply only to matching files):\n")
			for _, c := range rs.Conditional {
				fmt.Fprintf(&b, "- %s [%s] (%s)\n", ruleText(c), c.ID, appliesNote(c.Meta().AppliesTo))
			}
		} else {
			fmt.Fprintf(&b, "(+%d rule(s) apply only to particular files: kb rules -f <path> before working on a file)\n", n)
		}
	}
	if withPending && len(rs.Pending) > 0 {
		b.WriteString("Proposed rules awaiting confirmation (not applied; confirm with 'kb verify <id> --confirm -e \"<why>\"' or retire with 'kb deprecate'):\n")
		for _, n := range rs.Pending {
			fmt.Fprintf(&b, "- %s [%s]\n", oneLine(n.Meta().Title, 160), n.ID)
		}
	}
	return b.String()
}

// GlobalRulesText renders the rules that apply everywhere (for user-level
// instruction files that cannot run commands, such as ~/.codex/AGENTS.md).
// Those files are read in every repository, so project rules and global rules
// restricted to some projects (--for) stay out. Repo mode returns the
// bundle's rules.
func (s *Store) GlobalRulesText() string {
	var rs RuleSet
	var err error
	if s.Hub {
		rs, err = s.Rules([]string{GlobalProject}, false)
		rs.Scoped, rs.Project = nil, ""
	} else {
		rs, err = s.Rules(nil, false)
	}
	if err != nil {
		return ""
	}
	rs.Pending = nil
	return s.RenderRules(rs, false)
}
