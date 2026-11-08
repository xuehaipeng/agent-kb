package kb

import (
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// The repository's own docs (README, docs/, ADRs, AGENTS.md ...) are the
// source of truth for what they cover. kb never copies them: search reads
// their sections live from the checkout, and kb add refuses entries that
// merely restate a section. The knowledge base holds what the docs don't.

const (
	maxDocFiles     = 400
	maxDocFileBytes = 256 << 10
	maxSectionChars = 4000

	// Weighted share of an entry's claim terms found in one doc section.
	// Calibrated on real entries: new knowledge scored 0.17-0.27, partly
	// documented claims 0.41-0.56, restated doc sections 0.65-1.0.
	DocBlockCoverage = 0.65 // kb add refuses (--force overrides); lint warns
	DocWarnCoverage  = 0.45 // kb add warns; extraction drops the claim
)

// DocSection is a heading-delimited part of a Markdown file in a checkout.
type DocSection struct {
	Project string // "" in repo mode
	Path    string // repo-relative file path
	Line    int    // 1-based line of the heading (1 for text before any heading)
	Heading string
	Trail   string // parent headings, outermost first, joined with " › "
	Text    string
	tf      map[string]float64
	size    float64
}

// Ref renders "docs/ARCHITECTURE.md:125 (§ 2.5 Index ...)".
func (d *DocSection) Ref() string {
	p := d.Path
	if d.Project != "" {
		p = d.Project + ":" + p
	}
	if d.Heading == "" {
		return p + ":" + itoa(d.Line)
	}
	return p + ":" + itoa(d.Line) + " (§ " + d.Heading + ")"
}

type docCorpus struct {
	sections []*DocSection
	df       map[string]int
	avg      float64
}

// docsIn loads (once per store) the Markdown docs tracked in a checkout.
func (s *Store) docsIn(root, project string) *docCorpus {
	if s.docCache == nil {
		s.docCache = map[string]*docCorpus{}
	}
	if c, ok := s.docCache[root]; ok {
		return c
	}
	c := &docCorpus{df: map[string]int{}}
	s.docCache[root] = c
	if root == "" || !s.gitFor(root).ok {
		return c
	}
	exclude := ""
	if !s.Hub && root == s.CodeRoot {
		exclude = s.bundleRelToTop()
	}
	out, err := Git(root, "ls-files", "--", "*.md", "*.mdx", "*.markdown")
	if err != nil {
		return c
	}
	n := 0
	for _, rel := range strings.Split(out, "\n") {
		rel = strings.TrimSpace(rel)
		if rel == "" || !isDocPath(rel, exclude) {
			continue
		}
		if n++; n > maxDocFiles {
			break
		}
		p := filepath.Join(root, filepath.FromSlash(rel))
		st, err := os.Stat(p)
		if err != nil || st.Size() > maxDocFileBytes {
			continue
		}
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		for _, sec := range parseSections(rel, string(data)) {
			sec.Project = project
			sec.tf = map[string]float64{}
			add := func(text string, w float64) {
				for _, t := range Tokenize(text) {
					sec.tf[t] += w
					sec.size += w
				}
			}
			add(sec.Heading, 3)
			add(sec.Trail, 1)
			add(sec.Text, 1)
			if sec.size == 0 {
				continue
			}
			for t := range sec.tf {
				c.df[t]++
			}
			c.sections = append(c.sections, sec)
		}
	}
	total := 0.0
	for _, sec := range c.sections {
		total += sec.size
	}
	if len(c.sections) > 0 {
		c.avg = total / float64(len(c.sections))
	}
	return c
}

// isDocPath keeps prose docs and drops dependencies, generated or hidden
// trees, the knowledge bundle itself, and boilerplate files.
func isDocPath(rel, exclude string) bool {
	if exclude != "" && (rel == exclude || strings.HasPrefix(rel, exclude+"/")) {
		return false
	}
	for _, seg := range strings.Split(rel, "/") {
		switch {
		case strings.HasPrefix(seg, ".") && seg != ".":
			return false
		case seg == "node_modules", seg == "vendor", seg == "third_party", seg == "testdata":
			return false
		}
	}
	base := strings.ToUpper(filepath.Base(rel))
	for _, skip := range []string{"CHANGELOG", "LICENSE", "CODE_OF_CONDUCT", "SECURITY", "CONTRIBUTORS", "NOTICE"} {
		if strings.HasPrefix(base, skip) {
			return false
		}
	}
	return true
}

// parseSections splits Markdown by ATX headings, ignoring fenced code.
func parseSections(rel, content string) []*DocSection {
	var out []*DocSection
	var trail []string // heading text by level-1 index
	cur := &DocSection{Path: rel, Line: 1}
	var body strings.Builder
	flush := func() {
		cur.Text = strings.TrimSpace(body.String())
		if len(cur.Text) > maxSectionChars {
			cur.Text = cur.Text[:maxSectionChars]
		}
		if cur.Heading != "" || cur.Text != "" {
			out = append(out, cur)
		}
		body.Reset()
	}
	fence := ""
	for i, line := range strings.Split(strings.ReplaceAll(content, "\r\n", "\n"), "\n") {
		trimmed := strings.TrimSpace(line)
		if fence != "" {
			if strings.HasPrefix(trimmed, fence) {
				fence = ""
			}
			body.WriteString(line + "\n")
			continue
		}
		if strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~") {
			fence = trimmed[:3]
			body.WriteString(line + "\n")
			continue
		}
		level := 0
		for level < len(line) && level < 7 && line[level] == '#' {
			level++
		}
		if level >= 1 && level <= 6 && len(line) > level && line[level] == ' ' {
			flush()
			heading := strings.TrimSpace(strings.TrimRight(strings.TrimSpace(line[level:]), "#"))
			if len(trail) >= level {
				trail = trail[:level-1]
			}
			for len(trail) < level-1 {
				trail = append(trail, "")
			}
			var parents []string
			for _, h := range trail {
				if h != "" {
					parents = append(parents, h)
				}
			}
			trail = append(trail, heading)
			cur = &DocSection{Path: rel, Line: i + 1, Heading: heading, Trail: strings.Join(parents, " › ")}
			continue
		}
		body.WriteString(line + "\n")
	}
	flush()
	return out
}

