package kb

import (
	"math"
	"sort"
	"strings"
	"unicode"
)

// SearchQuery describes a knowledge lookup.
type SearchQuery struct {
	Text              string
	Projects          []string // hub: restrict to these projects (plus matching global entries)
	AllProjects       bool     // hub: search every project
	Files             []string // repo-relative paths the caller will touch
	Types             []string
	IncludeDeprecated bool
	TrustedOnly       bool // only corroborated / reviewed
	Limit             int
}

// Hit is one ranked result.
type Hit struct {
	Node      *Node
	Trust     Trust
	Score     float64
	PathMatch string // the queried file that matched a resource glob
}

// Search ranks nodes with BM25 over title/description/tags/resources/body,
// boosts nodes whose resource globs match the queried files, and weights by
// trust. With no text, it returns everything anchored to the given files.
// The whole bundle is scanned in-process; at a few hundred files this takes
// milliseconds and can never disagree with what is on disk.
func (s *Store) Search(q SearchQuery) ([]Hit, error) {
	nodes, err := s.Nodes()
	if err != nil {
		return nil, err
	}
	if q.Limit <= 0 {
		q.Limit = s.Cfg.Search.Limit
	}
	types := map[string]bool{}
	for _, t := range q.Types {
		if k, ok := s.Cfg.TypeKey(t); ok {
			types[k] = true
		}
	}
	files := make([]string, 0, len(q.Files))
	for _, f := range q.Files {
		if r := s.RelToCode(f); r != "" {
			files = append(files, r)
		}
	}

	inScope := s.scopeFilter(q.Projects, q.AllProjects)
	sup := s.supersededIndex()
	type doc struct {
		n    *Node
		tf   map[string]float64
		size float64
	}
	var docs []doc
	df := map[string]int{}
	var total float64
	for _, n := range nodes {
		if !inScope(n) {
			continue
		}
		if len(types) > 0 && !types[n.TypeKey] {
			continue
		}
		if len(types) == 0 && n.TypeKey == "review" {
			continue // reviews are listed with 'kb review list'
		}
		if !q.IncludeDeprecated && s.TrustStatic(n, sup).State == StateDeprecated {
			continue
		}
		tf := map[string]float64{}
		size := 0.0
		addField := func(text string, w float64) {
			for _, t := range Tokenize(text) {
				tf[t] += w
				size += w
			}
		}
		m := n.Meta()
		addField(m.Title, 3)
		addField(m.Description, 2)
		addField(strings.Join(m.Tags, " "), 2)
		addField(strings.Join(m.Resources, " "), 1)
		addField(n.ID, 1)
		addField(n.Doc.Body, 1)
		for t := range tf {
			df[t]++
		}
		total += size
		docs = append(docs, doc{n, tf, size})
	}
	if len(docs) == 0 {
		return nil, nil
	}
	avg := total / float64(len(docs))
	terms := uniqueTokens(q.Text)
	const k1, b = 1.2, 0.75
	N := float64(len(docs))

	var hits []Hit
	for _, d := range docs {
		score := 0.0
		for _, t := range terms {
			f := d.tf[t]
			if f == 0 {
				continue
			}
			idf := math.Log(1 + (N-float64(df[t])+0.5)/(float64(df[t])+0.5))
			score += idf * f * (k1 + 1) / (f + k1*(1-b+b*d.size/avg))
		}
		match := ""
		for _, f := range files {
			if MatchAny(d.n.Meta().Resources, f) {
				match = f
				break
			}
		}
		if score == 0 && match == "" {
			continue
		}
		if len(terms) > 0 && score == 0 && match != "" {
			score = 0.5 // anchored to the file but no keyword overlap
		}
		if match != "" {
			score += 6
		}
		tr := s.trust(d.n, sup, true)
		if q.TrustedOnly && !tr.Trusted() {
			continue
		}
		score *= trustWeight(tr)
		hits = append(hits, Hit{Node: d.n, Trust: tr, Score: score, PathMatch: match})
	}
	sort.SliceStable(hits, func(i, j int) bool {
		if hits[i].Score != hits[j].Score {
			return hits[i].Score > hits[j].Score
		}
		return hits[i].Node.ID < hits[j].Node.ID
	})
	s.lastTruncated = 0
	if len(hits) > q.Limit {
		s.lastTruncated = len(hits) - q.Limit
		hits = hits[:q.Limit]
	}
	return hits, nil
}

