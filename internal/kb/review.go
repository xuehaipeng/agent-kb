package kb

import (
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/xuehaipeng/agent-kb/internal/okf"
)

// SubmitInput opens a cross-review request for a commit range.
type SubmitInput struct {
	Title       string
	Summary     string
	Base, Head  string
	Assumptions []string
	Concepts    []string
	Actor       Actor
}

// ReviewSubmit writes a review packet. Touched files come from git, and the
// reviewer must belong to a different model family than the author. The
// packet carries claims and the diff range only, not the author's reasoning.
func (s *Store) ReviewSubmit(in SubmitInput) (*Node, error) {
	if in.Actor.Zero() {
		return nil, ErrNoActor
	}
	in.Title = oneLine(in.Title, 120)
	if in.Title == "" {
		return nil, errors.New("a one-line task summary (title) is required")
	}
	if !s.InGit() {
		return nil, errors.New("reviews need to run inside the project's git repository")
	}
	project, err := s.targetProject("")
	if err != nil {
		return nil, err
	}
	head := in.Head
	if head == "" {
		head = "HEAD"
	}
	headSHA, err := Git(s.CodeRoot, "rev-parse", "--short=12", head)
	if err != nil {
		return nil, fmt.Errorf("head: %w", err)
	}
	base := in.Base
	if base == "" {
		base = s.defaultBase()
	}
	if base == "" {
		return nil, errors.New("cannot infer --base (no upstream/main branch); pass --base <ref>")
	}
	baseSHA, err := Git(s.CodeRoot, "rev-parse", "--short=12", base)
	if err != nil {
		return nil, fmt.Errorf("base: %w", err)
	}
	if baseSHA == headSHA {
		return nil, fmt.Errorf("base and head are the same commit (%s): commit your work first or pass --base", headSHA)
	}
	out, err := Git(s.CodeRoot, "diff", "--name-only", baseSHA, headSHA, "--")
	if err != nil {
		return nil, err
	}
	var files []string
	bundleRel := s.bundleRelToTop()
	for _, f := range strings.Split(out, "\n") {
		f = strings.TrimSpace(f)
		if f == "" || (bundleRel != "" && strings.HasPrefix(f, bundleRel+"/")) {
			continue
		}
		files = append(files, f)
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("no code changes between %s and %s", baseSHA, headSHA)
	}
	dirty, _ := Git(s.CodeRoot, "status", "--porcelain", "--untracked-files=no")

	unlock, err := s.Lock()
	if err != nil {
		return nil, err
	}
	defer unlock()
	s.Invalidate()
	if err := s.Load(); err != nil {
		return nil, err
	}

	concepts := cleanList(in.Concepts)
	if len(concepts) == 0 {
		// Attach the verified knowledge anchored to the touched files.
		hits, _ := s.Search(SearchQuery{Files: files, Limit: 10, Projects: projectList(project)})
		for _, h := range hits {
			concepts = append(concepts, h.Node.ID)
		}
	}
	var body strings.Builder
	body.WriteString("## Summary\n\n")
	summary, _ := Redact(strings.TrimSpace(in.Summary))
	if summary == "" {
		summary = in.Title
	}
	body.WriteString(summary + "\n")
	if as := cleanLines(in.Assumptions); len(as) > 0 {
		body.WriteString("\n## Key assumptions to check\n\n")
		for _, a := range as {
			a, _ = Redact(a)
			body.WriteString("- " + a + "\n")
		}
	}
	pathspec := ""
	if rel := s.bundleRelToTop(); rel != "" {
		pathspec = " -- . ':(exclude)" + rel + "'"
	}
	fmt.Fprintf(&body, "\n## How to review\n\n```\ngit diff %s..%s%s\n```\n", baseSHA, headSHA, pathspec)
	if dirty != "" {
		body.WriteString("\n> Note: the author's work tree had uncommitted changes at submit time; only the commit range is under review.\n")
	}
	body.WriteString("\n## Verdicts\n")

	now := s.Now().UTC()
	meta := okf.Meta{
		Type:        "Review",
		Title:       in.Title,
		Description: oneLine(summary, 160),
		Status:      ReviewPending,
		Generated:   okf.Event{By: in.Actor.String(), At: now.Format(time.RFC3339), Commit: headSHA},
		Review: &okf.Review{
			Base: baseSHA, Head: headSHA, Files: files,
			ReviewerMustDifferFrom: s.Cfg.Family(in.Actor),
			Concepts:               concepts,
		},
	}
	if ttl := s.Cfg.Types["review"].TTLDays; ttl > 0 {
		meta.StaleAfter = now.Add(time.Duration(ttl) * 24 * time.Hour).Format(time.RFC3339)
	}
	if err := s.ensureProject(project); err != nil {
		return nil, err
	}
	dir := s.Cfg.Types["review"].Dir
	local := s.uniqueLocalID(project, dir, now.Format("20060102")+"-"+Slugify(in.Title))
	n := &Node{ID: joinID(project, local), Project: project, Path: s.pathFor(project, local), TypeKey: "review", Doc: okf.New(meta, body.String())}
	if err := s.writeNode(n); err != nil {
		return nil, err
	}
	return n, s.recordChange("review-submit", n.ID, in.Actor, fmt.Sprintf("%s..%s %s", baseSHA, headSHA, in.Title))
}

