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
	Scoped  []*Node // the project's rules (repo mode: all rules)
	Global  []*Node // global rules that apply here
	Pending []*Node // unverified or disputed rules in scope, not delivered
	Omitted int     // trusted rules left out by the size cap
}

// Empty reports whether there is nothing to deliver or confirm.
func (r RuleSet) Empty() bool { return len(r.Scoped)+len(r.Global)+len(r.Pending) == 0 }

// ChangeHook, when set, runs after a rule (always-on entry) changes, so
// installers can refresh the rule blocks in agents' instruction files.
var ChangeHook func(s *Store)

// Rules collects the rules for the current scope (or the given projects).
func (s *Store) Rules(projects []string, all bool) (RuleSet, error) {
	nodes, err := s.Nodes()
	if err != nil {
		return RuleSet{}, err
	}
	rs := RuleSet{Project: s.Project}
	inScope := s.scopeFilter(projects, all)
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
		fmt.Fprintf(&b, "- %s [%s]\n", oneLine(text, 220), n.ID)
	}
	if len(rs.Scoped) > 0 {
		if s.Hub && rs.Project != "" {
			fmt.Fprintf(&b, "%s:\n", rs.Project)
		}
		for _, n := range rs.Scoped {
			line(n)
		}
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
// Repo mode returns the bundle's rules.
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
