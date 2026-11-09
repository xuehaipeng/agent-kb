package kb

import (
	"strings"
	"testing"
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
	if g := s.GlobalRulesText(); !strings.Contains(g, "commit messages in English") || strings.Contains(g, "pnpm") {
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
