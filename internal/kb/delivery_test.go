package kb

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestContextLen(t *testing.T) {
	for in, want := range map[string]int{"": 0, "abc": 3, "中文规则": 4, "😀": 2, "a中😀\n": 5} {
		if got := ContextLen(in); got != want {
			t.Errorf("ContextLen(%q) = %d, want %d", in, got, want)
		}
	}
}

func addRule(t *testing.T, s *Store, title, project, evidence string) *Node {
	t.Helper()
	r, err := s.Add(AddInput{Type: "rule", Title: title, Description: "because the team agreed on it", Project: project, Evidence: evidence, Actor: claude, Force: true})
	if err != nil {
		t.Fatal(err)
	}
	return r.Node
}

// ruleLines returns the rendered rule lines of a payload.
func ruleLines(text string) []string {
	var out []string
	for _, l := range strings.Split(text, "\n") {
		if strings.HasPrefix(l, "- ") {
			out = append(out, l)
		}
	}
	return out
}

// Forty-five maximum-length rules (ASCII, Chinese and non-BMP text, long
// ids) must fit the session-start budget as a whole payload, with whole
// lines only, project rules first, and every omission counted.
func TestSessionStartBudget(t *testing.T) {
	newHub(t)
	repo := newCodeRepo(t, "git@github.com:AtlasCloudTeam/aiproxy.git")
	s := openAt(t, repo)
	const projectRules, chinese, emoji = 5, 25, 15
	for i := 0; i < projectRules; i++ {
		addRule(t, s, fmt.Sprintf("Project rule %02d: %s", i, strings.Repeat("always prefer the documented approach over an improvised one ", 5)), "", "user said so")
	}
	for i := 0; i < chinese; i++ {
		addRule(t, s, fmt.Sprintf("规则%02d：%s", i, strings.Repeat("始终优先使用文档中记录的方法而不是临时方案，", 12)), "global", "user said so")
	}
	for i := 0; i < emoji; i++ {
		addRule(t, s, fmt.Sprintf("Rule %02d %s", i, strings.Repeat("ship it 🚀 keep it green ✅ no secrets 🔐 ", 8)), "global", "user said so")
	}
	addRule(t, s, "Prefer table-driven tests", "", "") // proposed: one brief line
	const applicable = projectRules + chinese + emoji

	d, err := s.SessionStartDelivery(claude)
	if err != nil {
		t.Fatal(err)
	}
	if d.Surface != SurfaceSessionStart || d.Unit != ContextUnit || d.Budget != SessionStartBudget || d.Scope != "aiproxy + global" {
		t.Fatalf("plan header: %+v", d)
	}
	if d.Size != ContextLen(d.Text) || d.Size > SessionStartBudget {
		t.Fatalf("payload is %d units (reported %d), budget %d", ContextLen(d.Text), d.Size, SessionStartBudget)
	}
	if !utf8.ValidString(d.Text) {
		t.Fatal("payload is not valid UTF-8: a character was split")
	}
	// Every applicable rule is accounted for.
	if d.Applicable != applicable || d.OmittedByCount != applicable-maxDeliveredRules || d.OmittedBySize == 0 || d.Rendered+d.Omitted() != applicable {
		t.Fatalf("accounting: %+v", d)
	}
	lines := ruleLines(d.Text)
	if len(lines) != d.Rendered {
		t.Fatalf("%d rule lines rendered, plan says %d", len(lines), d.Rendered)
	}
	for _, l := range lines {
		if !strings.HasSuffix(l, "]") || !strings.Contains(l, " [") {
			t.Fatalf("a rule line was split: %q", l)
		}
	}
	// Project rules come first and all survive; global rules give way.
	if !strings.HasPrefix(d.Text, "Rules from kb") || strings.Count(d.Text, "- Project rule ") != projectRules || strings.Index(d.Text, "aiproxy:\n") > strings.Index(d.Text, "global:\n") {
		t.Fatalf("project rules must be kept and come first:\n%s", d.Text[:400])
	}
	// The omission notice is in the payload and names the real total.
	if !strings.Contains(d.Text, fmt.Sprintf("(+%d more rule(s) apply here but were left out", d.Omitted())) || !strings.Contains(d.Text, "kb rules --all-rules") {
		t.Fatalf("omission notice missing or miscounted (omitted %d):\n%s", d.Omitted(), d.Text[len(d.Text)-300:])
	}
	// The brief gives way before any further rule does.
	if d.BriefLines != 0 || d.BriefOmitted != 1 || !strings.Contains(d.Text, briefOmittedNotice) || strings.Contains(d.Text, "proposed rule(s)") {
		t.Fatalf("brief should be replaced by its notice before rules are dropped: %+v", d)
	}
	// Chinese text is three bytes a character: a byte ceiling of the same
	// size would have cut far more than the documented limit requires.
	if len(d.Text) <= SessionStartBudget {
		t.Fatalf("fixture should exceed the budget in bytes (%d) while fitting it in characters", len(d.Text))
	}

	// Pure: the same inputs give the same payload and the same diagnostics.
	again, _ := openAt(t, repo).SessionStartDelivery(claude)
	if !reflect.DeepEqual(again, d) {
		t.Fatalf("delivery is not deterministic:\n%+v\n%+v", d, again)
	}
	st, err := openAt(t, repo).StatusFor(StatusOptions{Me: claude})
	if err != nil {
		t.Fatal(err)
	}
	want := d
	want.Text, want.IDs = "", nil
	if st.Delivery == nil || !reflect.DeepEqual(*st.Delivery, want) {
		t.Fatalf("status diagnostics differ from the hook's plan:\n%+v\n%+v", st.Delivery, want)
	}
	// rules_delivered keeps counting applicable rules before any cap.
	if st.Rules != applicable || st.RulesOmitted != d.Omitted() || st.RulesScope != "aiproxy + global" {
		t.Fatalf("status counts: rules=%d omitted=%d scope=%q", st.Rules, st.RulesOmitted, st.RulesScope)
	}
	if out := RenderStatus(st); !strings.Contains(out, fmt.Sprintf("rules that apply (aiproxy + global): %d; session start delivers %d and leaves out %d", applicable, d.Rendered, d.Omitted())) || !strings.Contains(out, "1 health line(s) left out") {
		t.Fatalf("status text:\n%s", out)
	}
	// What a cap left out can still be listed.
	all, _ := s.AllRules(nil, false)
	if len(all.Scoped)+len(all.Global) != applicable || all.Omitted != 0 {
		t.Fatalf("AllRules: %d + %d, omitted %d", len(all.Scoped), len(all.Global), all.Omitted)
	}

	// Outside a project the plan is for the global scope, and status in its
	// default scope reports that same plan although its entry counts cover
	// every project (whose proposed rule is not this session's business).
	home := openAt(t, t.TempDir())
	hd, _ := home.SessionStartDelivery(claude)
	if hd.Scope != "global only" || hd.Size > SessionStartBudget || hd.Applicable != chinese+emoji || strings.Contains(hd.Text, "Project rule") || hd.BriefLines+hd.BriefOmitted != 0 {
		t.Fatalf("no-project plan: %+v", hd)
	}
	hst, _ := home.StatusFor(StatusOptions{Me: claude})
	hwant := hd
	hwant.Text, hwant.IDs = "", nil
	if hst.Scope != "all projects" || len(hst.PendingRules) != 0 || hst.Delivery == nil || !reflect.DeepEqual(*hst.Delivery, hwant) {
		t.Fatalf("no-project status diagnostics:\n%+v\n%+v", hst.Delivery, hwant)
	}
	// An explicit scope plans for that scope.
	if pst, _ := home.StatusFor(StatusOptions{Me: claude, Projects: []string{"aiproxy"}}); pst.Delivery == nil || pst.Delivery.Scope != "aiproxy + global" || pst.Delivery.Applicable != applicable || pst.Delivery.Size > SessionStartBudget {
		t.Fatalf("-p aiproxy plan: %+v", pst.Delivery)
	}

	// MCP instructions have their own declared budget; the fixed guidance
	// is always kept.
	const base = "BASE GUIDANCE."
	md, err := s.MCPInstructionsDelivery(base)
	if err != nil || md.Surface != SurfaceMCPInstructions || md.Budget != MCPInstructionsBudget || md.Size > MCPInstructionsBudget || !strings.HasPrefix(md.Text, base+"\n\nRules from kb") {
		t.Fatalf("mcp plan: %v %+v", err, md)
	}
	if md.Rendered+md.Omitted() != applicable || md.OmittedBySize == 0 || len(ruleLines(md.Text)) != md.Rendered {
		t.Fatalf("mcp accounting: %+v", md)
	}
}

