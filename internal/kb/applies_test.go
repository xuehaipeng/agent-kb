package kb

import (
	"strings"
	"testing"

	"github.com/xuehaipeng/agent-kb/internal/okf"
)

func TestAppliesToFile(t *testing.T) {
	type c struct {
		pattern, base, file string
		want                bool
	}
	for _, tc := range []c{
		// Claude Code's documented examples.
		{"**/*.ts", "", "a.ts", true},
		{"**/*.ts", "", "src/deep/a.ts", true},
		{"**/*.ts", "", "src/a.tsx", false},
		{"src/**/*", "", "src/a.go", true},
		{"src/**/*", "", "src/x/y/a.go", true},
		{"src/**/*", "", "lib/src/a.go", false},
		{"*.md", "", "README.md", true},
		{"*.md", "", "docs/guide/intro.md", true}, // a bare name matches at any depth
		{"/*.md", "", "README.md", true},
		{"/*.md", "", "docs/intro.md", false}, // rooted: the base directory only
		{"src/components/*.tsx", "", "src/components/Button.tsx", true},
		{"src/components/*.tsx", "", "src/components/forms/Input.tsx", false},
		// Braces, including nested ones.
		{"src/**/*.{ts,tsx}", "", "src/ui/a.tsx", true},
		{"src/**/*.{ts,tsx}", "", "src/ui/a.js", false},
		{"{a,b}/{c,d}/*.{ts,tsx}", "", "b/c/x.ts", true},
		{"{a,b}/{c,d}/*.{ts,tsx}", "", "b/e/x.ts", false},
		{"{src,{lib,pkg}}/*.go", "", "pkg/a.go", true},
		// Brackets, and a literal one.
		{"src/[ab]*.go", "", "src/alpha.go", true},
		{"src/[ab]*.go", "", "src/gamma.go", false},
		{"src/[!ab]*.go", "", "src/gamma.go", true},
		// A negated class stays within one segment, like "*" and "?".
		{"test[!s]*.go", "", "testa.go", true},
		{"test[!s]*.go", "", "tests.go", false},
		{"test[!s]*.go", "", "test/foo.go", false},
		{"test[^s]*.go", "", "test/foo.go", false},
		{"src/[!a]/x.go", "", "src/b/x.go", true},
		{"test[0-9]*.go", "", "test1.go", true},
		// An escaped character in brackets is that character.
		{`file[a\-z].go`, "", "file-.go", true},
		{`file[a\-z].go`, "", "filea.go", true},
		{`file[a\-z].go`, "", "filem.go", false},
		{`file[a-z].go`, "", "filem.go", true},
		{`file[\]x].go`, "", "file].go", true},
		{`file[!\-].go`, "", "file-.go", false},
		{`file[!\-].go`, "", "filea.go", true},
		{`file[a^].go`, "", "file^.go", true},
		{"test[0-9]*.go", "", "test/1.go", false},
		{`photos \[2024\]/**`, "", "photos [2024]/jan/a.jpg", true},
		// A base directory: patterns are relative to it and stop at it.
		{"**", "packages/web", "packages/web/src/a.ts", true},
		{"**", "packages/web", "packages/api/src/a.ts", false},
		{"src/*.ts", "packages/web", "packages/web/src/a.ts", true},
		{"src/*.ts", "packages/web", "src/a.ts", false},
		{"*.ts", "", "packages/web/x/a.ts", true},
		{"?.go", "", "a.go", true},
		{"?.go", "", "ab.go", false},
	} {
		a := &okf.Applicability{Paths: []string{tc.pattern}, Base: tc.base}
		if err := ValidateApplies(a); err != nil {
			t.Errorf("%q: unexpected error %v", tc.pattern, err)
		}
		if got := AppliesToFile(a, tc.file); got != tc.want {
			t.Errorf("pattern %q base %q file %q: got %v, want %v", tc.pattern, tc.base, tc.file, got, tc.want)
		}
	}
	// A pattern that cannot be matched exactly is an error and matches
	// nothing: it is never widened.
	for _, bad := range []string{"photos [2024/**", "src/{a,b", "src/a}", "", `src\`, "test[.-0]*.go", "src/[a/]x.go", "{" + strings.Repeat("{a,b,c,d,e,f,g,h,i,j}", 4) + "}"} {
		a := &okf.Applicability{Paths: []string{bad}}
		if err := ValidateApplies(a); err == nil {
			t.Errorf("%q: expected an error", bad)
		}
		if AppliesToFile(a, "photos [2024/a.jpg") || AppliesToFile(a, "src/a") {
			t.Errorf("%q matched a file", bad)
		}
	}
	// One bad pattern does not disable the others in a list.
	mixed := &okf.Applicability{Paths: []string{"photos [2024/**", "lib/**/*.ts"}}
	if !AppliesToFile(mixed, "lib/x/a.ts") {
		t.Error("a valid pattern next to an invalid one stopped matching")
	}
	if ValidateApplies(&okf.Applicability{}) == nil {
		t.Error("an applicability without paths must be rejected")
	}
}

// A conditional rule applies only to matching files: it is never part of
// always-on delivery, and resources do not make a rule conditional.
func TestConditionalRulesAreNotAlwaysOn(t *testing.T) {
	newHub(t)
	repo := newCodeRepo(t, "git@github.com:AtlasCloudTeam/aiproxy.git")
	s := openAt(t, repo)
	add := func(in AddInput) *Node {
		t.Helper()
		in.Type, in.Evidence, in.Actor, in.Force = "rule", "user said so", claude, true
		r, err := s.Add(in)
		if err != nil {
			t.Fatal(err)
		}
		return r.Node
	}
	always := add(AddInput{Title: "Use pnpm, never npm", Description: "lockfile is pnpm-lock.yaml"})
	// Resources say what a rule depends on; they do not limit when it applies.
	anchored := add(AddInput{Title: "Keep handlers thin", Description: "logic lives in services", Resources: []string{"src/api/**"}})
	api := add(AddInput{Title: "Validate every API handler input", Description: "use the shared schema helpers", AppliesTo: &okf.Applicability{Paths: []string{"src/api/**/*.ts"}}})
	web := add(AddInput{Title: "Web components use CSS modules", Description: "no inline styles", AppliesTo: &okf.Applicability{Paths: []string{"**"}, Base: "packages/web"}})
	global := add(AddInput{Title: "Markdown lines wrap at 100 columns", Description: "docs style", Project: "global", AppliesTo: &okf.Applicability{Paths: []string{"*.md"}}})

	rs, err := s.Rules(nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if got := ids(append(append([]*Node{}, rs.Scoped...), rs.Global...)); len(got) != 2 || got[0] != anchored.ID || got[1] != always.ID {
		t.Fatalf("always-on rules: %v", got)
	}
	if len(rs.Conditional) != 3 || rs.Applicable() != 2 {
		t.Fatalf("conditional %v applicable %d", ids(rs.Conditional), rs.Applicable())
	}
	// No always-on surface carries a conditional rule's text.
	d, _ := s.SessionStartDelivery(claude)
	md, _ := s.MCPInstructionsDelivery("BASE.")
	for name, text := range map[string]string{"kb rules": s.RenderRules(rs, true), "session start": d.Text, "mcp instructions": md.Text, "user-level block": s.GlobalRulesText()} {
		for _, n := range []*Node{api, web, global} {
			if strings.Contains(text, n.Meta().Title) {
				t.Errorf("%s delivers the conditional rule %q:\n%s", name, n.Meta().Title, text)
			}
		}
	}
	if !strings.Contains(d.Text, "(+3 rule(s) apply only to particular files: kb rules -f <path>") || !strings.Contains(d.Text, "Keep handlers thin") {
		t.Fatalf("session start should name that conditional rules exist and keep the anchored rule:\n%s", d.Text)
	}
	if d.Applicable != 2 || d.Rendered != 2 {
		t.Fatalf("delivery counts: %+v", d)
	}
	st, _ := s.StatusFor(StatusOptions{Me: claude})
	if st.Rules != 2 || st.RulesConditional != 3 || !strings.Contains(RenderStatus(st), "3 conditional (only for matching files") {
		t.Fatalf("status: rules=%d conditional=%d", st.Rules, st.RulesConditional)
	}

	// They surface for a matching file, and only for one.
	match := func(files ...string) string {
		t.Helper()
		ns, err := s.RulesForFiles(files, nil, false)
		if err != nil {
			t.Fatal(err)
		}
		return strings.Join(ids(ns), " ")
	}
	if got := match("src/api/users/create.ts"); got != api.ID {
		t.Fatalf("api file: %s", got)
	}
	if got := match("src/web/page.ts", "cmd/main.go"); got != "" {
		t.Fatalf("unrelated files: %s", got)
	}
	if got := match("packages/web/src/Button.tsx"); got != web.ID {
		t.Fatalf("web file: %s", got)
	}
	if got := match("docs/guide.md"); got != global.ID {
		t.Fatalf("markdown file: %s", got)
	}
	out := s.RenderFileRules([]*Node{api}, []string{"src/api/users/create.ts"})
	if !strings.Contains(out, "Rules from kb that apply to src/api/users/create.ts") || !strings.Contains(out, "- Validate every API handler input - use the shared schema helpers ["+api.ID+"]") {
		t.Fatalf("file rules:\n%s", out)
	}
	// Searching by file shows them too: that is when they matter.
	q := SearchQuery{Files: []string{"src/api/users/create.ts"}, NoDocs: true}
	hits, _ := s.Search(q)
	if out := s.RenderHits(hits, q); !strings.Contains(out, "rule for this file: Validate every API handler input") || strings.Contains(out, "Web components") {
		t.Fatalf("search by file:\n%s", out)
	}
	// Inspection lists them with their condition.
	all, _ := s.AllRules(nil, false)
	if out := s.RenderRules(all, false); !strings.Contains(out, "Conditional rules (not delivered at session start") || !strings.Contains(out, "(when working on ** under packages/web/)") {
		t.Fatalf("--all-rules:\n%s", out)
	}
	if out := s.RenderNode(api); !strings.Contains(out, "conditional rule: applies only when working on src/api/**/*.ts") {
		t.Fatalf("show:\n%s", out)
	}
	// Another project's conditional rule stays out of this one's files.
	other := openAt(t, newCodeRepo(t, "git@github.com:AtlasCloudTeam/kubedl.git"))
	if ns, _ := other.RulesForFiles([]string{"src/api/users/create.ts", "README.md"}, nil, false); len(ns) != 1 || ns[0].ID != global.ID {
		t.Fatalf("cross-project file rules: %v", ids(ns))
	}

	// Only rules can be conditional, and only with patterns that can be matched.
	if _, err := s.Add(AddInput{Type: "fact", Title: "Handlers live in src/api", Description: "layout", Actor: claude, AppliesTo: &okf.Applicability{Paths: []string{"src/api/**"}}}); err == nil || !strings.Contains(err.Error(), "only rules") {
		t.Fatalf("conditional fact: %v", err)
	}
	if _, err := s.Add(AddInput{Type: "rule", Title: "Broken pattern rule", Description: "x", Actor: claude, AppliesTo: &okf.Applicability{Paths: []string{"photos [2024/**"}}}); err == nil || !strings.Contains(err.Error(), "applies_to") {
		t.Fatalf("invalid pattern: %v", err)
	}
	if _, err := s.WriteIndex(); err != nil {
		t.Fatal(err)
	}
	issues, _ := s.Lint(LintOptions{})
	for _, i := range issues {
		if i.Severity == "error" {
			t.Errorf("lint: %s", i)
		}
	}
}
