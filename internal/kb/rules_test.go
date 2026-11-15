package kb

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestRulesDelivery(t *testing.T) {
	newHub(t)
	repo := newCodeRepo(t, "git@github.com:AtlasCloudTeam/aiproxy.git")
	s := openAt(t, repo)
	add := func(title, project, evidence string, projects ...string) *Node {
		t.Helper()
		r, err := s.Add(AddInput{Type: "rule", Title: title, Description: "because", Project: project, Projects: projects, Evidence: evidence, Actor: claude, Force: true})
		if err != nil {
			t.Fatal(err)
		}
		return r.Node
	}
	pnpm := add("Use pnpm, never npm", "", "user said so")
	if pnpm.TypeKey != "convention" {
		t.Fatalf("rule alias: %s", pnpm.TypeKey)
	}
	add("Write commit messages in English", "global", "user said so")
	add("Only for kubedl: use volcano queues", "global", "user said so", "kubedl")
	guess := add("Prefer table-driven tests", "", "")
	gone := add("Use yarn", "", "user said so")
	if _, err := s.Deprecate(gone.ID, "replaced by pnpm", claude); err != nil {
		t.Fatal(err)
	}
	rs, err := s.Rules(nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(rs.Scoped) != 1 || rs.Scoped[0].ID != pnpm.ID || len(rs.Global) != 1 || len(rs.Pending) != 1 || rs.Pending[0].ID != guess.ID {
		t.Fatalf("rule set: scoped=%v global=%v pending=%v", ids(rs.Scoped), ids(rs.Global), ids(rs.Pending))
	}
	out := s.RenderRules(rs, true)
	for _, want := range []string{"aiproxy:\n- Use pnpm, never npm - because [aiproxy/conventions/use-pnpm-never-npm]", "global:\n- Write commit messages in English", "Proposed rules awaiting confirmation", "Prefer table-driven tests"} {
		if !strings.Contains(out, want) {
			t.Errorf("render missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "volcano") || strings.Contains(out, "yarn") {
		t.Fatalf("out-of-scope or deprecated rule delivered:\n%s", out)
	}
	// User-level files are read in every repository: only rules that apply
	// everywhere go there, not project rules and not --for restricted ones.
	if g := s.GlobalRulesText(); !strings.Contains(g, "commit messages in English") || strings.Contains(g, "pnpm") || strings.Contains(g, "volcano") {
		t.Fatalf("global rules text:\n%s", g)
	}
	// Confirming the guess delivers it.
	if _, _, err := s.Verify(VerifyInput{Ref: guess.ID, Result: ResultConfirmed, Evidence: "user confirmed", Actor: human}); err != nil {
		t.Fatal(err)
	}
	rs, _ = s.Rules(nil, false)
	if len(rs.Scoped) != 2 || len(rs.Pending) != 0 {
		t.Fatalf("after confirm: %v pending %v", ids(rs.Scoped), ids(rs.Pending))
	}
	st, _ := s.Status(claude)
	if st.Rules != 3 {
		t.Fatalf("status rules: %d", st.Rules)
	}
}

func ids(ns []*Node) []string {
	var out []string
	for _, n := range ns {
		out = append(out, n.ID)
	}
	return out
}

// Rules are instructions, so their scope is narrower than discovery: every
// surface gets only what applies where the session runs.
func TestRuleScopeIsNotDiscoveryScope(t *testing.T) {
	newHub(t)
	aiproxy := newCodeRepo(t, "git@github.com:AtlasCloudTeam/aiproxy.git")
	kubedl := newCodeRepo(t, "git@github.com:AtlasCloudTeam/kubedl.git")
	a, k := openAt(t, aiproxy), openAt(t, kubedl)
	rule := func(s *Store, title, project string, projects ...string) {
		t.Helper()
		if _, err := s.Add(AddInput{Type: "rule", Title: title, Description: "because", Project: project, Projects: projects, Evidence: "user said so", Actor: claude, Force: true}); err != nil {
			t.Fatal(err)
		}
	}
	rule(a, "Use pnpm, never npm", "")
	rule(k, "Submit jobs through volcano queues", "")
	rule(a, "Write commit messages in English", "global")
	rule(a, "Tag images with the git sha", "global", "kubedl")
	if _, err := a.Add(AddInput{Type: "rule", Title: "Prefer table-driven tests", Description: "inferred", Actor: claude}); err != nil {
		t.Fatal(err)
	}
	titles := func(rs RuleSet) string {
		var out []string
		for _, n := range append(append([]*Node{}, rs.Scoped...), rs.Global...) {
			out = append(out, n.Meta().Title)
		}
		return strings.Join(out, " | ")
	}
	check := func(name string, s *Store, projects []string, all bool, scope string, want ...string) RuleSet {
		t.Helper()
		rs, err := s.Rules(projects, all)
		if err != nil {
			t.Fatal(err)
		}
		if got := titles(rs); got != strings.Join(want, " | ") || rs.Scope != scope {
			t.Fatalf("%s: scope %q rules %q", name, rs.Scope, got)
		}
		return rs
	}
	home := openAt(t, t.TempDir())
	// Outside any project: only what applies everywhere, and no other
	// project's proposals either.
	rs := check("no project", home, nil, false, "global only", "Write commit messages in English")
	if len(rs.Pending) != 0 {
		t.Fatalf("no project: pending %v", ids(rs.Pending))
	}
	if out := home.RenderRules(rs, true); strings.Contains(out, "pnpm") || strings.Contains(out, "volcano") || strings.Contains(out, "git sha") {
		t.Fatalf("unscoped delivery leaked project rules:\n%s", out)
	}
	check("-p global", home, []string{"global"}, false, "global only", "Write commit messages in English")
	check("in aiproxy", a, nil, false, "aiproxy + global", "Use pnpm, never npm", "Write commit messages in English")
	// A restricted global rule is delivered where it applies, from anywhere.
	check("in kubedl", openAt(t, kubedl), nil, false, "kubedl + global", "Submit jobs through volcano queues", "Tag images with the git sha", "Write commit messages in English")
	check("-p kubedl outside", home, []string{"kubedl"}, false, "kubedl + global", "Submit jobs through volcano queues", "Tag images with the git sha", "Write commit messages in English")
	rs = check("-p kubedl from aiproxy", a, []string{"kubedl"}, false, "kubedl + global", "Submit jobs through volcano queues", "Tag images with the git sha", "Write commit messages in English")
	if out := a.RenderRules(rs, false); !strings.Contains(out, "kubedl:\n- Submit jobs") || strings.Contains(out, "aiproxy:") {
		t.Fatalf("another project's rules must be labelled as its own:\n%s", out)
	}
	// All-project inspection stays available and says what it is.
	rs = check("--all", home, nil, true, "all projects", "Use pnpm, never npm", "Submit jobs through volcano queues", "Tag images with the git sha", "Write commit messages in English")
	if out := home.RenderRules(rs, true); !strings.Contains(out, "aiproxy:\n- Use pnpm") || !strings.Contains(out, "kubedl:\n- Submit jobs") || !strings.Contains(out, "(only for kubedl)") || !strings.Contains(out, "Prefer table-driven tests") {
		t.Fatalf("--all render:\n%s", out)
	}
	if g := home.GlobalRulesText(); !strings.Contains(g, "commit messages in English") || strings.Contains(g, "git sha") || strings.Contains(g, "pnpm") {
		t.Fatalf("user-level block:\n%s", g)
	}

	// Status: entries use the discovery scope (every project outside one),
	// rules use the applicable scope, and the report says which is which.
	st, _ := home.StatusFor(StatusOptions{})
	if st.Scope != "all projects" || st.Total != 5 || st.Rules != 1 || st.RulesScope != "global only" || len(st.PendingRules) != 0 {
		t.Fatalf("unscoped status: %+v", st)
	}
	if out := RenderStatus(st); !strings.Contains(out, "scope: all projects") || !strings.Contains(out, "rules that apply (global only): 1") {
		t.Fatalf("status text:\n%s", out)
	}
	if st, _ := home.StatusFor(StatusOptions{Projects: []string{"kubedl"}}); st.Rules != 3 || st.RulesScope != "kubedl + global" {
		t.Fatalf("-p kubedl status: %+v", st)
	}
	if st, _ := home.StatusFor(StatusOptions{AllProjects: true}); st.Rules != 4 || st.RulesScope != "all projects" || len(st.PendingRules) != 1 {
		t.Fatalf("--all status: %+v", st)
	}
	// The automatic brief outside a project is global-scoped: the proposed
	// rule of aiproxy is not this session's business.
	if b := home.SessionBrief(claude); b != "" {
		t.Fatalf("session brief outside a project: %q", b)
	}
	if b := openAt(t, aiproxy).SessionBrief(claude); !strings.Contains(b, "1 proposed rule(s)") {
		t.Fatalf("session brief in aiproxy: %q", b)
	}

	// The cap: the count keeps including what the rendering leaves out.
	for i := 0; i < maxDeliveredRules+2; i++ {
		rule(a, fmt.Sprintf("Global habit number %03d", i), "global")
	}
	home = openAt(t, t.TempDir())
	rs, _ = home.Rules(nil, false)
	st, _ = home.StatusFor(StatusOptions{})
	if len(rs.Global) != maxDeliveredRules || rs.Omitted != 3 || st.Rules != maxDeliveredRules+3 || st.RulesOmitted != 3 {
		t.Fatalf("cap: delivered %d omitted %d status %d/%d", len(rs.Global), rs.Omitted, st.Rules, st.RulesOmitted)
	}
}

// The automatic brief outside a project carries only what applies in every
// session. A global entry restricted to one project (--for) is that
// project's business, even though selecting the global bundle for discovery
// includes it.
func TestSessionBriefSkipsRestrictedGlobals(t *testing.T) {
	newHub(t)
	shop := newCodeRepo(t, "git@github.com:Acme/shop.git")
	s := openAt(t, shop)
	add := func(typ, title string, projects ...string) *Node {
		t.Helper()
		r, err := s.Add(AddInput{Type: typ, Title: title, Description: "because", Project: "global", Projects: projects, Evidence: "user said so", Actor: claude, Force: true})
		if err != nil {
			t.Fatal(err)
		}
		return r.Node
	}
	restrictedRule := add("rule", "Tag shop images with the git sha", "shop")
	restrictedFact := add("fact", "Shop checkout calls the tax service", "shop")
	universalFact := add("fact", "All services log in JSON")
	if _, _, err := s.Verify(VerifyInput{Ref: restrictedRule.ID, Result: ResultDisputed, Evidence: "tags are semver now", Actor: codex}); err != nil {
		t.Fatal(err)
	}
	later := func(st *Store) *Store {
		st.Now = func() time.Time { return time.Date(2027, 6, 1, 0, 0, 0, 0, time.UTC) } // facts are past stale_after
		return st
	}
	home := later(openAt(t, t.TempDir()))
	b := home.SessionBrief(claude)
	if strings.Contains(b, restrictedRule.ID) || strings.Contains(b, restrictedFact.ID) || strings.Contains(b, "disputed") || strings.Contains(b, "proposed rule") {
		t.Fatalf("brief outside a project carries a shop-only global entry:\n%s", b)
	}
	if !strings.Contains(b, "1 stale entr(ies) in global: "+universalFact.ID) {
		t.Fatalf("brief outside a project should still report universal global entries:\n%s", b)
	}
	// Where the restriction applies, they are reported.
	in := later(openAt(t, shop)).SessionBrief(claude)
	if !strings.Contains(in, "1 disputed entr(ies): "+restrictedRule.ID) || !strings.Contains(in, restrictedFact.ID) {
		t.Fatalf("brief in shop:\n%s", in)
	}
	// Interactive status keeps discovery semantics: selecting the global
	// bundle lists everything in it.
	st, _ := home.StatusFor(StatusOptions{Projects: []string{"global"}})
	if st.Scope != "global" || len(st.Disputed) != 1 || st.Stale != 2 || st.Total != 3 {
		t.Fatalf("kb status -p global: %+v", st)
	}
	if st, _ := home.StatusFor(StatusOptions{}); st.Scope != "all projects" || len(st.Disputed) != 1 {
		t.Fatalf("kb status: %+v", st)
	}
}
