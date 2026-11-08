package kb

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"
	"unicode"

	"github.com/xuehaipeng/agent-kb/internal/okf"
)

// AddInput is a new knowledge entry.
type AddInput struct {
	Type        string
	Title       string
	Description string
	Body        string
	Tags        []string
	Resources   []string
	Sources     []okf.Source
	Supersedes  []string
	Projects    []string // global entries: the projects they apply to
	Project     string   // hub: target project ("" = current, "global" = shared)
	Evidence    string   // how the author knows; makes the entry verified
	Actor       Actor
	Force       bool // skip the near-duplicate check
}

// AddResult reports what Add did.
type AddResult struct {
	Node     *Node
	Trust    Trust
	Related  []Similar // similar existing entries worth updating instead
	Redacted int
}

// Similar is a near-match found by the duplicate check.
type Similar struct {
	ID    string
	Title string
	Score float64
}

// DuplicateError is returned when a proposal repeats an existing entry.
type DuplicateError struct{ Match Similar }

func (e *DuplicateError) Error() string {
	return fmt.Sprintf("looks like a duplicate of %s (%q, similarity %.2f): improve it with 'kb update %s' or confirm it with 'kb verify', or pass --force", e.Match.ID, e.Match.Title, e.Match.Score, e.Match.ID)
}

// targetProject picks the hub project an entry is written to.
func (s *Store) targetProject(p string) (string, error) {
	if !s.Hub {
		return "", nil
	}
	switch p {
	case "":
		if s.Project == "" {
			return "", errors.New("not inside a project repository: pass --project <id> or --global")
		}
		return s.Project, nil
	case "global", "--global":
		return GlobalProject, nil
	}
	return p, validProjectID(p)
}

// Add writes a new entry. With evidence it is verified by its author at
// once; without, it is an unverified hypothesis others can check.
func (s *Store) Add(in AddInput) (*AddResult, error) {
	if in.Actor.Zero() {
		return nil, ErrNoActor
	}
	tk, ok := s.Cfg.TypeKey(in.Type)
	if !ok {
		return nil, fmt.Errorf("unknown type %q (known: %s)", in.Type, strings.Join(s.Cfg.TypeNames(), ", "))
	}
	if tk == "review" {
		return nil, errors.New("use 'kb review submit' to create reviews")
	}
	in.Title = oneLine(in.Title, 0)
	if in.Title == "" {
		return nil, errors.New("title is required")
	}
	in.Description = oneLine(in.Description, 0)
	if in.Description == "" {
		return nil, errors.New("description (one line) is required")
	}
	project, err := s.targetProject(in.Project)
	if err != nil {
		return nil, err
	}
	unlock, err := s.Lock()
	if err != nil {
		return nil, err
	}
	defer unlock()
	s.Invalidate()
	if err := s.Load(); err != nil {
		return nil, err
	}

	res := &AddResult{}
	var redacted int
	in.Title, redacted = Redact(in.Title)
	res.Redacted += redacted
	in.Description, redacted = Redact(in.Description)
	res.Redacted += redacted
	in.Body, redacted = Redact(in.Body)
	res.Redacted += redacted
	in.Evidence, redacted = Redact(oneLine(in.Evidence, 600))
	res.Redacted += redacted

	var supIDs []string
	for _, ref := range in.Supersedes {
		target, err := s.Get(ref)
		if err != nil {
			return nil, fmt.Errorf("supersedes: %w", err)
		}
		if target.TypeKey != tk {
			return nil, fmt.Errorf("supersedes: %s is a %s, not a %s", target.ID, target.TypeKey, tk)
		}
		ref := target.ID
		if target.Project == project {
			ref = target.LocalID()
		}
		supIDs = append(supIDs, ref)
	}

	if len(supIDs) == 0 {
		related := s.similar(in.Title+" "+in.Description, tk, project)
		if len(related) > 0 && related[0].Score >= 0.85 && !in.Force {
			return nil, &DuplicateError{Match: related[0]}
		}
		for _, r := range related {
			if r.Score >= 0.4 {
				res.Related = append(res.Related, r)
			}
		}
	}

	now := s.Now().UTC()
	tc := s.Cfg.Types[tk]
	head := s.headFor(project)
	meta := okf.Meta{
		Type:        DisplayType(tk),
		Title:       in.Title,
		Description: in.Description,
		Tags:        cleanList(in.Tags),
		Status:      "draft",
		Resources:   cleanList(in.Resources),
		Supersedes:  supIDs,
		Generated:   okf.Event{By: in.Actor.String(), At: now.Format(time.RFC3339), Commit: head},
		Sources:     in.Sources,
	}
	if project == GlobalProject {
		meta.Projects = cleanList(in.Projects)
	}
	if in.Evidence != "" {
		meta.Status = "stable"
		meta.Verified = []okf.Event{{By: in.Actor.String(), At: now.Format(time.RFC3339), Result: ResultConfirmed, Rev: 1, Commit: head, Evidence: in.Evidence}}
	}
	if tc.TTLDays > 0 {
		meta.StaleAfter = now.Add(time.Duration(tc.TTLDays) * 24 * time.Hour).Format(time.RFC3339)
	}
	body := strings.TrimSpace(in.Body)
	if body == "" {
		body = in.Description
	}
	if err := s.ensureProject(project); err != nil {
		return nil, err
	}
	local := s.uniqueLocalID(project, tc.Dir, Slugify(in.Title))
	n := &Node{ID: joinID(project, local), Project: project, Path: s.pathFor(project, local), TypeKey: tk, Doc: okf.New(meta, body+"\n")}
	if err := s.writeNode(n); err != nil {
		return nil, err
	}
	detail := in.Title
	if len(supIDs) > 0 {
		detail += " (supersedes " + strings.Join(supIDs, ", ") + ")"
	}
	if err := s.recordChange("add", n.ID, in.Actor, detail); err != nil {
		return nil, err
	}
	res.Node = n
	res.Trust = s.Trust(n)
	return res, nil
}

