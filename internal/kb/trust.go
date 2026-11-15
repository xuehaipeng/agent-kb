package kb

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/xuehaipeng/agent-kb/internal/okf"
)

// Trust states, derived on every read. Nothing stores them.
//
// The model favours knowledge sharing over ceremony: an agent that checked a
// claim (ran the test, read the code) records it as verified on its own;
// confirmation by a different model family or a human is a stronger signal,
// not a gate. Every state applies to the current content revision only.
const (
	StateUnverified   = "unverified"    // recorded without evidence: a hypothesis
	StateVerified     = "verified"      // confirmed with evidence by the author's model family
	StateCrossChecked = "cross-checked" // also confirmed by a different model family
	StateReviewed     = "reviewed"      // confirmed by a human
	StateDisputed     = "disputed"      // the latest check on this revision says it is wrong
	StateDeprecated   = "deprecated"    // retracted or superseded
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
	Revision     int
	Stale        bool
	StaleReason  string
	Confirmers   []string // model families that confirmed the current revision
	Disputers    []string
	SupersededBy string
	Anchor       string // commit the current revision was last confirmed (or written) at

	// Live output only (empty in static trust, e.g. for index.md). Stale
	// stays the legacy "needs a look" flag: past its deadline or code changed.
	Overdue         bool      // past stale_after
	CodeFreshness   string    // Fresh*: unchanged | changed | unknown | not_applicable
	FreshnessCode   string    // Unknown* code when CodeFreshness is unknown
	FreshnessReason string    // why it is changed or unknown
	Baseline        *Baseline // what the code was compared against

	importAnchor bool // the anchor was stamped by a memory/rule import
}

// Badge renders the compact label used in search results: evidence state,
// then code freshness, then "stale" when the entry needs a look (code
// changed or past its deadline). "unknown-freshness" means the code could
// not be compared on this machine; it is not a pass.
func (t Trust) Badge() string {
	b := t.State
	switch t.CodeFreshness {
	case FreshUnchanged:
		b += ",code-unchanged"
	case FreshChanged:
		b += ",code-changed"
	case FreshUnknown:
		b += ",unknown-freshness"
	case FreshNotApplicable:
		b += ",code-not-applicable"
	}
	if t.Stale {
		b += ",stale"
	}
	return b
}

// Ack is the label in a write acknowledgement ("added X [verified]"): what
// the write established. Code freshness is named only when it is unknown,
// because then no commit could be recorded or compared for the entry.
func (t Trust) Ack() string {
	b := t.State
	if t.CodeFreshness == FreshUnknown {
		b += ",unknown-freshness"
	}
	if t.Stale {
		b += ",stale"
	}
	return b
}

// Trusted reports whether the state is backed by evidence.
func (t Trust) Trusted() bool {
	return t.State == StateVerified || t.State == StateCrossChecked || t.State == StateReviewed
}

// ContentAuthor is who wrote the current revision.
func ContentAuthor(m *okf.Meta) okf.Event {
	if m.Rev() > 1 && m.Updated != nil && m.Updated.By != "" {
		return *m.Updated
	}
	return m.Generated
}

// supersededIndex maps a node ID to the ID of the node that supersedes it.
func (s *Store) supersededIndex() map[string]string {
	out := map[string]string{}
	for _, n := range s.nodes {
		for _, ref := range n.Meta().Supersedes {
			t := s.resolveRef(n, ref)
			if t == nil {
				continue
			}
			if prev, ok := out[t.ID]; !ok || n.ID > prev {
				out[t.ID] = n.ID
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
	rev := m.Rev()
	author := ContentAuthor(m)
	t := Trust{State: StateUnverified, Revision: rev, Anchor: author.Commit}
	anchorAt, anchorBy := author.At, author.By
	authorFam := s.Cfg.FamilyOf(author.By)

	// Events on the current revision, in order. The latest dispute (if any)
	// splits them: only confirmations after it count.
	var events []okf.Event
	for _, e := range m.Verified {
		if okf.EventRev(e) == rev {
			events = append(events, e)
		}
	}
	lastDispute := -1
	disputers := map[string]bool{}
	for i, e := range events {
		if e.Result == ResultDisputed {
			lastDispute = i
			disputers[s.Cfg.FamilyOf(e.By)] = true
		}
	}
	confirmers := map[string]bool{}
	human, cross := false, false
	for _, e := range events[lastDispute+1:] {
		if e.Result != "" && e.Result != ResultConfirmed {
			continue
		}
		fam := s.Cfg.FamilyOf(e.By)
		confirmers[fam] = true
		switch {
		case fam == "human":
			human = true
		case fam != authorFam:
			cross = true
		}
		if e.Commit != "" {
			t.Anchor = e.Commit
			anchorAt, anchorBy = e.At, e.By
		}
	}
	// An import stamps whatever HEAD the chosen checkout had at import time:
	// that commit says nothing about where the claim was checked. A later
	// check by anyone (or an update) replaces it with a real anchor.
	t.importAnchor = importedSource(m) && anchorAt == m.Generated.At && anchorBy == m.Generated.By
	t.Confirmers = sortedKeys(confirmers)
	t.Disputers = sortedKeys(disputers)
	lastIsDispute := len(events) > 0 && events[len(events)-1].Result == ResultDisputed

	switch {
	case strings.EqualFold(m.Status, "deprecated"):
		t.State = StateDeprecated
	case sup[n.ID] != "":
		t.State = StateDeprecated
	case lastIsDispute:
		t.State = StateDisputed
	case human:
		t.State = StateReviewed
	case cross:
		t.State = StateCrossChecked
	case len(confirmers) > 0:
		t.State = StateVerified
	}
	t.SupersededBy = sup[n.ID]

	if dynamic && t.State != StateDeprecated {
		if m.StaleAfter != "" {
			if ts, err := parseTime(m.StaleAfter); err == nil && s.Now().After(ts) {
				t.Overdue = true
				t.Stale = true
				t.StaleReason = "past stale_after " + ts.Format("2006-01-02")
			}
		}
		// The deadline and the code check are independent: an overdue entry
		// can also have changed or uncheckable code, and both are reported.
		t.CodeFreshness, t.FreshnessCode, t.FreshnessReason, t.Baseline = s.codeFreshness(n, t.Anchor, t.importAnchor)
		if t.CodeFreshness == FreshChanged {
			if !t.Stale {
				t.StaleReason = t.FreshnessReason
			}
			t.Stale = true
		}
	}
	return t
}

// importedSource reports whether an entry came from a memory or rule-file
// import (kb extract --scan memories, kb rules import).
func importedSource(m *okf.Meta) bool {
	for _, src := range m.Sources {
		if strings.HasPrefix(src.ID, "memory-") || strings.HasPrefix(src.ID, "rules-") {
			return true
		}
	}
	return false
}

// ReviewState derives the state of a review packet: the latest verdict on
// the current head decides; a revision reopens it.
func (s *Store) ReviewState(n *Node) (state string, verdicts int) {
	m := n.Meta()
	if strings.EqualFold(m.Status, ReviewClosed) {
		return ReviewClosed, len(m.Verified)
	}
	head := ""
	if m.Review != nil {
		head = m.Review.Head
	}
	state = ReviewPending
	for _, e := range m.Verified {
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
