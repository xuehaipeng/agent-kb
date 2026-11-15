package kb

import "strings"

// Rules and the health brief are pushed into an agent's context, and every
// push surface has a size it must stay within. A count cap alone does not
// bound size: forty long rules exceed what a hook may print. So each surface
// declares a budget for its complete payload (rule lines, ids, headings,
// omission notice and brief), and one planner fits the payload to it.
//
// Claude Code caps a hook's stdout at 10,000 characters; past that, Claude
// gets a file path and a 2,000-character preview and is not asked to read
// the file, so most of the rules would silently go missing.
const (
	// SessionStartBudget is the ceiling for everything the SessionStart hook
	// prints, kept below the documented 10,000-character hook limit.
	SessionStartBudget = 9000
	// MCPInstructionsBudget is our own declared budget for the server's
	// `instructions`. It is a context-cost choice, not a host limit: MCP
	// clients do not document the hook cap for this field.
	MCPInstructionsBudget = 9000

	// NativeRulesBudget is the declared budget of the managed rules file
	// Claude Code loads itself. The hook limit does not apply to a file;
	// this bounds its always-on context cost.
	NativeRulesBudget = 9000
	// SubagentBudget is the ceiling for what SubagentStart and the rule
	// refresh hand to an agent as additionalContext (the hook limit applies).
	SubagentBudget = 9000

	SurfaceSessionStart    = "session-start"
	SurfaceMCPInstructions = "mcp-instructions"
	SurfaceNativeRules     = "native-rules-file"
	SurfaceSubagentStart   = "subagent-start"
	SurfaceRuleRefresh     = "rule-refresh"

	// ContextUnit names what budgets and sizes are measured in.
	ContextUnit = "utf16-code-units"
)

// ContextLen measures text in UTF-16 code units, which is how a JavaScript
// host such as Claude Code measures string length. It is never smaller than
// the number of characters (code points), so a payload within a budget in
// these units is within the same budget in characters. Counting UTF-8 bytes
// would be safe too, but would cut CJK text to a third of the allowance.
func ContextLen(s string) int {
	n := 0
	for _, r := range s {
		if r >= 0x10000 {
			n += 2
		} else {
			n++
		}
	}
	return n
}

// Delivery is the plan for one push of rules (and brief) to one surface:
// what is rendered, what was left out and why, against which budget.
type Delivery struct {
	Surface        string   `json:"surface"`
	Scope          string   `json:"scope"`               // rule scope the plan was made for
	Unit           string   `json:"unit"`                // what Budget and Size count
	Budget         int      `json:"budget"`              // ceiling for the complete payload
	Size           int      `json:"size"`                // size of the rendered payload
	Applicable     int      `json:"applicable"`          // trusted rules in scope, before any cap
	Rendered       int      `json:"rendered"`            // rules in the payload
	OmittedByCount int      `json:"omitted_by_count"`    // left out by the rule-count cap
	OmittedBySize  int      `json:"omitted_by_size"`     // left out to fit the budget
	Native         int      `json:"native"`              // not in this payload: delivered by the managed rules file
	BriefLines     int      `json:"brief_lines"`         // health lines in the payload
	BriefOmitted   int      `json:"brief_lines_omitted"` // health lines left out to fit the budget
	Text           string   `json:"-"`                   // the payload itself
	IDs            []string `json:"-"`                   // the rules in the payload
}

// Omitted is the number of applicable rules that are not in the payload.
func (d Delivery) Omitted() int { return d.OmittedByCount + d.OmittedBySize }

const briefOmittedNotice = "kb: health notes were left out to fit the context budget (kb status)"