func (s *Store) defaultBase() string {
	for _, ref := range []string{"@{upstream}", "origin/main", "origin/master", "main", "master"} {
		mb, err := Git(s.CodeRoot, "merge-base", "HEAD", ref)
		if err != nil || mb == "" {
			continue
		}
		head, _ := Git(s.CodeRoot, "rev-parse", "HEAD")
		if mb != head {
			return mb
		}
	}
	if _, err := Git(s.CodeRoot, "rev-parse", "HEAD~1"); err == nil {
		return "HEAD~1"
	}
	return ""
}

// codePathspecFor limits a diff in root to code, excluding a repo-mode
// bundle that lives inside the same repository.
func (s *Store) codePathspecFor(root string) []string {
	if !s.Hub && root == s.CodeRoot {
		return s.CodePathspec()
	}
	return []string{"--"}
}

func (s *Store) bundleRelToTop() string {
	if s.Hub {
		return ""
	}
	top := s.GitTop()
	if top == "" {
		return ""
	}
	rel, err := filepath.Rel(top, s.Dir)
	if err != nil || strings.HasPrefix(rel, "..") {
		return ""
	}
	return filepath.ToSlash(rel)
}

// VerdictInput is a reviewer's decision.
type VerdictInput struct {
	Ref      string
	Approve  bool
	Findings []string
	Summary  string
	Actor    Actor
}

// ReviewVerdict records a verdict; the reviewer's family must differ from the
// author's (humans always may).
func (s *Store) ReviewVerdict(in VerdictInput) (*Node, string, error) {
	if in.Actor.Zero() {
		return nil, "", ErrNoActor
	}
	findings := cleanLines(in.Findings)
	if !in.Approve && len(findings) == 0 {
		return nil, "", errors.New("requesting changes needs at least one --finding")
	}
	unlock, err := s.Lock()
	if err != nil {
		return nil, "", err
	}
	defer unlock()
	s.Invalidate()
	n, err := s.Get(in.Ref)
	if err != nil {
		return nil, "", err
	}
	if n.TypeKey != "review" {
		return nil, "", fmt.Errorf("%s is not a review; use 'kb verify' for claims", n.ID)
	}
	m := n.Meta()
	if st, _ := s.ReviewState(n); st == ReviewClosed {
		return nil, "", fmt.Errorf("%s is closed", n.ID)
	}
	// Same-family reviews are allowed (a fresh session catches a lot) but
	// labelled, since they are less independent than a cross-family review.
	authorFam := s.Cfg.FamilyOf(m.Generated.By)
	sameFamily := s.Cfg.Family(in.Actor) == authorFam && authorFam != "human"
	result := ResultApproved
	if !in.Approve {
		result = ResultChanges
	}
	head := ""
	if m.Review != nil {
		head = m.Review.Head
	}
	summary, _ := Redact(oneLine(in.Summary, 400))
	evidence := summary
	if evidence == "" {
		evidence = fmt.Sprintf("%d finding(s)", len(findings))
	}
	n.Doc.AppendVerified(okf.Event{By: in.Actor.String(), At: s.nowString(), Result: result, Commit: head, Evidence: evidence})
	var b strings.Builder
	label := ""
	if sameFamily {
		label = ", same model family as the author"
	}
	fmt.Fprintf(&b, "\n### %s by %s at %s (head %s%s)\n\n", result, in.Actor, s.Now().UTC().Format("2006-01-02 15:04Z"), head, label)
	if summary != "" {
		b.WriteString(summary + "\n\n")
	}
	for _, f := range findings {
		f, _ = Redact(f)
		b.WriteString("- " + f + "\n")
	}
	n.Doc.Body = strings.TrimRight(n.Doc.Body, "\n") + "\n" + b.String()
	state, _ := s.ReviewState(n)
	n.Doc.SetString("status", state)
	if err := s.writeNode(n); err != nil {
		return nil, "", err
	}
	return n, state, s.recordChange("review-verdict", n.ID, in.Actor, result+" "+evidence)
}

