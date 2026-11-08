package kb

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// Trust states, derived on every read. Nothing stores them.
const (
	StateUnverified   = "unverified"
	StateCorroborated = "corroborated"
	StateReviewed     = "reviewed" // confirmed by a human
	StateDisputed     = "disputed"
	StateDeprecated   = "deprecated"
)

// Review states.
const (
	ReviewPending  = "pending"
	ReviewChanges  = "changes_requested"
	ReviewApproved = "approved"
	ReviewClosed   = "closed"
)

// Verification results.
const (
	ResultConfirmed = "confirmed"
	ResultDisputed  = "disputed"
	ResultApproved  = "approved"
	ResultChanges   = "changes_requested"
)

// Trust is the derived trust view of a node.
type Trust struct {
	State        string
	Stale        bool
	StaleReason  string
	Confirmers   []string // distinct cross-family verifier families that confirmed
	Disputers    []string
	SupersededBy string
	Anchor       string // commit the claim was last confirmed (or generated) at
}

// Badge renders the compact trust label used in search results.
func (t Trust) Badge() string {
	b := t.State
	if t.Stale {
		b += ",stale"
	}
	return b
}

// supersededIndex maps a node ID to the ID of the node that supersedes it.
func (s *Store) supersededIndex() map[string]string {
	out := map[string]string{}
	for _, n := range s.nodes {
		for _, ref := range n.Meta().Supersedes {
			id := strings.TrimSuffix(strings.TrimPrefix(ref, "/"), ".md")
			if t, err := s.Get(id); err == nil {
				id = t.ID
			}
			if prev, ok := out[id]; !ok || n.ID > prev {
				out[id] = n.ID
			}
		}
	}
	return out
}

// Trust derives the trust state of a knowledge node.
func (s *Store) Trust(n *Node) Trust {
	return s.trust(n, s.supersededIndex(), true)
}

// TrustStatic derives trust without time- or git-dependent staleness, for
// deterministic outputs such as index.md.
func (s *Store) TrustStatic(n *Node, sup map[string]string) Trust {
	return s.trust(n, sup, false)
}

func (s *Store) trust(n *Node, sup map[string]string, dynamic bool) Trust {
	m := n.Meta()
	t := Trust{State: StateUnverified, Anchor: m.Generated.Commit}
	genFam := s.Cfg.FamilyOf(m.Generated.By)

	humanConfirmed := false
	disputed := false
	confirmers := map[string]bool{}
	disputers := map[string]bool{}
	for _, e := range m.Verified {
		fam := s.Cfg.FamilyOf(e.By)
		switch {
		case fam == "human":
			// A human event resets the dispute ledger.
			disputed = false
			disputers = map[string]bool{}
			humanConfirmed = e.Result == "" || e.Result == ResultConfirmed
			if !humanConfirmed {
				disputed = true
				disputers["human"] = true
			}
			if e.Commit != "" && humanConfirmed {
				t.Anchor = e.Commit
			}
		case fam == genFam:
			// Same-family verification is not independent; ignore it.
		case e.Result == ResultDisputed:
			disputed = true
			disputers[fam] = true
		case e.Result == "" || e.Result == ResultConfirmed:
			confirmers[fam] = true
			if e.Commit != "" {
				t.Anchor = e.Commit
			}
		}
	}
	t.Confirmers = sortedKeys(confirmers)
	t.Disputers = sortedKeys(disputers)

	switch {
	case strings.EqualFold(m.Status, "deprecated"):
		t.State = StateDeprecated
	case sup[n.ID] != "":
		t.State = StateDeprecated
		t.SupersededBy = sup[n.ID]
	case disputed:
		t.State = StateDisputed
	case humanConfirmed:
		t.State = StateReviewed
	case len(confirmers) > 0:
		t.State = StateCorroborated
	}
	if sup[n.ID] != "" {
		t.SupersededBy = sup[n.ID]
	}

	if dynamic && t.State != StateDeprecated {
		if m.StaleAfter != "" {
			if ts, err := parseTime(m.StaleAfter); err == nil && s.Now().After(ts) {
				t.Stale = true
				t.StaleReason = "past stale_after " + ts.Format("2006-01-02")
			}
		}
		if !t.Stale && len(m.Resources) > 0 && t.Anchor != "" {
			if files, ok := s.ChangedSince(t.Anchor); ok {
				for _, f := range files {
					if MatchAny(m.Resources, f) {
						t.Stale = true
						t.StaleReason = fmt.Sprintf("code changed since %s: %s", short(t.Anchor), f)
						break
					}
				}
			}
		}
	}
	return t
}

// ReviewState derives the state of a review packet: the latest verdict from a
// family other than the author's decides, a revision reopens it.
func (s *Store) ReviewState(n *Node) (state string, verdicts int) {
	m := n.Meta()
	if strings.EqualFold(m.Status, ReviewClosed) {
		return ReviewClosed, len(m.Verified)
	}
	authorFam := s.Cfg.FamilyOf(m.Generated.By)
	head := ""
	if m.Review != nil {
		head = m.Review.Head
	}
	state = ReviewPending
	for _, e := range m.Verified {
		fam := s.Cfg.FamilyOf(e.By)
		if fam == authorFam && fam != "human" {
			continue
		}
		// A verdict on an older head no longer applies after a revision.
		if head != "" && e.Commit != "" && !sameCommit(e.Commit, head) {
			continue
		}
		switch e.Result {
		case ResultApproved, ResultConfirmed:
			state = ReviewApproved
			verdicts++
		case ResultChanges, ResultDisputed:
			state = ReviewChanges
			verdicts++
		}
	}
	return state, verdicts
}

func sameCommit(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	if len(a) > len(b) {
		a, b = b, a
	}
	return strings.HasPrefix(b, a)
}

func short(c string) string {
	if len(c) > 8 {
		return c[:8]
	}
	return c
}

func parseTime(s string) (time.Time, error) {
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05", "2006-01-02"} {
		if t, err := time.Parse(layout, strings.TrimSpace(s)); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("not an ISO 8601 timestamp: %q", s)
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
