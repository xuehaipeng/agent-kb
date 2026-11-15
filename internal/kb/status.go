package kb

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// StatusReport summarises bundle health; the numbers are the metrics the
// design calls for (staleness, disputes, review backlog, capture per actor).
//
// Two scopes meet here. Entry numbers use the discovery scope (Scope: every
// project when run outside one). Rule numbers use the narrower scope of the
// rules that apply (RulesScope: only global rules outside a project), the
// same selection 'kb rules' and session start use.
type StatusReport struct {
	Project          string         `json:"project,omitempty"`
	Scope            string         `json:"scope"`
	Total            int            `json:"total"`
	ByState          map[string]int `json:"by_state"`
	ByType           map[string]int `json:"by_type"`
	Stale            int            `json:"stale"` // needs a look: code changed or past stale_after
	Overdue          int            `json:"overdue"`
	CodeFreshness    map[string]int `json:"code_freshness"`
	UnknownFreshness int            `json:"unknown_freshness"` // not counted in Stale
	UnknownIDs       []string       `json:"unknown_freshness_ids"`
	UnknownReasons   map[string]int `json:"unknown_freshness_reasons,omitempty"`
	UnverifiedOld    int            `json:"unverified_older_than_7d"`
	ClaimsByProducer map[string]int `json:"claims_by_producer"`
	Disputed         []string       `json:"disputed"`
	StaleIDs         []string       `json:"stale_ids"`
	ReviewsOpen      int            `json:"reviews_open"`
	ReviewsForMe     []string       `json:"reviews_for_me"`
	ReviewsChanges   []string       `json:"reviews_changes_requested_mine"`
	ParseErrors      int            `json:"parse_errors"`
	Rules            int            `json:"rules_delivered"` // applicable trusted rules, before the delivery cap
	RulesScope       string         `json:"rules_scope"`
	RulesOmitted     int            `json:"rules_omitted_by_cap"` // applicable rules the session-start delivery leaves out (count cap or size budget)
	RulesConditional int            `json:"rules_conditional"`    // trusted rules that apply only to matching files; never in rules_delivered
	Delivery         *Delivery      `json:"rules_delivery,omitempty"`
	PendingRules     []string       `json:"rules_pending"`
	Uncommitted      int            `json:"uncommitted_files"`
	Unpushed         int            `json:"unpushed_commits"`
	Upstream         string         `json:"upstream,omitempty"`
}

// StatusOptions scopes the report.
type StatusOptions struct {
	Me          Actor
	Projects    []string
	AllProjects bool

	// everywhere limits entries to those that apply in any session: global
	// entries with no `projects:` restriction. Discovery scope would count a
	// global entry restricted to one project when the global bundle is
	// selected; automatic session output must not.
	everywhere bool
	// noDelivery skips the delivery plan (set when a report is computed only
	// for its brief, which the plan itself needs).
	noDelivery bool
}

// Status computes the report for the current scope; me may be zero.
func (s *Store) Status(me Actor) (*StatusReport, error) {
	return s.StatusFor(StatusOptions{Me: me})
}

// StatusFor computes the report for an explicit scope.
func (s *Store) StatusFor(o StatusOptions) (*StatusReport, error) {
	me := o.Me
	nodes, err := s.Nodes()
	if err != nil {
		return nil, err
	}
	r := &StatusReport{ByState: map[string]int{}, ByType: map[string]int{}, ClaimsByProducer: map[string]int{}, CodeFreshness: map[string]int{}, ParseErrors: len(s.ParseErrors()), Project: s.Project}
	var named []string
	for _, p := range o.Projects {
		if p != GlobalProject {
			named = append(named, p)
		}
	}
	switch {
	case !s.Hub:
		r.Scope = "repository"
	case o.AllProjects || (len(o.Projects) == 0 && s.Project == ""):
		r.Scope = "all projects"
	case len(o.Projects) > 0 && len(named) == 0:
		r.Scope = "global"
	case len(o.Projects) > 0:
		r.Scope = strings.Join(named, ", ") + " + global"
	default:
		r.Scope = s.Project + " + global"
	}
	r.Uncommitted, r.Unpushed, r.Upstream = s.SyncState()
	var planRules *RuleSet
	inScope := s.scopeFilter(o.Projects, o.AllProjects)
	if o.everywhere && s.Hub {
		inScope = func(n *Node) bool { return n.Project == GlobalProject && len(n.Meta().Projects) == 0 }
		r.Scope = "global"
	}
	sup := s.supersededIndex()
	week := s.Now().Add(-7 * 24 * time.Hour)
	for _, n := range nodes {
		if n.TypeKey == "review" || !inScope(n) {
			continue
		}
		t := s.trust(n, sup, true)
		r.ByState[t.State]++
		if t.State == StateDeprecated {
			continue
		}
		r.Total++
		r.ByType[n.TypeKey]++
		r.ClaimsByProducer[ParseActor(n.Meta().Generated.By).Producer]++
		if t.State == StateDisputed {
			r.Disputed = append(r.Disputed, n.ID)
		}
		if t.Stale {
			r.Stale++
			r.StaleIDs = append(r.StaleIDs, n.ID)
		}
		if t.Overdue {
			r.Overdue++
		}
		r.CodeFreshness[t.CodeFreshness]++
		if t.CodeFreshness == FreshUnknown {
			r.UnknownFreshness++
			r.UnknownIDs = append(r.UnknownIDs, n.ID)
			if r.UnknownReasons == nil {
				r.UnknownReasons = map[string]int{}
			}
			r.UnknownReasons[t.FreshnessCode]++
		}
		if t.State == StateUnverified {
			if ts, err := parseTime(n.Meta().Generated.At); err == nil && ts.Before(week) {
				r.UnverifiedOld++
			}
		}
	}
	if rs, err := s.Rules(o.Projects, o.AllProjects); err == nil {
		r.Rules, r.RulesScope, r.RulesOmitted = rs.Applicable(), rs.Scope, rs.Omitted
		r.RulesConditional = len(rs.Conditional)
		for _, n := range rs.Pending {
			r.PendingRules = append(r.PendingRules, n.ID)
		}
		planRules = &rs
	}
	open, _ := s.Reviews(ReviewFilter{Projects: o.Projects, AllProjects: o.AllProjects})
	r.ReviewsOpen = len(open)
	if !me.Zero() {
		mine, _ := s.Reviews(ReviewFilter{ForMe: me, Projects: o.Projects, AllProjects: o.AllProjects})
		for _, it := range mine {
			r.ReviewsForMe = append(r.ReviewsForMe, it.Node.ID)
		}
		myFam := s.Cfg.Family(me)
		for _, it := range open {
			if it.State == ReviewChanges && s.Cfg.FamilyOf(it.Node.Meta().Generated.By) == myFam {
				r.ReviewsChanges = append(r.ReviewsChanges, it.Node.ID)
			}
		}
	}
	// What the SessionStart hook delivers for this rule scope, planned with
	// the same helper and budget the hook uses. In the default scope the
	// brief is the session's own, so the numbers are exactly the hook's.
	if planRules != nil && !o.noDelivery {
		brief := r.briefLines()
		if len(o.Projects) == 0 && !o.AllProjects && s.Hub && s.Project == "" {
			brief = s.sessionBriefLines(me)
		}
		var native planOpts
		if len(o.Projects) == 0 && !o.AllProjects {
			native.native = s.nativeRuleIDs()
		}
		d := s.planDelivery(SurfaceSessionStart, SessionStartBudget, "", *planRules, brief, native)
		d.Text = ""
		r.Delivery, r.RulesOmitted = &d, d.Omitted()
	}
	return r, nil
}