// planDelivery fits prefix + rules + brief into budget. It is pure: the same
// inputs give the same payload, so 'kb status' can report exactly what the
// hook delivers. Whole lines only: a rule line is never split, and neither
// is a character. When the payload is too big, brief lines go first (lowest
// priority first, replaced by a one-line notice), then rules from the end:
// global rules before project rules, as the count cap does. The omission
// notice is part of the payload and is counted.
//
// With native set, the universal global rules are left out: the managed
// rules file delivers them. They stay in Applicable and are counted as
// Native, not as omitted.
func (s *Store) planDelivery(surface string, budget int, prefix string, rs RuleSet, brief []string, native bool) Delivery {
	d := Delivery{Surface: surface, Scope: rs.Scope, Unit: ContextUnit, Budget: budget, Applicable: rs.Applicable(), OmittedByCount: rs.Omitted}
	if native {
		var kept []*Node
		for _, n := range rs.Global {
			if len(n.Meta().Projects) == 0 {
				d.Native++
				continue
			}
			kept = append(kept, n)
		}
		rs.Global = kept
	}
	nS, nG, nB := len(rs.Scoped), len(rs.Global), len(brief)
	build := func() string {
		t := rs
		t.Scoped, t.Global, t.Pending = rs.Scoped[:nS], rs.Global[:nG], nil
		t.Omitted = rs.Omitted + (len(rs.Scoped) - nS) + (len(rs.Global) - nG)
		var b strings.Builder
		b.WriteString(prefix)
		if rules := s.RenderRules(t, false); rules != "" {
			if prefix != "" {
				b.WriteString("\n\n")
			}
			b.WriteString(rules)
		}
		for _, l := range brief[:nB] {
			b.WriteString(l + "\n")
		}
		if nB < len(brief) {
			b.WriteString(briefOmittedNotice + "\n")
		}
		return b.String()
	}
	text := build()
	fits := func() bool { return budget <= 0 || ContextLen(text) <= budget }
	for !fits() && nB > 0 {
		nB--
		text = build()
	}
	for !fits() && nS+nG > 0 {
		if nG > 0 {
			nG--
		} else {
			nS--
		}
		text = build()
	}
	d.Text, d.Size = text, ContextLen(text)
	d.Rendered = nS + nG
	for _, n := range append(append([]*Node{}, rs.Scoped[:nS]...), rs.Global[:nG]...) {
		d.IDs = append(d.IDs, n.ID)
	}
	d.OmittedBySize = len(rs.Scoped) + len(rs.Global) - d.Rendered
	d.BriefLines, d.BriefOmitted = nB, len(brief)-nB
	return d
}

// SessionStartDelivery plans what the SessionStart hook prints for the
// session's own scope: the rules that apply, then the health brief.
func (s *Store) SessionStartDelivery(me Actor) (Delivery, error) {
	rs, err := s.Rules(nil, false)
	if err != nil {
		return Delivery{}, err
	}
	_, native := s.NativeRulesActive()
	return s.planDelivery(SurfaceSessionStart, SessionStartBudget, "", rs, s.sessionBriefLines(me), native), nil
}

// AgentDelivery plans a delivery of the complete applicable rule set to one
// agent as hook context: a subagent at its start, or any agent whose rules
// changed since it last received them. Nothing is left to the managed rules
// file here: a subagent may not load it (Explore, Plan and agents that omit
// CLAUDE.md do not), and an agent that loaded it has the old version.
func (s *Store) AgentDelivery(surface string) (Delivery, error) {
	rs, err := s.Rules(nil, false)
	if err != nil {
		return Delivery{}, err
	}
	return s.planDelivery(surface, SubagentBudget, "", rs, nil, false), nil
}

// MCPInstructionsDelivery plans the server's `instructions`: the fixed
// guidance (always kept) followed by the rules that apply.
func (s *Store) MCPInstructionsDelivery(base string) (Delivery, error) {
	rs, err := s.Rules(nil, false)
	if err != nil {
		return Delivery{Surface: SurfaceMCPInstructions, Unit: ContextUnit, Budget: MCPInstructionsBudget, Text: base, Size: ContextLen(base)}, err
	}
	return s.planDelivery(SurfaceMCPInstructions, MCPInstructionsBudget, base, rs, nil, false), nil
}
