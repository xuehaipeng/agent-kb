package kb

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// File context is an opt-in retrieval aid for Claude Code: when an agent
// reads or edits a file, a hook hands it the one-line headers of the
// entries anchored to that file (their `resources` match it) and the rules
// that apply only to it. It is the hook-side counterpart of 'kb search -f'.
//
// It is off by default and stays a hint: delivered after a tool call it
// informs the work that follows, not the call that triggered it; it covers
// the Read, Edit and Write tools, not files reached through shell commands;
// and it adds context to every first touch of an anchored file, a cost that
// has to be measured (BenchFileContext) before it is recommended.

// FileContextBudget is the ceiling for one injection.
const FileContextBudget = 2000

// fileContextTitle bounds a title in a header line (characters).
const fileContextTitle = 160

// FileContext is one planned injection.
type FileContext struct {
	File    string
	Text    string   // "" when there is nothing (new) to say
	IDs     []string // entries and rules named in Text
	Omitted int      // matches not yet given that were left out to fit the budget
	Skipped string   // why nothing was looked up

	revs map[string]int // revision of each entry in IDs, for the record
}

type fileMatch struct {
	id   string
	rev  int
	line string
}

// fileMatches lists what is recorded about a file: the conditional rules
// that apply to it, then the entries anchored to it, in ranked order.
func (s *Store) fileMatches(rel string) ([]fileMatch, error) {
	var out []fileMatch
	rules, err := s.RulesForFiles([]string{rel}, nil, false)
	if err != nil {
		return nil, err
	}
	// A rule limited to the file may also be anchored to it: it is listed
	// once, as the rule it is.
	listed := map[string]bool{}
	for _, n := range rules {
		listed[n.ID] = true
		out = append(out, fileMatch{n.ID, n.Meta().Rev(), fmt.Sprintf("- rule for this file: %s [%s]", ruleText(n), n.ID)})
	}
	// Every entry anchored to the file, not the first few: what was given
	// before is taken out afterwards, so a cut here would hide the rest for
	// good once the first few have been delivered. The budget, applied to
	// what is still to give, bounds the injection.
	hits, err := s.Search(SearchQuery{Files: []string{rel}, NoDocs: true, Limit: 1 << 20})
	if err != nil {
		return nil, err
	}
	for _, h := range hits {
		if h.PathMatch == "" || listed[h.Node.ID] {
			continue
		}
		listed[h.Node.ID] = true
		m := h.Node.Meta()
		// A header is a pointer, not the entry: a title of any length is
		// cut, so one line can never use up the budget and hold back
		// itself and everything ranked after it.
		out = append(out, fileMatch{h.Node.ID, m.Rev(), fmt.Sprintf("- %s [%s] %s - %s", h.Node.ID, h.Trust.Badge(), oneLine(m.Title, fileContextTitle), oneLine(m.Description, 110))})
	}
	return out, nil
}

// relForContext maps a tool's file path to a repository-relative one, or
// says why the file is out of scope.
func (s *Store) relForContext(file string) (string, string) {
	if strings.TrimSpace(file) == "" {
		return "", "no file"
	}
	if s.CodeRoot == "" || (s.Hub && s.Project == "") {
		return "", "not inside a project repository"
	}
	rel := s.RelToCode(file)
	if filepath.IsAbs(rel) || strings.HasPrefix(rel, "..") {
		return "", "outside the repository"
	}
	return rel, ""
}

func renderFileContext(rel string, pre bool, ms []fileMatch) (text string, ids []string, omitted int) {
	if len(ms) == 0 {
		return "", nil, 0
	}
	head := fmt.Sprintf("kb: recorded about %s, for the work that follows (kb show <id> for details; entries marked stale or unknown-freshness are leads to check):\n", rel)
	if pre {
		head = fmt.Sprintf("kb: recorded about %s, before you change it (kb show <id> for details; entries marked stale or unknown-freshness are leads to check):\n", rel)
	}
	more := func(n int) string { return fmt.Sprintf("(+%d more: kb search -f %s)\n", n, rel) }
	// Lines are taken in ranked order while they fit. One that does not fit
	// is left for a later touch and the next ones are still tried, so a
	// long line never holds back the shorter ones behind it. Room is kept
	// for the "+N more" line whenever something stays behind.
	body := ""
	for i, m := range ms {
		next := body + m.line + "\n"
		tail := ""
		if rest := omitted + len(ms) - i - 1; rest > 0 {
			tail = more(rest)
		}
		if ContextLen(head+next+tail) > FileContextBudget {
			omitted++
			continue
		}
		body = next
		ids = append(ids, m.id)
	}
	if len(ids) == 0 {
		return "", nil, omitted
	}
	if omitted > 0 {
		body += more(omitted)
	}
	return head + body, ids, omitted
}

