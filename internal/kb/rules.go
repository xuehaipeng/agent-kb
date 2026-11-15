package kb

import (
	"fmt"
	"sort"
	"strings"
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
// rule count in 'kb status'.
func (s *Store) Rules(projects []string, all bool) (RuleSet, error) {
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
	if over := len(rs.Scoped) + len(rs.Global) - maxDeliveredRules; over > 0 {
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
	return rs, nil
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
		text := m.Title
		if d := strings.TrimSpace(m.Description); d != "" && !strings.EqualFold(strings.TrimRight(d, "."), strings.TrimRight(text, ".")) {
			text += " - " + d
		}
		only := ""
		if rs.All && n.Project == GlobalProject && len(m.Projects) > 0 {
			only = " (only for " + strings.Join(m.Projects, ", ") + ")"
		}
		fmt.Fprintf(&b, "- %s [%s]%s\n", oneLine(text, 220), n.ID, only)
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
		fmt.Fprintf(&b, "(+%d more rules: kb rules --all-rules)\n", rs.Omitted)
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
