package kb

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xuehaipeng/agent-kb/internal/okf"
)

func TestSourceDeadline(t *testing.T) {
	now := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	ttl := 90 * 24 * time.Hour
	day := 24 * time.Hour
	for _, c := range []struct {
		name   string
		ttl    time.Duration
		date   time.Time
		basis  string
		due    time.Time
		wantBy string
	}{
		{"old source: due at once", ttl, now.Add(-400 * day), BasisSourceUpdatedAt, now.Add(-310 * day), BasisSourceUpdatedAt},
		{"recent source", ttl, now.Add(-10 * day), BasisSourceMtime, now.Add(80 * day), BasisSourceMtime},
		{"slightly ahead (clock skew): capped at the import time", ttl, now.Add(2 * time.Hour), BasisSourceMtime, now.Add(ttl), BasisSourceMtime},
		{"future date: not trusted", ttl, now.Add(30 * day), BasisSourceUpdatedAt, now.Add(7 * day), BasisUndated},
		{"no date: review window", ttl, time.Time{}, "", now.Add(7 * day), BasisUndated},
		{"no date, short ttl: never longer than the ttl", 3 * day, time.Time{}, "", now.Add(3 * day), BasisUndated},
		{"type without expiry", 0, now.Add(-400 * day), BasisSourceUpdatedAt, time.Time{}, ""},
	} {
		due, basis := sourceDeadline(now, c.ttl, c.date, c.basis)
		if !due.Equal(c.due) || basis != c.wantBy {
			t.Errorf("%s: got %s (%s), want %s (%s)", c.name, due, basis, c.due, c.wantBy)
		}
		if c.ttl > 0 && due.After(now.Add(c.ttl)) {
			t.Errorf("%s: deadline %s is later than an import-time deadline", c.name, due)
		}
	}
}