// Truncated reports how many matches the last Search left out.
func (s *Store) Truncated() int { return s.lastTruncated }

// scopeFilter returns the project filter for searches and listings. In a
// hub, the default scope is the current project plus global entries that
// apply to it; outside any project it is every project.
func (s *Store) scopeFilter(projects []string, all bool) func(*Node) bool {
	if !s.Hub || all {
		return func(*Node) bool { return true }
	}
	want := map[string]bool{}
	for _, p := range projects {
		want[p] = true
	}
	if len(want) == 0 {
		if s.Project == "" {
			return func(*Node) bool { return true }
		}
		want[s.Project] = true
	}
	return func(n *Node) bool {
		if want[n.Project] {
			return true
		}
		if n.Project != GlobalProject || want[GlobalProject] {
			return want[n.Project]
		}
		ps := n.Meta().Projects
		if len(ps) == 0 {
			return true
		}
		for _, p := range ps {
			if want[p] {
				return true
			}
		}
		return false
	}
}

func trustWeight(t Trust) float64 {
	w := 1.0
	switch t.State {
	case StateReviewed:
		w = 1.3
	case StateCrossChecked:
		w = 1.25
	case StateVerified:
		w = 1.15
	case StateUnverified:
		w = 0.85
	case StateDisputed:
		w = 0.6
	case StateDeprecated:
		w = 0.3
	}
	if t.Stale {
		w *= 0.8
	}
	return w
}

var stopwords = map[string]bool{
	"a": true, "an": true, "the": true, "and": true, "or": true, "of": true, "to": true, "in": true,
	"on": true, "for": true, "is": true, "are": true, "be": true, "with": true, "when": true, "it": true,
	"this": true, "that": true, "by": true, "as": true, "at": true, "from": true, "how": true, "what": true,
	"we": true, "do": true, "does": true, "not": true, "if": true, "into": true, "use": true,
}

// Tokenize lowercases, splits on non-alphanumerics, drops stopwords and
// applies light suffix stripping. Han/Kana/Hangul text yields unigrams and
// bigrams so CJK queries work without a dictionary.
func Tokenize(s string) []string {
	var out []string
	var word []rune
	var cjk []rune
	flushWord := func() {
		if len(word) > 0 {
			w := string(word)
			if !stopwords[w] && (len(word) > 1 || unicode.IsDigit(word[0])) {
				out = append(out, stem(w))
			}
			word = word[:0]
		}
	}
	flushCJK := func() {
		for i, r := range cjk {
			out = append(out, string(r))
			if i+1 < len(cjk) {
				out = append(out, string(cjk[i:i+2]))
			}
		}
		cjk = cjk[:0]
	}
	for _, r := range strings.ToLower(s) {
		switch {
		case isCJK(r):
			flushWord()
			cjk = append(cjk, r)
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			flushCJK()
			word = append(word, r)
		default:
			flushWord()
			flushCJK()
		}
	}
	flushWord()
	flushCJK()
	return out
}

func isCJK(r rune) bool {
	return unicode.Is(unicode.Han, r) || unicode.Is(unicode.Hiragana, r) || unicode.Is(unicode.Katakana, r) || unicode.Is(unicode.Hangul, r)
}

func stem(w string) string {
	if len([]rune(w)) <= 3 {
		return w
	}
	for _, suf := range []string{"ations", "ation", "ings", "ing", "edly", "ies", "ied", "ed", "ly", "s"} {
		if suf == "s" && strings.HasSuffix(w, "ss") {
			break
		}
		if strings.HasSuffix(w, suf) && len(w)-len(suf) >= 3 {
			w = w[:len(w)-len(suf)]
			if suf == "ies" || suf == "ied" {
				w += "y"
			}
			break
		}
	}
	// Drop a final "e" so cache/caches/cached and fix/fixes meet.
	if strings.HasSuffix(w, "e") && len(w) > 3 {
		w = w[:len(w)-1]
	}
	return w
}

func uniqueTokens(s string) []string {
	seen := map[string]bool{}
	var out []string
	for _, t := range Tokenize(s) {
		if !seen[t] {
			seen[t] = true
			out = append(out, t)
		}
	}
	return out
}