// A payload that fits is delivered whole and unchanged in shape.
func TestSessionStartWithinBudgetIsUntouched(t *testing.T) {
	newHub(t)
	repo := newCodeRepo(t, "git@github.com:AtlasCloudTeam/aiproxy.git")
	s := openAt(t, repo)
	addRule(t, s, "Use pnpm, never npm", "", "user said so")
	addRule(t, s, "Write commit messages in English", "global", "user said so")
	addRule(t, s, "Prefer table-driven tests", "", "")
	d, err := s.SessionStartDelivery(claude)
	if err != nil {
		t.Fatal(err)
	}
	rs, _ := s.Rules(nil, false)
	want := s.RenderRules(rs, false) + s.SessionBrief(claude) + "\n"
	if d.Text != want || d.Rendered != 2 || d.Omitted() != 0 || d.BriefLines != 1 || d.BriefOmitted != 0 || strings.Contains(d.Text, "left out") {
		t.Fatalf("plan: %+v\n%s", d, d.Text)
	}
	if st, _ := s.StatusFor(StatusOptions{Me: claude}); st.RulesOmitted != 0 || strings.Contains(RenderStatus(st), "leaves out") {
		t.Fatalf("status: %+v", st.Delivery)
	}
	// Nothing to say: nothing printed.
	if e, _ := openAt(t, t.TempDir()).SessionStartDelivery(claude); !strings.HasPrefix(e.Text, "Rules from kb") || e.Rendered != 1 {
		t.Fatalf("global-only plan: %+v", e)
	}
	empty := newRepo(t)
	if e, _ := empty.SessionStartDelivery(claude); e.Text != "" || e.Size != 0 {
		t.Fatalf("empty bundle should deliver nothing: %q", e.Text)
	}
}
