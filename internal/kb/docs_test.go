package kb

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

const designDoc = `# Gateway design

Intro text.

## Retry policy

Upstream 429 responses are retried with exponential backoff and jitter,
honouring Retry-After, at most three attempts per request.

` + "```" + `
# not a heading inside a fence
` + "```" + `

### Idempotency

Every retried request carries an Idempotency-Key header so upstream jobs
are not duplicated.

## Deployment

The gateway runs as a Kubernetes Deployment with three replicas.
`

func TestParseSections(t *testing.T) {
	secs := parseSections("docs/design.md", designDoc)
	var heads []string
	for _, s := range secs {
		heads = append(heads, s.Heading)
	}
	if strings.Join(heads, "|") != "Gateway design|Retry policy|Idempotency|Deployment" {
		t.Fatalf("headings: %v", heads)
	}
	idem := secs[2]
	if idem.Trail != "Gateway design › Retry policy" || idem.Line != 14 || !strings.Contains(secs[1].Text, "not a heading") {
		t.Fatalf("section: %+v", idem)
	}
	if got := secs[1].Snippet(60); !strings.HasPrefix(got, "Upstream 429 responses are retried") {
		t.Fatalf("snippet: %q", got)
	}
}

func TestIsDocPath(t *testing.T) {
	for p, want := range map[string]bool{
		"README.md": true, "docs/design.md": true, "AGENTS.md": true,
		"knowledge/facts/x.md": false, "node_modules/x/README.md": false, "vendor/a/b.md": false,
		".github/PULL_REQUEST_TEMPLATE.md": false, "CHANGELOG.md": false,
	} {
		if got := isDocPath(p, "knowledge"); got != want {
			t.Errorf("isDocPath(%q) = %v", p, got)
		}
	}
}

func TestDocsAreSourceOfTruth(t *testing.T) {
	s := newRepo(t)
	write(t, filepath.Join(s.CodeRoot, "docs", "design.md"), designDoc)
	gitRun(t, s, "add", "-A")
	gitRun(t, s, "commit", "-qm", "docs")

	// Restating a doc section is refused, naming the section.
	_, err := s.Add(AddInput{Type: "fact", Title: "Upstream 429 responses are retried with exponential backoff",
		Description: "Retries honour Retry-After with jitter, at most three attempts.", Actor: claude})
	var dc *DocCoveredError
	if !errors.As(err, &dc) || dc.Match.Section.Path != "docs/design.md" || dc.Match.Section.Heading != "Retry policy" {
		t.Fatalf("want doc-covered error for Retry policy, got %v", err)
	}
	// --force records it anyway; lint then flags the copy.
	forced, err := s.Add(AddInput{Type: "fact", Title: "Upstream 429 responses are retried with exponential backoff",
		Description: "Retries honour Retry-After with jitter, at most three attempts.", Force: true, Actor: claude})
	if err != nil {
		t.Fatal(err)
	}
	issues, _ := s.Lint(LintOptions{})
	found := false
	for _, i := range issues {
		if strings.Contains(i.Msg, "restates docs/design.md") && strings.Contains(i.Path, forced.Node.LocalID()) {
			found = true
		}
	}
	if !found {
		t.Fatalf("lint should flag the copy: %v", issues)
	}
	// Knowledge the docs don't hold goes in.
	res, err := s.Add(AddInput{Type: "learning", Title: "Envoy sidecar strips Idempotency-Key on HTTP/1.0 clients",
		Description: "Legacy clients lose the header at the mesh; upgrade or set it server-side.", Actor: codex})
	if err != nil {
		t.Fatalf("new knowledge refused: %v", err)
	}
	_ = res
	// Search lists matching doc sections alongside entries.
	q := SearchQuery{Text: "idempotency key duplicated jobs"}
	hits, _ := s.Search(q)
	out := s.RenderHits(hits, q)
	if !strings.Contains(out, "doc docs/design.md:14 (§ Idempotency)") {
		t.Fatalf("doc hit missing:\n%s", out)
	}
	if out := s.RenderHits(hits, SearchQuery{Text: q.Text, NoDocs: true}); strings.Contains(out, "doc docs/") {
		t.Fatalf("--no-docs ignored:\n%s", out)
	}
	if hs := s.DocHeadings(10); len(hs) != 4 || hs[1] != "docs/design.md § Retry policy" {
		t.Fatalf("headings: %v", hs)
	}
}