func joinID(project, local string) string {
	if project == "" {
		return local
	}
	return project + "/" + local
}

// VerifyInput records a check of an entry against the code.
type VerifyInput struct {
	Ref      string
	Result   string // confirmed | disputed
	Evidence string
	Actor    Actor
}

// Verify appends a verification event for the current revision. Any agent
// may verify, including the author (re-confirming a stale entry refreshes
// it); a different model family upgrades it to cross-checked.
func (s *Store) Verify(in VerifyInput) (*Node, Trust, error) {
	if in.Actor.Zero() {
		return nil, Trust{}, ErrNoActor
	}
	if in.Result != ResultConfirmed && in.Result != ResultDisputed {
		return nil, Trust{}, fmt.Errorf("result must be %q or %q", ResultConfirmed, ResultDisputed)
	}
	in.Evidence = oneLine(in.Evidence, 600)
	if in.Evidence == "" {
		return nil, Trust{}, errors.New("evidence is required: say what you checked (file, test, command)")
	}
	in.Evidence, _ = Redact(in.Evidence)
	unlock, err := s.Lock()
	if err != nil {
		return nil, Trust{}, err
	}
	defer unlock()
	s.Invalidate()
	n, err := s.Get(in.Ref)
	if err != nil {
		return nil, Trust{}, err
	}
	if n.TypeKey == "review" {
		return nil, Trust{}, errors.New("reviews take verdicts: use 'kb review verdict'")
	}
	before := s.Trust(n)
	if before.State == StateDeprecated {
		return nil, Trust{}, fmt.Errorf("%s is deprecated%s; verify its replacement instead", n.ID, supersededNote(before))
	}
	rev := n.Meta().Rev()
	head := s.headFor(n.Project)
	// Skip exact repeats (same actor, revision, result and commit).
	for _, e := range n.Meta().Verified {
		if e.By == in.Actor.String() && okf.EventRev(e) == rev && e.Result == in.Result && e.Commit == head && !before.Stale {
			return n, before, nil
		}
	}
	n.Doc.AppendVerified(okf.Event{By: in.Actor.String(), At: s.nowString(), Result: in.Result, Rev: rev, Commit: head, Evidence: in.Evidence})
	if in.Result == ResultConfirmed {
		s.refreshStaleAfter(n)
	}
	after := s.Trust(n)
	s.applyStatus(n, after)
	if err := s.writeNode(n); err != nil {
		return nil, Trust{}, err
	}
	if err := s.recordChange("verify", n.ID, in.Actor, in.Result+": "+in.Evidence); err != nil {
		return nil, Trust{}, err
	}
	return n, after, nil
}

