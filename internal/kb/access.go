package kb

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"
)

// Local access statistics: how often each entry was retrieved on this
// machine. They are a maintenance signal ("nobody has looked at this in a
// long time") and nothing more. They never change an entry's trust, never
// expire or delete anything, and never leave this machine. An entry that is
// rarely retrieved can still be right, and a hub synced from other machines
// is retrieved there, not here.
//
// Three things are kept apart, because they are not the same evidence that
// a model used an entry:
//   - listed: it appeared as a one-line header in search results;
//   - shown: its content was returned by 'kb show' or kb_show;
//   - delivered: it was pushed into a session (a rule, through a hook, the
//     MCP instructions or 'kb rules').
const (
	AccessListed    = "listed"
	AccessShown     = "shown"
	AccessDelivered = "delivered"
)

// AccessStats is the local retrieval record.
type AccessStats struct {
	Since   string                  `json:"since"`
	Entries map[string]*EntryAccess `json:"entries"`
}

// EntryAccess counts one entry's retrievals.
type EntryAccess struct {
	Listed      int    `json:"listed,omitempty"`
	ListedAt    string `json:"listed_at,omitempty"`
	Shown       int    `json:"shown,omitempty"`
	ShownAt     string `json:"shown_at,omitempty"`
	Delivered   int    `json:"delivered,omitempty"`
	DeliveredAt string `json:"delivered_at,omitempty"`
}

func (s *Store) accessPath() string { return s.LocalPath("access.json") }

// LoadAccess reads the local access statistics (empty when none).
func (s *Store) LoadAccess() *AccessStats {
	a := &AccessStats{Entries: map[string]*EntryAccess{}}
	if data, err := os.ReadFile(s.accessPath()); err == nil {
		_ = json.Unmarshal(data, a)
	}
	if a.Entries == nil {
		a.Entries = map[string]*EntryAccess{}
	}
	return a
}

// NoteAccess records that entries were retrieved. Best effort: a hub that
// cannot be written, or KB_NO_ACCESS_LOG=1, simply records nothing.
func (s *Store) NoteAccess(kind string, ids []string) {
	if len(ids) == 0 || os.Getenv("KB_NO_ACCESS_LOG") != "" {
		return
	}
	unlock, err := s.lockFile("access.lock")
	if err != nil {
		return
	}
	defer unlock()
	a := s.LoadAccess()
	now := s.Now().UTC().Format(time.RFC3339)
	if a.Since == "" {
		a.Since = now
	}
	for _, id := range ids {
		e := a.Entries[id]
		if e == nil {
			e = &EntryAccess{}
			a.Entries[id] = e
		}
		switch kind {
		case AccessListed:
			e.Listed, e.ListedAt = e.Listed+1, now
		case AccessShown:
			e.Shown, e.ShownAt = e.Shown+1, now
		case AccessDelivered:
			e.Delivered, e.DeliveredAt = e.Delivered+1, now
		}
	}
	if data, err := json.Marshal(a); err == nil {
		_, _ = WriteFileIfChanged(s.accessPath(), data)
	}
}

// NoteHits records search results as listed.
func (s *Store) NoteHits(hits []Hit) {
	ids := make([]string, 0, len(hits))
	for _, h := range hits {
		ids = append(ids, h.Node.ID)
	}
	s.NoteAccess(AccessListed, ids)
}

// AccessSummary is the part of the statistics 'kb status' reports.
type AccessSummary struct {
	Since          string   `json:"since,omitempty"`
	Retrieved      int      `json:"retrieved"`       // active entries in scope listed, shown or delivered at least once here
	NeverRetrieved int      `json:"never_retrieved"` // active entries in scope with no record on this machine
	NeverIDs       []string `json:"never_retrieved_ids,omitempty"`
}

// RenderAccess lists what was never retrieved here and what was retrieved
// longest ago, for the entries in scope.
func (s *Store) RenderAccess(o StatusOptions, limit int) (string, error) {
	nodes, err := s.Nodes()
	if err != nil {
		return "", err
	}
	a := s.LoadAccess()
	inScope := s.scopeFilter(o.Projects, o.AllProjects)
	sup := s.supersededIndex()
	type row struct {
		id   string
		e    *EntryAccess
		last string
	}
	var never []string
	var rows []row
	for _, n := range nodes {
		if n.TypeKey == "review" || !inScope(n) || s.TrustStatic(n, sup).State == StateDeprecated {
			continue
		}
		e := a.Entries[n.ID]
		if e == nil {
			never = append(never, n.ID)
			continue
		}
		last := e.ListedAt
		for _, t := range []string{e.ShownAt, e.DeliveredAt} {
			if t > last {
				last = t
			}
		}
		rows = append(rows, row{n.ID, e, last})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].last < rows[j].last })
	var b strings.Builder
	b.WriteString("Local access statistics: a maintenance signal for this machine only. They do not change trust and nothing is expired or deleted because of them.\n")
	if a.Since == "" {
		b.WriteString("nothing recorded yet (searches, 'kb show' and rule deliveries are counted from now on)\n")
		return b.String(), nil
	}
	fmt.Fprintf(&b, "recorded since %s: %d entr(ies) retrieved here, %d never\n", a.Since, len(rows), len(never))
	show := func(ids []string) []string {
		if limit > 0 && len(ids) > limit {
			return append(ids[:limit:limit], fmt.Sprintf("... +%d more", len(ids)-limit))
		}
		return ids
	}
	if len(never) > 0 {
		b.WriteString("never retrieved on this machine (they may be used elsewhere, or simply not needed yet):\n")
		for _, id := range show(never) {
			b.WriteString("  " + id + "\n")
		}
	}
	if len(rows) > 0 {
		b.WriteString("retrieved longest ago (listed = a search result line, shown = content returned, delivered = pushed into a session):\n")
		for i, r := range rows {
			if limit > 0 && i >= limit {
				fmt.Fprintf(&b, "  ... +%d more\n", len(rows)-limit)
				break
			}
			fmt.Fprintf(&b, "  %s: last %s; listed %d, shown %d, delivered %d\n", r.id, r.last, r.e.Listed, r.e.Shown, r.e.Delivered)
		}
	}
	return b.String(), nil
}