// briefLines is the budget for automatically injected session context.
const briefLines = 3

// Brief renders at most three short lines for session-start context, or ""
// when nothing needs attention. Lines come in priority order (reviews,
// disputes, proposed rules, entries needing a look, then one aggregate line
// for code that could not be compared); what does not fit is left to
// 'kb status'.
func (r *StatusReport) Brief() string { return strings.Join(r.briefLines(), "\n") }

func (r *StatusReport) briefLines() []string {
	var lines []string
	if n := len(r.ReviewsForMe); n > 0 {
		lines = append(lines, fmt.Sprintf("kb: %d review(s) waiting for you: %s (kb review show <id>)", n, list3(r.ReviewsForMe)))
	}
	if n := len(r.ReviewsChanges); n > 0 {
		lines = append(lines, fmt.Sprintf("kb: changes requested on your review(s): %s", list3(r.ReviewsChanges)))
	}
	if n := len(r.Disputed); n > 0 {
		lines = append(lines, fmt.Sprintf("kb: %d disputed entr(ies): %s (check the code; kb update/verify/deprecate)", n, list3(r.Disputed)))
	}
	if n := len(r.PendingRules); n > 0 {
		lines = append(lines, fmt.Sprintf("kb: %d proposed rule(s) not applied until confirmed: %s (ask the user; kb verify <id> --confirm)", n, list3(r.PendingRules)))
	}
	if r.Stale > 0 {
		lines = append(lines, fmt.Sprintf("kb: %d stale entr(ies) in %s: %s (refresh them when you touch that code)", r.Stale, r.Scope, list3(r.StaleIDs)))
	}
	if r.UnknownFreshness > 0 {
		lines = append(lines, fmt.Sprintf("kb: %d entr(ies) in %s whose code could not be compared on this machine (kb status)", r.UnknownFreshness, r.Scope))
	}
	if over := len(lines) - briefLines; over > 0 {
		lines = lines[:briefLines]
		lines[briefLines-1] += fmt.Sprintf(" (+%d more: kb status)", over)
	}
	return lines
}

// SessionBrief is the health brief injected into a session automatically
// (the SessionStart hook). It follows the session: the current project plus
// the global entries that apply to it, and outside any project only global
// entries that apply everywhere, so a session never receives the health of
// unrelated projects, including through a global entry restricted to one of
// them. Interactive 'kb status' keeps its discovery defaults.
func (s *Store) SessionBrief(me Actor) string {
	return strings.Join(s.sessionBriefLines(me), "\n")
}

func (s *Store) sessionBriefLines(me Actor) []string {
	o := StatusOptions{Me: me, noDelivery: true}
	if s.Hub && s.Project == "" {
		o.Projects = []string{GlobalProject}
		o.everywhere = true
	}
	r, err := s.StatusFor(o)
	if err != nil {
		return nil
	}
	return r.briefLines()
}

func list3(ids []string) string {
	sorted := append([]string(nil), ids...)
	sort.Strings(sorted)
	if len(sorted) > 3 {
		return strings.Join(sorted[:3], ", ") + fmt.Sprintf(" +%d", len(sorted)-3)
	}
	return strings.Join(sorted, ", ")
}