// ReviewRevise points a review at a new head after the author addressed
// findings, reopening it for another verdict.
func (s *Store) ReviewRevise(ref, head, note string, actor Actor) (*Node, error) {
	if actor.Zero() {
		return nil, ErrNoActor
	}
	if head == "" {
		head = "HEAD"
	}
	if !s.InGit() {
		return nil, errors.New("run 'kb review revise' inside the project's git repository")
	}
	sha, err := Git(s.CodeRoot, "rev-parse", "--short=12", head)
	if err != nil {
		return nil, err
	}
	unlock, err := s.Lock()
	if err != nil {
		return nil, err
	}
	defer unlock()
	s.Invalidate()
	n, err := s.Get(ref)
	if err != nil {
		return nil, err
	}
	if n.TypeKey != "review" || n.Meta().Review == nil {
		return nil, fmt.Errorf("%s is not a review", n.ID)
	}
	r := *n.Meta().Review
	if sameCommit(r.Head, sha) {
		return nil, fmt.Errorf("review already points at %s; commit the fixes first", sha)
	}
	out, err := Git(s.CodeRoot, "diff", "--name-only", r.Base, sha, "--")
	if err == nil {
		var files []string
		bundleRel := s.bundleRelToTop()
		for _, f := range strings.Split(out, "\n") {
			if f = strings.TrimSpace(f); f != "" && (bundleRel == "" || !strings.HasPrefix(f, bundleRel+"/")) {
				files = append(files, f)
			}
		}
		r.Files = files
	}
	prev := r.Head
	r.Head = sha
	n.Doc.Set("review", okf.ReviewNode(&r))
	note, _ = Redact(oneLine(note, 400))
	if note == "" {
		note = "addressed review findings"
	}
	n.Doc.Body = strings.TrimRight(n.Doc.Body, "\n") + fmt.Sprintf("\n\n### revised by %s at %s (head %s -> %s)\n\n%s\nIncremental diff: `git diff %s..%s`\n", actor, s.Now().UTC().Format("2006-01-02 15:04Z"), prev, sha, note, prev, sha)
	n.Doc.SetString("status", ReviewPending)
	if ttl := s.Cfg.Types["review"].TTLDays; ttl > 0 {
		n.Doc.SetString("stale_after", s.Now().UTC().Add(time.Duration(ttl)*24*time.Hour).Format(time.RFC3339))
	}
	if err := s.writeNode(n); err != nil {
		return nil, err
	}
	return n, s.recordChange("review-revise", n.ID, actor, prev+" -> "+sha+": "+note)
}

// ReviewClose marks a review closed (author family or human).
func (s *Store) ReviewClose(ref, note string, actor Actor) (*Node, error) {
	if actor.Zero() {
		return nil, ErrNoActor
	}
	unlock, err := s.Lock()
	if err != nil {
		return nil, err
	}
	defer unlock()
	s.Invalidate()
	n, err := s.Get(ref)
	if err != nil {
		return nil, err
	}
	if n.TypeKey != "review" {
		return nil, fmt.Errorf("%s is not a review", n.ID)
	}
	fam := s.Cfg.Family(actor)
	if fam != "human" && fam != s.Cfg.FamilyOf(n.Meta().Generated.By) {
		return nil, errors.New("only the author's family or a human can close a review")
	}
	n.Doc.SetString("status", ReviewClosed)
	if err := s.writeNode(n); err != nil {
		return nil, err
	}
	return n, s.recordChange("review-close", n.ID, actor, note)
}

// ReviewItem is a review with its derived state.
type ReviewItem struct {
	Node     *Node
	State    string
	Verdicts int
	Expired  bool
}

// ReviewFilter selects reviews for listing.
type ReviewFilter struct {
	All         bool  // include closed and approved
	ForMe       Actor // pending reviews authored by another model family
	MineBy      Actor // authored by this actor's family
	Projects    []string
	AllProjects bool
}

// Reviews lists reviews, newest first.
func (s *Store) Reviews(f ReviewFilter) ([]ReviewItem, error) {
	nodes, err := s.Nodes()
	if err != nil {
		return nil, err
	}
	inScope := s.scopeFilter(f.Projects, f.AllProjects)
	var out []ReviewItem
	for _, n := range nodes {
		if n.TypeKey != "review" || !inScope(n) {
			continue
		}
		st, v := s.ReviewState(n)
		item := ReviewItem{Node: n, State: st, Verdicts: v}
		if sa := n.Meta().StaleAfter; sa != "" {
			if ts, err := parseTime(sa); err == nil && s.Now().After(ts) {
				item.Expired = true
			}
		}
		if !f.All && (st == ReviewClosed || st == ReviewApproved) {
			continue
		}
		authorFam := s.Cfg.FamilyOf(n.Meta().Generated.By)
		if !f.ForMe.Zero() {
			fam := s.Cfg.Family(f.ForMe)
			if st != ReviewPending || (fam == authorFam && fam != "human") {
				continue
			}
		}
		if !f.MineBy.Zero() && s.Cfg.Family(f.MineBy) != authorFam {
			continue
		}
		out = append(out, item)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Node.Meta().Generated.At > out[j].Node.Meta().Generated.At })
	return out, nil
}

func cleanLines(in []string) []string {
	var out []string
	for _, l := range in {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	return out
}