// An entry ingested from a source records no commit and takes its deadline
// from the source's date; a later real check gives it both.
func TestSourceOnlyEntries(t *testing.T) {
	s := newRepo(t)
	if s.HeadCommit() == "" {
		t.Fatal("fixture needs a HEAD")
	}
	old := time.Date(2025, 1, 10, 9, 30, 0, 0, time.UTC)
	src := []okf.Source{{ID: "memory-claude-code", Resource: "~/.claude/projects/x/memory/a.md", LastModified: old.Format(time.RFC3339), IngestedAt: "2026-11-08T12:00:00Z", Session: "sess-1"}}
	res, err := s.Add(AddInput{Type: "fact", Title: "Gateway retries are capped at three", Description: "Set in the gateway config.", Body: "Three attempts. [^memory-claude-code]",
		Resources: []string{"internal/gateway/**"}, Evidence: "saved in claude-code's memory", Actor: claude, Sources: src,
		SourceOnly: true, SourceDate: old, SourceDateBasis: BasisSourceUpdatedAt})
	if err != nil {
		t.Fatal(err)
	}
	m := res.Node.Meta()
	if m.Generated.Commit != "" || len(m.Verified) != 1 || m.Verified[0].Commit != "" {
		t.Fatalf("a source-only entry must not record the import's HEAD: %+v", m)
	}
	if m.StaleAfter != "2025-04-10T09:30:00Z" || m.StaleBasis != BasisSourceUpdatedAt || m.Sources[0].IngestedAt == "" || m.Sources[0].Session != "sess-1" {
		t.Fatalf("deadline and source metadata: %+v", m)
	}
	tr := s.Trust(res.Node)
	// Source-backed trust stays; the deadline is past and says what it is
	// based on; code that cannot be compared is unknown, not a pass.
	if tr.State != StateVerified || !tr.Trusted() || !tr.Overdue || !tr.Stale || tr.StaleReason != "past stale_after 2025-04-10 (deadline from source_updated_at)" {
		t.Fatalf("trust: %+v", tr)
	}
	if tr.CodeFreshness != FreshUnknown || tr.FreshnessCode != UnknownNoAnchor {
		t.Fatalf("no anchor must mean unknown code freshness: %+v", tr)
	}
	if out := s.RenderNode(res.Node); !strings.Contains(out, "(deadline from source_updated_at; when the claim was last observed is unknown)") || !strings.Contains(out, "source modified 2025-01-10T09:30:00Z, ingested 2026-11-08T12:00:00Z, from session sess-1") {
		t.Fatalf("show:\n%s", out)
	}

	// A conditional update refuses a stale revision.
	body := "Three attempts, then the request fails. [^memory-claude-code]"
	_, _, err = s.Update(UpdateInput{Ref: res.Node.ID, Body: &body, Reason: "source changed", Actor: claude, ExpectRevision: 7})
	var conflict *RevisionConflictError
	if !errors.As(err, &conflict) || conflict.Got != 1 || conflict.Want != 7 {
		t.Fatalf("want a revision conflict, got %v", err)
	}
	// Refreshing from a newer source: no commit, and the deadline does not move later.
	newer := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	src[0].LastModified = newer.Format(time.RFC3339)
	n, _, err := s.Update(UpdateInput{Ref: res.Node.ID, Body: &body, Reason: "source changed", Evidence: "saved in claude-code's memory", Actor: claude,
		ExpectRevision: 1, Sources: src, SetSources: true, SourceOnly: true, SourceDate: newer, SourceDateBasis: BasisSourceUpdatedAt})
	if err != nil {
		t.Fatal(err)
	}
	m = n.Meta()
	if m.Rev() != 2 || m.Updated.Commit != "" || m.Verified[len(m.Verified)-1].Commit != "" || m.StaleAfter != "2025-04-10T09:30:00Z" || m.StaleBasis != BasisSourceUpdatedAt || m.Sources[0].LastModified != newer.Format(time.RFC3339) {
		t.Fatalf("source refresh: %+v", m)
	}
	// An entry with a later deadline is pulled earlier by an older source date.
	res2, _ := s.Add(AddInput{Type: "fact", Title: "Queue depth alarm fires at 500", Description: "Paging threshold.", Evidence: "read the alert rules", Actor: claude})
	before := res2.Node.Meta().StaleAfter
	desc := "Paging threshold for the job queue."
	n2, _, err := s.Update(UpdateInput{Ref: res2.Node.ID, Description: &desc, Reason: "source changed", Actor: claude, SourceOnly: true, SourceDate: old, SourceDateBasis: BasisSourceMtime})
	if err != nil || n2.Meta().StaleAfter != "2025-04-10T09:30:00Z" || n2.Meta().StaleBasis != BasisSourceMtime || before == n2.Meta().StaleAfter {
		t.Fatalf("earlier source deadline: %v %+v", err, n2.Meta())
	}

	// A real check of the code establishes an anchor and a fresh deadline,
	// and the ingestion basis no longer applies.
	n, tr, err = s.Verify(VerifyInput{Ref: res.Node.ID, Result: ResultConfirmed, Evidence: "read internal/gateway/stream.go", Actor: codex})
	if err != nil {
		t.Fatal(err)
	}
	m = n.Meta()
	if m.Verified[len(m.Verified)-1].Commit != s.HeadCommit() || m.StaleBasis != "" || m.StaleAfter != "2027-02-06T12:00:00Z" || n.Doc.Has("stale_basis") {
		t.Fatalf("after a real check: %+v", m)
	}
	if tr.State != StateCrossChecked || tr.Stale || tr.CodeFreshness != FreshUnchanged {
		t.Fatalf("trust after a real check: %+v", tr)
	}

	// Types without an expiry get none, and no basis.
	rule, err := s.Add(AddInput{Type: "rule", Title: "Never mock the database in integration tests", Description: "A mocked migration passed while production failed.", Evidence: "saved in claude-code's memory", Actor: claude,
		SourceOnly: true, SourceDate: old, SourceDateBasis: BasisSourceUpdatedAt})
	if err != nil || rule.Node.Meta().StaleAfter != "" || rule.Node.Meta().StaleBasis != "" || rule.Node.Meta().Generated.Commit != "" || !rule.Trust.Trusted() {
		t.Fatalf("source-only rule: %v %+v", err, rule.Node.Meta())
	}
	// The files stay valid.
	if _, err := s.WriteIndex(); err != nil {
		t.Fatal(err)
	}
	issues, _ := s.Lint(LintOptions{})
	for _, i := range issues {
		if i.Severity == "error" {
			t.Errorf("lint: %s", i)
		}
	}
	if _, err := Open(filepath.Dir(res.Node.Path) + "/.."); err != nil {
		t.Fatal(err)
	}
}