// UpdateInput changes an entry's content in place.
type UpdateInput struct {
	Ref          string
	Title        *string
	Description  *string
	Body         *string
	Resources    []string // replaces when SetResources
	SetResources bool
	Tags         []string
	SetTags      bool
	Reason       string // why it changed (required)
	Evidence     string // optional: how the updater knows; verifies the new revision
	Actor        Actor
}

// Update rewrites an entry's content as a new revision. Knowledge evolves:
// an agent that finds an entry stale or wrong fixes it instead of piling up
// near-duplicates. Earlier revisions stay in git history, earlier
// verifications stay in the trail (scoped to their revision), and the new
// revision starts unverified unless the updater supplies evidence.
// Immutable types (decisions) change only by superseding.
func (s *Store) Update(in UpdateInput) (*Node, Trust, error) {
	if in.Actor.Zero() {
		return nil, Trust{}, ErrNoActor
	}
	in.Reason = oneLine(in.Reason, 300)
	if in.Reason == "" {
		return nil, Trust{}, errors.New("say why it changed: -m \"<reason>\"")
	}
	if in.Title == nil && in.Description == nil && in.Body == nil && !in.SetResources && !in.SetTags {
		return nil, Trust{}, errors.New("nothing to update: pass -t/--title, -d, -b, -r or --tags")
	}
	unlock, err := s.Lock()
	if err != nil {
		return nil, Trust{}, err
	}
	defer unlock()
	s.Invalidate()
	n, err := s.Get(in.Ref)
	if err != nil {
		return nil, Trust{}, err
	}
	if n.TypeKey == "review" {
		return nil, Trust{}, errors.New("reviews change with 'kb review revise'")
	}
	if s.Cfg.Types[n.TypeKey].Immutable {
		return nil, Trust{}, fmt.Errorf("%s entries are immutable so their history stays readable: record the new %s with 'kb add %s ... --supersedes %s'", n.TypeKey, n.TypeKey, n.TypeKey, n.ID)
	}
	if s.Trust(n).State == StateDeprecated {
		return nil, Trust{}, fmt.Errorf("%s is deprecated; add a new entry instead", n.ID)
	}
	var red int
	if in.Title != nil {
		t, r := Redact(oneLine(*in.Title, 0))
		red += r
		if t == "" {
			return nil, Trust{}, errors.New("title cannot be empty")
		}
		n.Doc.SetString("title", t)
	}
	if in.Description != nil {
		d, r := Redact(oneLine(*in.Description, 0))
		red += r
		if d == "" {
			return nil, Trust{}, errors.New("description cannot be empty")
		}
		n.Doc.SetString("description", d)
	}
	if in.Body != nil {
		b, r := Redact(strings.TrimSpace(*in.Body))
		red += r
		if b == "" {
			return nil, Trust{}, errors.New("body cannot be empty")
		}
		n.Doc.Body = b + "\n"
	}
	if in.SetResources {
		n.Doc.Set("resources", okf.ListNode(cleanList(in.Resources)))
	}
	if in.SetTags {
		n.Doc.Set("tags", okf.ListNode(cleanList(in.Tags)))
	}
	rev := n.Meta().Rev() + 1
	head := s.headFor(n.Project)
	now := s.nowString()
	n.Doc.Set("revision", okf.IntNode(rev))
	n.Doc.Set("updated", okf.EventNode(okf.Event{By: in.Actor.String(), At: now, Rev: rev, Commit: head, Evidence: in.Reason}, true))
	evidence, _ := Redact(oneLine(in.Evidence, 600))
	if evidence != "" {
		n.Doc.AppendVerified(okf.Event{By: in.Actor.String(), At: now, Result: ResultConfirmed, Rev: rev, Commit: head, Evidence: evidence})
	}
	s.refreshStaleAfter(n)
	t := s.Trust(n)
	s.applyStatus(n, t)
	if err := s.writeNode(n); err != nil {
		return nil, Trust{}, err
	}
	detail := fmt.Sprintf("rev %d: %s", rev, in.Reason)
	if red > 0 {
		detail += fmt.Sprintf(" (redacted %d)", red)
	}
	if err := s.recordChange("update", n.ID, in.Actor, detail); err != nil {
		return nil, Trust{}, err
	}
	return n, t, nil
}