// PlanFileContext plans the injection for one file access by one agent of
// a session. Each entry is given to an agent once per revision and per
// compaction of that agent: a second touch of the same file adds nothing
// (or the next entries that did not fit before), a rewritten entry is
// given again, and after the agent compacts it starts over, since its
// summary may have dropped the headers. Another agent of the same session
// is not affected either way.
//
// Planning records nothing. The caller hands the text over and then calls
// RecordFileContext: an injection that was planned but never reached the
// agent (the hook ran out of time) must not count as given, or the agent
// would never see those entries.
func (s *Store) PlanFileContext(session, agent, file string, pre bool) FileContext {
	rel, skip := s.relForContext(file)
	fc := FileContext{File: rel, Skipped: skip}
	if skip != "" {
		return fc
	}
	if agent == "" {
		agent = "main"
	}
	ms, err := s.fileMatches(rel)
	if err != nil || len(ms) == 0 {
		return fc
	}
	given := map[string]int{}
	if a := s.LoadDelivery(session).Agents[agent]; a != nil {
		given = a.FileContext
	}
	var fresh []fileMatch
	for _, m := range ms {
		if rev, seen := given[m.id]; !seen || rev != m.rev {
			fresh = append(fresh, m)
		}
	}
	fc.Text, fc.IDs, fc.Omitted = renderFileContext(rel, pre, fresh)
	in := map[string]bool{}
	for _, id := range fc.IDs {
		in[id] = true
	}
	fc.revs = map[string]int{}
	for _, m := range fresh {
		if in[m.id] {
			fc.revs[m.id] = m.rev
		}
	}
	return fc
}

// RecordFileContext notes that a planned injection was handed to the agent.
func (s *Store) RecordFileContext(session, agent, agentType string, fc FileContext) {
	if len(fc.IDs) == 0 || session == "" {
		return
	}
	if agent == "" {
		agent = "main"
	}
	if unlock, err := s.lockFile("sessions.lock"); err == nil {
		st := s.LoadDelivery(session)
		a := st.Agents[agent]
		if a == nil {
			a = &AgentDelivery{Type: agentType}
			st.Agents[agent] = a
		}
		if a.FileContext == nil {
			a.FileContext = map[string]int{}
		}
		for id, rev := range fc.revs {
			a.FileContext[id] = rev
		}
		st.Project = s.Project
		if data, err := json.MarshalIndent(st, "", "  "); err == nil {
			_, _ = WriteFileIfChanged(s.deliveryPath(session), data)
		}
		unlock()
	}
	s.NoteAccess(AccessListed, fc.IDs)
}

// FileContextFor plans an injection and records it as given.
func (s *Store) FileContextFor(session, agent, agentType, file string, pre bool) FileContext {
	fc := s.PlanFileContext(session, agent, file, pre)
	s.RecordFileContext(session, agent, agentType, fc)
	return fc
}

// FileContextCase is one judged benchmark case: a file an agent touches and
// the entries a person judged relevant to working on it.
type FileContextCase struct {
	File     string   `json:"file"`
	Relevant []string `json:"relevant"`
}

// FileContextBench is the measured outcome over a set of cases.
type FileContextBench struct {
	Cases        int      `json:"cases"`
	Injected     int      `json:"injections"`   // cases where something would be injected
	Returned     int      `json:"returned"`     // entries injected in total
	Hits         int      `json:"relevant_hit"` // injected entries judged relevant
	Relevant     int      `json:"relevant"`     // judged relevant in total
	Precision    float64  `json:"precision"`
	Recall       float64  `json:"recall"`
	MeanUnits    float64  `json:"mean_context_units"` // per injection
	MaxUnits     int      `json:"max_context_units"`
	MedianMillis float64  `json:"median_lookup_ms"`
	MaxMillis    float64  `json:"max_lookup_ms"`
	Lines        []string `json:"-"`
}

// BenchFileContext measures what file context would inject for judged
// cases: precision and recall against the judgments, the context it costs,
// and how long the lookup takes. It injects nothing and records nothing.
func (s *Store) BenchFileContext(cases []FileContextCase) (FileContextBench, error) {
	b := FileContextBench{Cases: len(cases)}
	var millis []float64
	units := 0
	for _, c := range cases {
		rel, skip := s.relForContext(c.File)
		if skip != "" {
			rel = c.File
		}
		start := time.Now()
		ms, err := s.fileMatches(rel)
		if err != nil {
			return b, err
		}
		text, ids, _ := renderFileContext(rel, false, ms)
		ms1 := float64(time.Since(start).Microseconds()) / 1000
		millis = append(millis, ms1)
		want := map[string]bool{}
		for _, id := range c.Relevant {
			if n, err := s.Get(id); err == nil {
				want[n.ID] = true
			} else {
				want[id] = true
			}
		}
		hit := 0
		for _, id := range ids {
			if want[id] {
				hit++
			}
		}
		u := ContextLen(text)
		if len(ids) > 0 {
			b.Injected++
			units += u
			if u > b.MaxUnits {
				b.MaxUnits = u
			}
		}
		b.Returned += len(ids)
		b.Hits += hit
		b.Relevant += len(want)
		b.Lines = append(b.Lines, fmt.Sprintf("%s: injected %d, relevant %d of %d, %d units, %.1f ms", rel, len(ids), hit, len(want), u, ms1))
	}
	if b.Returned > 0 {
		b.Precision = float64(b.Hits) / float64(b.Returned)
	}
	if b.Relevant > 0 {
		b.Recall = float64(b.Hits) / float64(b.Relevant)
	}
	if b.Injected > 0 {
		b.MeanUnits = float64(units) / float64(b.Injected)
	}
	if len(millis) > 0 {
		sort.Float64s(millis)
		b.MedianMillis, b.MaxMillis = millis[len(millis)/2], millis[len(millis)-1]
	}
	return b, nil
}
