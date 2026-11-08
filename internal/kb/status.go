package kb

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// StatusReport summarises bundle health; the numbers are the metrics the
// design calls for (staleness, disputes, review backlog, capture per actor).
type StatusReport struct {
	Project          string         `json:"project,omitempty"`
	Scope            string         `json:"scope"`
	Total            int            `json:"total"`
	ByState          map[string]int `json:"by_state"`
	ByType           map[string]int `json:"by_type"`
	Stale            int            `json:"stale"`
	UnverifiedOld    int            `json:"unverified_older_than_7d"`
	ClaimsByProducer map[string]int `json:"claims_by_producer"`
	Disputed         []string       `json:"disputed"`
	StaleIDs         []string       `json:"stale_ids"`
	ReviewsOpen      int            `json:"reviews_open"`
	ReviewsForMe     []string       `json:"reviews_for_me"`
	ReviewsChanges   []string       `json:"reviews_changes_requested_mine"`
	ParseErrors      int            `json:"parse_errors"`
	Uncommitted      int            `json:"uncommitted_files"`
	Unpushed         int            `json:"unpushed_commits"`
	Upstream         string         `json:"upstream,omitempty"`
}

// StatusOptions scopes the report.
type StatusOptions struct {
	Me          Actor
	Projects    []string
	AllProjects bool
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
	r := &StatusReport{ByState: map[string]int{}, ByType: map[string]int{}, ClaimsByProducer: map[string]int{}, ParseErrors: len(s.ParseErrors()), Project: s.Project}
	switch {
	case !s.Hub:
		r.Scope = "repository"
	case o.AllProjects || (len(o.Projects) == 0 && s.Project == ""):
		r.Scope = "all projects"
	case len(o.Projects) > 0:
		r.Scope = strings.Join(o.Projects, ", ") + " + global"
	default:
		r.Scope = s.Project + " + global"
	}
	r.Uncommitted, r.Unpushed, r.Upstream = s.SyncState()
	inScope := s.scopeFilter(o.Projects, o.AllProjects)
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
		if t.State == StateUnverified {
			if ts, err := parseTime(n.Meta().Generated.At); err == nil && ts.Before(week) {
				r.UnverifiedOld++
			}
		}
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
	return r, nil
}

// Brief renders at most three short lines for session-start context, or ""
// when nothing needs attention.
func (r *StatusReport) Brief() string {
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
	if r.Stale > 0 {
		lines = append(lines, fmt.Sprintf("kb: %d stale entr(ies) in %s: %s (refresh them when you touch that code)", r.Stale, r.Scope, list3(r.StaleIDs)))
	}
	return strings.Join(lines, "\n")
}

func list3(ids []string) string {
	sorted := append([]string(nil), ids...)
	sort.Strings(sorted)
	if len(sorted) > 3 {
		return strings.Join(sorted[:3], ", ") + fmt.Sprintf(" +%d", len(sorted)-3)
	}
	return strings.Join(sorted, ", ")
}