func (s *Store) refreshStaleAfter(n *Node) {
	if ttl := s.Cfg.Types[n.TypeKey].TTLDays; ttl > 0 {
		n.Doc.SetString("stale_after", s.Now().UTC().Add(time.Duration(ttl)*24*time.Hour).Format(time.RFC3339))
	}
}

// Resolve is a human decision on an entry: keep (confirm) or deprecate.
func (s *Store) Resolve(ref string, keep bool, note string, actor Actor) (*Node, Trust, error) {
	if !actor.IsHuman() {
		return nil, Trust{}, errors.New("resolve records a human decision: pass --as human:<name> (agents use kb verify, kb update or kb deprecate)")
	}
	unlock, err := s.Lock()
	if err != nil {
		return nil, Trust{}, err
	}
	defer unlock()
	s.Invalidate()
	n, err := s.Get(ref)
	if err != nil {
		return nil, Trust{}, err
	}
	note = oneLine(note, 600)
	rev := n.Meta().Rev()
	head := s.headFor(n.Project)
	if keep {
		if note == "" {
			note = "resolved: kept"
		}
		n.Doc.AppendVerified(okf.Event{By: actor.String(), At: s.nowString(), Result: ResultConfirmed, Rev: rev, Commit: head, Evidence: note})
		s.refreshStaleAfter(n)
	} else {
		if note == "" {
			note = "resolved: deprecated"
		}
		n.Doc.AppendVerified(okf.Event{By: actor.String(), At: s.nowString(), Result: ResultDisputed, Rev: rev, Commit: head, Evidence: note})
		n.Doc.SetString("status", "deprecated")
	}
	t := s.Trust(n)
	s.applyStatus(n, t)
	if err := s.writeNode(n); err != nil {
		return nil, Trust{}, err
	}
	op := "resolve-keep"
	if !keep {
		op = "resolve-deprecate"
	}
	if err := s.recordChange(op, n.ID, actor, note); err != nil {
		return nil, Trust{}, err
	}
	return n, t, nil
}

// Deprecate retracts an entry that no longer holds. Any agent may do it
// with a reason; the entry stays on disk and in git history.
func (s *Store) Deprecate(ref, reason string, actor Actor) (*Node, error) {
	if actor.Zero() {
		return nil, ErrNoActor
	}
	reason = oneLine(reason, 300)
	if reason == "" {
		return nil, errors.New("a reason is required: -m \"<why it no longer holds>\"")
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
	n.Doc.SetString("status", "deprecated")
	if err := s.writeNode(n); err != nil {
		return nil, err
	}
	return n, s.recordChange("deprecate", n.ID, actor, reason)
}

// applyStatus keeps OKF `status` consistent with the derived trust state.
func (s *Store) applyStatus(n *Node, t Trust) {
	want := "draft"
	switch {
	case t.Trusted():
		want = "stable"
	case t.State == StateDeprecated:
		if t.SupersededBy == "" || strings.EqualFold(n.Meta().Status, "deprecated") {
			want = "deprecated"
		} else {
			want = n.Meta().Status
		}
	}
	if want != "" && n.Meta().Status != want {
		n.Doc.SetString("status", want)
	}
}

// ExpectedStatus is the OKF status consistent with a trust state, for lint.
func ExpectedStatus(t Trust) []string {
	switch {
	case t.Trusted():
		return []string{"stable"}
	case t.State == StateDeprecated:
		return []string{"deprecated", "stable", "draft"}
	}
	return []string{"draft"}
}

func supersededNote(t Trust) string {
	if t.SupersededBy != "" {
		return " (superseded by " + t.SupersededBy + ")"
	}
	return ""
}

func (s *Store) uniqueLocalID(project, dir, slug string) string {
	if slug == "" {
		slug = "entry"
	}
	base := dir + "/" + slug
	local := base
	for i := 2; ; i++ {
		if _, taken := s.byID[joinID(project, local)]; !taken && !fileExists(s.pathFor(project, local)) {
			return local
		}
		local = fmt.Sprintf("%s-%d", base, i)
	}
}

// Slugify turns a title into a filename-safe slug, keeping non-Latin letters.
func Slugify(title string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(title) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
			dash = false
		} else if !dash && b.Len() > 0 {
			b.WriteByte('-')
			dash = true
		}
	}
	slug := strings.Trim(b.String(), "-")
	if r := []rune(slug); len(r) > 60 {
		cut := strings.Trim(string(r[:61]), "-")
		if len(r) > 61 && r[61] != '-' {
			// Cut mid-word: back off to the previous word boundary.
			if i := strings.LastIndex(cut, "-"); i > 30 {
				cut = cut[:i]
			}
		}
		slug = cut
	}
	// Don't end on filler words ("...-instead-of-a").
	for {
		i := strings.LastIndex(slug, "-")
		if i <= 0 || !slugFiller[slug[i+1:]] {
			break
		}
		slug = slug[:i]
	}
	return slug
}

