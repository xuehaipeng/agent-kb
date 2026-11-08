package kb

import (
	"fmt"
	"sort"
	"strings"
)

// RenderHits prints search results as compact one-line headers (roughly
// 15-25 tokens each), followed by open reviews that touch the queried files.
func (s *Store) RenderHits(hits []Hit, q SearchQuery) string {
	var b strings.Builder
	if len(hits) == 0 {
		b.WriteString("no matching knowledge")
		if q.TrustedOnly {
			b.WriteString(" (trusted only; drop --trusted to include unverified claims)")
		}
		b.WriteString("\n")
	}
	for _, h := range hits {
		m := h.Node.Meta()
		fmt.Fprintf(&b, "%s [%s] %s - %s", h.Node.ID, h.Trust.Badge(), m.Title, oneLine(m.Description, 110))
		if h.PathMatch != "" {
			fmt.Fprintf(&b, " (anchored: %s)", h.PathMatch)
		}
		b.WriteString("\n")
	}
	if len(q.Files) > 0 {
		var files []string
		for _, f := range q.Files {
			files = append(files, s.RelToCode(f))
		}
		open, _ := s.Reviews(ReviewFilter{})
		for _, it := range open {
			if r := it.Node.Meta().Review; r != nil {
				for _, rf := range r.Files {
					if containsStr(files, rf) {
						fmt.Fprintf(&b, "open review %s [%s] touches %s\n", it.Node.ID, it.State, rf)
						break
					}
				}
			}
		}
	}
	return b.String()
}

// RenderNode prints a condensed node: header, trust trail, resources, body.
func (s *Store) RenderNode(n *Node) string {
	m := n.Meta()
	var b strings.Builder
	if n.TypeKey == "review" {
		return s.RenderReview(n)
	}
	t := s.Trust(n)
	fmt.Fprintf(&b, "# %s\n", m.Title)
	fmt.Fprintf(&b, "id: %s | type: %s | trust: %s", n.ID, n.TypeKey, t.State)
	if t.Stale {
		fmt.Fprintf(&b, " | STALE: %s", t.StaleReason)
	}
	b.WriteString("\n")
	if t.SupersededBy != "" {
		fmt.Fprintf(&b, "superseded by: %s\n", t.SupersededBy)
	}
	if len(m.Supersedes) > 0 {
		fmt.Fprintf(&b, "supersedes: %s\n", strings.Join(m.Supersedes, ", "))
	}
	fmt.Fprintf(&b, "generated: %s at %s", m.Generated.By, m.Generated.At)
	if m.Generated.Commit != "" {
		fmt.Fprintf(&b, " (commit %s)", m.Generated.Commit)
	}
	b.WriteString("\n")
	for _, e := range m.Verified {
		res := e.Result
		if res == "" {
			res = ResultConfirmed
		}
		fmt.Fprintf(&b, "%s: %s at %s", res, e.By, e.At)
		if e.Commit != "" {
			fmt.Fprintf(&b, " (commit %s)", e.Commit)
		}
		if e.Evidence != "" {
			fmt.Fprintf(&b, " - %s", e.Evidence)
		}
		b.WriteString("\n")
	}
	if len(m.Resources) > 0 {
		fmt.Fprintf(&b, "resources: %s\n", strings.Join(m.Resources, ", "))
	}
	if len(m.Tags) > 0 {
		fmt.Fprintf(&b, "tags: %s\n", strings.Join(m.Tags, ", "))
	}
	if m.StaleAfter != "" {
		fmt.Fprintf(&b, "stale_after: %s\n", m.StaleAfter)
	}
	for _, src := range m.Sources {
		fmt.Fprintf(&b, "source [^%s]: %s", src.ID, src.Resource)
		if src.Author != "" {
			fmt.Fprintf(&b, " by %s", src.Author)
		}
		b.WriteString("\n")
	}
	b.WriteString("\n")
	b.WriteString(strings.TrimSpace(n.Doc.Body))
	b.WriteString("\n")
	if t.State == StateUnverified || t.State == StateDisputed || t.Stale {
		fmt.Fprintf(&b, "\n(hypothesis: check against the code, then 'kb verify %s --confirm|--dispute -e ...')\n", n.ID)
	}
	return b.String()
}

