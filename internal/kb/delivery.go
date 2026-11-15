package kb

import (
	"bytes"
	"encoding/json"
	"strings"
)

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
	NativeIDs      []string `json:"-"`                   // the rules left to the managed rules file
}

// RuleRefreshNotice opens a rule refresh: it tells the agent why it is
// handed rules again.
const RuleRefreshNotice = "kb: the rules that apply here changed since you last received them. They are now:\n"

// HookContext is what a hook prints to hand text to an agent as
// additionalContext, for events that take context only as JSON
// (SubagentStart, PreToolUse). It sets no decision. HTML characters are left
// as they are: escaping them only makes the output longer.
func HookContext(event, text string) string {
	type specific struct {
		Event   string `json:"hookEventName"`
		Context string `json:"additionalContext"`
	}
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if enc.Encode(struct {
		Out specific `json:"hookSpecificOutput"`
	}{specific{event, text}}) != nil {
		return ""
	}
	return b.String()
}

// planOpts adjusts a plan for how its surface delivers.
type planOpts struct {
	// native: rules the managed rules file already holds, by id. They are
	// left out of the payload and counted as Native, not as omitted.
	native map[string]bool
	// measure gives the size of the complete output for a payload, when
	// that is more than the payload itself (a JSON envelope around it).
	measure func(text string) int
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
// Rules the managed rules file holds (o.native) are left out: that file
// delivers them. Only rules actually in the file count: one cut from it to
// fit its own budget is still this payload's to deliver. They stay in
// Applicable and are counted as Native, not as omitted.
//
// The budget is for the complete output. Where the payload travels inside
// an envelope (o.measure), the envelope and its escaping count too.
func (s *Store) planDelivery(surface string, budget int, prefix string, rs RuleSet, brief []string, o planOpts) Delivery {
	d, _ := s.planDeliveryRules(surface, budget, prefix, rs, brief, o)
	return d
}

// planDeliveryRules is planDelivery that also names the rules in the payload.
func (s *Store) planDeliveryRules(surface string, budget int, prefix string, rs RuleSet, brief []string, o planOpts) (Delivery, []string) {
	d := Delivery{Surface: surface, Scope: rs.Scope, Unit: ContextUnit, Budget: budget, Applicable: rs.Applicable(), OmittedByCount: rs.Omitted}
	if len(o.native) > 0 {
		var kept []*Node
		for _, n := range rs.Global {
			if o.native[n.ID] {
				d.Native++
				d.NativeIDs = append(d.NativeIDs, n.ID)
				continue
			}
			kept = append(kept, n)
		}
		rs.Global = kept
	}
	size := ContextLen
	if o.measure != nil {
		size = o.measure
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
	fits := func() bool { return budget <= 0 || size(text) <= budget }
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
	d.Text, d.Size = text, size(text)
	d.Rendered = nS + nG
	var ids []string
	for _, n := range append(append([]*Node{}, rs.Scoped[:nS]...), rs.Global[:nG]...) {
		ids = append(ids, n.ID)
	}
	d.OmittedBySize = len(rs.Scoped) + len(rs.Global) - d.Rendered
	d.BriefLines, d.BriefOmitted = nB, len(brief)-nB
	d.IDs = ids
	return d, ids
}

// SessionStartDelivery plans what the SessionStart hook prints for the
// session's own scope: the rules that apply, then the health brief.
func (s *Store) SessionStartDelivery(me Actor) (Delivery, error) {
	rs, err := s.Rules(nil, false)
	if err != nil {
		return Delivery{}, err
	}
	return s.planDelivery(SurfaceSessionStart, SessionStartBudget, "", rs, s.sessionBriefLines(me), planOpts{native: s.nativeRuleIDs()}), nil
}

// nativeRuleIDs are the rules this session gets from the managed rules
// file: those the file holds, when the session can count on it.
func (s *Store) nativeRuleIDs() map[string]bool {
	if _, active := s.NativeRulesActive(); !active {
		return nil
	}
	rs, err := s.nativeRuleSet()
	if err != nil {
		return nil
	}
	_, rendered := s.planDeliveryRules(SurfaceNativeRules, NativeRulesBudget, "", rs, nil, planOpts{})
	ids := map[string]bool{}
	for _, id := range rendered {
		ids[id] = true
	}
	return ids
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
	// The hook limit applies to what the hook prints. Where that is JSON,
	// the envelope and the escaping of the text count, and so does the
	// notice a refresh opens with.
	var o planOpts
	switch surface {
	case SurfaceSubagentStart:
		o.measure = func(text string) int { return ContextLen(HookContext("SubagentStart", text)) }
	case SurfaceRuleRefresh:
		o.measure = func(text string) int { return ContextLen(HookContext("PreToolUse", RuleRefreshNotice+text)) }
	}
	return s.planDelivery(surface, SubagentBudget, "", rs, nil, o), nil
}

// MCPInstructionsDelivery plans the server's `instructions`: the fixed
// guidance (always kept) followed by the rules that apply.
func (s *Store) MCPInstructionsDelivery(base string) (Delivery, error) {
	rs, err := s.Rules(nil, false)
	if err != nil {
		return Delivery{Surface: SurfaceMCPInstructions, Unit: ContextUnit, Budget: MCPInstructionsBudget, Text: base, Size: ContextLen(base)}, err
	}
	return s.planDelivery(SurfaceMCPInstructions, MCPInstructionsBudget, base, rs, nil, planOpts{}), nil
}