var slugFiller = map[string]bool{"a": true, "an": true, "the": true, "of": true, "and": true, "or": true, "to": true, "in": true, "on": true, "for": true, "with": true, "is": true, "be": true, "by": true, "at": true}

func cleanList(in []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, v := range in {
		for _, part := range strings.Split(v, ",") {
			part = strings.TrimSpace(part)
			if part != "" && !seen[part] {
				seen[part] = true
				out = append(out, part)
			}
		}
	}
	return out
}

// similar finds existing claims of the same type whose title+description
// overlap the proposal (Jaccard over tokens).
func (s *Store) similar(text, typeKey, project string) []Similar {
	q := tokenSet(text)
	if len(q) == 0 {
		return nil
	}
	sup := s.supersededIndex()
	var out []Similar
	for _, n := range s.nodes {
		if n.TypeKey != typeKey || n.Project != project || s.TrustStatic(n, sup).State == StateDeprecated {
			continue
		}
		c := tokenSet(n.Meta().Title + " " + n.Meta().Description)
		inter := 0
		for t := range q {
			if c[t] {
				inter++
			}
		}
		union := len(q) + len(c) - inter
		if union == 0 || inter == 0 {
			continue
		}
		out = append(out, Similar{ID: n.ID, Title: n.Meta().Title, Score: float64(inter) / float64(union)})
	}
	sortSimilar(out)
	if len(out) > 3 {
		out = out[:3]
	}
	return out
}

func sortSimilar(xs []Similar) {
	for i := 1; i < len(xs); i++ {
		for j := i; j > 0 && xs[j].Score > xs[j-1].Score; j-- {
			xs[j], xs[j-1] = xs[j-1], xs[j]
		}
	}
}

func tokenSet(s string) map[string]bool {
	out := map[string]bool{}
	for _, t := range Tokenize(s) {
		out[t] = true
	}
	return out
}

var secretPatterns = []*regexp.Regexp{
	regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----[\s\S]*?-----END [A-Z ]*PRIVATE KEY-----`),
	regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`),
	regexp.MustCompile(`\b(sk|rk)-(proj-|ant-|live-|test-)?[A-Za-z0-9_\-]{20,}\b`),
	regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9]{30,}\b`),
	regexp.MustCompile(`\bgithub_pat_[A-Za-z0-9_]{30,}\b`),
	regexp.MustCompile(`\bxox[abprs]-[A-Za-z0-9-]{10,}\b`),
	regexp.MustCompile(`\bAIza[0-9A-Za-z_\-]{35}\b`),
	regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\b`),
	regexp.MustCompile(`(?i)\b(password|passwd|secret|api[_-]?key|access[_-]?token|auth[_-]?token)\s*[:=]\s*["']?[^\s"'` + "`" + `]{8,}`),
}

// Redact replaces likely secrets with a placeholder and counts replacements.
func Redact(s string) (string, int) {
	count := 0
	for _, re := range secretPatterns {
		s = re.ReplaceAllStringFunc(s, func(m string) string {
			count++
			if i := strings.IndexAny(m, ":="); i > 0 && i < 40 && !strings.HasPrefix(m, "-----") {
				return m[:i+1] + " [REDACTED]"
			}
			return "[REDACTED]"
		})
	}
	return s, count
}

// ContainsSecret reports whether text matches a secret pattern.
func ContainsSecret(s string) bool {
	_, n := Redact(s)
	return n > 0
}

// ensureDir is a small helper used by init and installers.
func ensureDir(p string) error { return os.MkdirAll(p, 0o755) }