// RenderReview prints a review packet for the reviewer: summary, range,
// diffstat, and the knowledge anchored to the touched files.
func (s *Store) RenderReview(n *Node) string {
	m := n.Meta()
	st, _ := s.ReviewState(n)
	var b strings.Builder
	fmt.Fprintf(&b, "# Review: %s\n", m.Title)
	fmt.Fprintf(&b, "id: %s | state: %s | author: %s", n.ID, st, m.Generated.By)
	r := m.Review
	if r != nil {
		fmt.Fprintf(&b, " | reviewer family must differ from: %s\n", r.ReviewerMustDifferFrom)
		fmt.Fprintf(&b, "range: %s..%s\n", r.Base, r.Head)
	} else {
		b.WriteString("\n")
	}
	b.WriteString("\n" + strings.TrimSpace(n.Doc.Body) + "\n")
	if r != nil && s.InGit() {
		args := append([]string{"diff", "--stat=100", r.Base, r.Head}, s.CodePathspec()...)
		if stat, err := GitRaw(s.GitTop(), args...); err == nil && strings.TrimSpace(stat) != "" {
			b.WriteString("\n## Diffstat\n\n```\n" + stat + "\n```\n")
		}
		var lines []string
		seen := map[string]bool{}
		for _, id := range r.Concepts {
			if c, err := s.Get(id); err == nil {
				seen[c.ID] = true
				t := s.Trust(c)
				lines = append(lines, fmt.Sprintf("- %s [%s] %s", c.ID, t.Badge(), c.Meta().Title))
			}
		}
		if hits, err := s.Search(SearchQuery{Files: r.Files, Limit: 10}); err == nil {
			for _, h := range hits {
				if !seen[h.Node.ID] {
					lines = append(lines, fmt.Sprintf("- %s [%s] %s", h.Node.ID, h.Trust.Badge(), h.Node.Meta().Title))
				}
			}
		}
		if len(lines) > 0 {
			b.WriteString("\n## Knowledge anchored to the touched files (check the change against these)\n\n")
			b.WriteString(strings.Join(lines, "\n") + "\n")
		}
	}
	if st == ReviewPending || st == ReviewChanges {
		fmt.Fprintf(&b, "\nVerdict: kb review verdict %s --approve -m \"...\" | --changes -f \"finding\" ...\n", n.ID)
	}
	return b.String()
}

// RenderReviews prints a review list.
func RenderReviews(items []ReviewItem) string {
	if len(items) == 0 {
		return "no reviews\n"
	}
	var b strings.Builder
	for _, it := range items {
		m := it.Node.Meta()
		files := 0
		rng := ""
		if m.Review != nil {
			files = len(m.Review.Files)
			rng = m.Review.Base + ".." + m.Review.Head
		}
		exp := ""
		if it.Expired {
			exp = ",expired"
		}
		fmt.Fprintf(&b, "%s [%s%s] %s - by %s, %d file(s), %s\n", it.Node.ID, it.State, exp, m.Title, m.Generated.By, files, rng)
	}
	return b.String()
}

// RenderStatus prints the status report.
func RenderStatus(r *StatusReport) string {
	var b strings.Builder
	fmt.Fprintf(&b, "knowledge: %d active", r.Total)
	var types []string
	for k, v := range r.ByType {
		types = append(types, fmt.Sprintf("%s %d", k, v))
	}
	sort.Strings(types)
	if len(types) > 0 {
		fmt.Fprintf(&b, " (%s)", strings.Join(types, ", "))
	}
	b.WriteString("\n")
	fmt.Fprintf(&b, "trust: corroborated %d, reviewed %d, unverified %d (%d older than 7d), disputed %d, deprecated %d\n",
		r.ByState[StateCorroborated], r.ByState[StateReviewed], r.ByState[StateUnverified], r.UnverifiedOld, r.ByState[StateDisputed], r.ByState[StateDeprecated])
	fmt.Fprintf(&b, "stale: %d", r.Stale)
	if len(r.StaleIDs) > 0 {
		fmt.Fprintf(&b, " (%s)", list3(r.StaleIDs))
	}
	b.WriteString("\n")
	var prod []string
	for k, v := range r.ClaimsByProducer {
		prod = append(prod, fmt.Sprintf("%s %d", k, v))
	}
	sort.Strings(prod)
	if len(prod) > 0 {
		fmt.Fprintf(&b, "claims by producer: %s\n", strings.Join(prod, ", "))
	}
	fmt.Fprintf(&b, "reviews open: %d\n", r.ReviewsOpen)
	if len(r.ReviewsForMe) > 0 {
		fmt.Fprintf(&b, "waiting for your review: %s\n", strings.Join(r.ReviewsForMe, ", "))
	}
	if len(r.ReviewsChanges) > 0 {
		fmt.Fprintf(&b, "changes requested on yours: %s\n", strings.Join(r.ReviewsChanges, ", "))
	}
	if len(r.Disputed) > 0 {
		fmt.Fprintf(&b, "disputed (human decides with 'kb resolve <id> --keep|--deprecate --as human:<you>'):\n")
		for _, id := range r.Disputed {
			fmt.Fprintf(&b, "  %s\n", id)
		}
	}
	if r.ParseErrors > 0 {
		fmt.Fprintf(&b, "parse errors: %d (run 'kb lint')\n", r.ParseErrors)
	}
	return b.String()
}

func containsStr(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}