// docRoots lists the checkouts whose docs are in a search scope.
func (s *Store) docRoots(projects []string, all bool) map[string]string {
	out := map[string]string{} // project -> checkout
	if !s.Hub {
		if s.CodeRoot != "" {
			out[""] = s.CodeRoot
		}
		return out
	}
	want := projects
	if all || (len(want) == 0 && s.Project == "") {
		want = nil
		for id := range s.ProjectFiles() {
			want = append(want, id)
		}
	} else if len(want) == 0 {
		want = []string{s.Project}
	}
	for _, p := range want {
		if p == GlobalProject {
			continue
		}
		if co := s.CheckoutFor(p); co != "" {
			out[p] = co
		}
	}
	return out
}

// DocHit is a matching doc section.
type DocHit struct {
	Section *DocSection
	Score   float64
}

// SearchDocs ranks the repo docs' sections for a text query (BM25).
func (s *Store) SearchDocs(text string, projects []string, all bool, limit int) []DocHit {
	terms := uniqueTokens(text)
	if len(terms) == 0 {
		return nil
	}
	if limit <= 0 {
		limit = 3
	}
	var hits []DocHit
	for project, root := range s.docRoots(projects, all) {
		c := s.docsIn(root, project)
		N := float64(len(c.sections))
		const k1, b = 1.2, 0.75
		for _, sec := range c.sections {
			score := 0.0
			matched := 0
			for _, t := range terms {
				f := sec.tf[t]
				if f == 0 {
					continue
				}
				matched++
				idf := math.Log(1 + (N-float64(c.df[t])+0.5)/(float64(c.df[t])+0.5))
				score += idf * f * (k1 + 1) / (f + k1*(1-b+b*sec.size/c.avg))
			}
			// Require most query terms, so doc hits stay relevant.
			if score > 0 && float64(matched) >= math.Ceil(float64(len(terms))*0.5) {
				hits = append(hits, DocHit{sec, score})
			}
		}
	}
	sort.SliceStable(hits, func(i, j int) bool {
		if hits[i].Score != hits[j].Score {
			return hits[i].Score > hits[j].Score
		}
		return hits[i].Section.Ref() < hits[j].Section.Ref()
	})
	if len(hits) > limit {
		hits = hits[:limit]
	}
	return hits
}

// DocMatch is a doc section that covers (part of) a claim.
type DocMatch struct {
	Section  *DocSection
	Coverage float64 // IDF-weighted share of the claim's terms found in the section
}

// DocCoverage measures how much of a claim (title + description) a project's
// docs already state, best sections first. Rare terms weigh more, so a claim
// with specifics the docs never mention scores low.
func (s *Store) DocCoverage(project, claim string) []DocMatch {
	return s.docCoverage(project, claim, DocWarnCoverage)
}

func (s *Store) docCoverage(project, claim string, floor float64) []DocMatch {
	root := s.CodeRoot
	if s.Hub {
		if project == GlobalProject {
			return nil
		}
		root = s.CheckoutFor(project)
	}
	c := s.docsIn(root, project)
	terms := uniqueTokens(claim)
	if len(terms) < 3 || len(c.sections) == 0 {
		return nil
	}
	N := float64(len(c.sections))
	weight := map[string]float64{}
	total := 0.0
	for _, t := range terms {
		w := math.Log(1 + N/(float64(c.df[t])+0.5))
		weight[t] = w
		total += w
	}
	var out []DocMatch
	for _, sec := range c.sections {
		got := 0.0
		for _, t := range terms {
			if sec.tf[t] > 0 {
				got += weight[t]
			}
		}
		if cov := got / total; cov >= floor {
			out = append(out, DocMatch{sec, cov})
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Coverage > out[j].Coverage })
	if len(out) > 3 {
		out = out[:3]
	}
	return out
}

// DocHeadings lists doc section headings (for the extraction prompt).
func (s *Store) DocHeadings(max int) []string {
	c := s.docsIn(s.CheckoutFor(s.Project), s.Project)
	var out []string
	for _, sec := range c.sections {
		if sec.Heading == "" {
			continue
		}
		h := sec.Path + " § " + sec.Heading
		out = append(out, h)
		if len(out) >= max {
			break
		}
	}
	return out
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

// Snippet is the section's first prose sentence, skipping code blocks,
// tables and Markdown decoration.
func (d *DocSection) Snippet(max int) string {
	fence := false
	for _, line := range strings.Split(d.Text, "\n") {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "```") || strings.HasPrefix(t, "~~~") {
			fence = !fence
			continue
		}
		if fence || t == "" || strings.HasPrefix(t, "|") || strings.HasPrefix(t, "---") || strings.HasPrefix(t, "<") {
			continue
		}
		t = strings.TrimSpace(strings.TrimLeft(t, "-*>#0123456789. "))
		t = strings.NewReplacer("**", "", "`", "", "__", "").Replace(t)
		if t == "" {
			continue
		}
		if i := strings.Index(t, ". "); i > 20 {
			t = t[:i+1]
		}
		return oneLine(t, max)
	}
	return ""
}
