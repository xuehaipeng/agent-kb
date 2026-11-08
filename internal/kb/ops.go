package kb

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode"

	"github.com/xuehaipeng/agent-kb/internal/okf"
)

// AddInput is a proposal for a new knowledge node.
type AddInput struct {
	Type        string
	Title       string
	Description string
	Body        string
	Tags        []string
	Resources   []string
	Sources     []okf.Source
	Supersedes  []string
	Actor       Actor
	Force       bool // skip the near-duplicate check
}

// AddResult reports what Add did.
type AddResult struct {
	Node     *Node
	Related  []Similar // similar existing claims worth verifying/disputing
	Redacted int
}

// Similar is a near-match found by the duplicate check.
type Similar struct {
	ID    string
	Title string
	Score float64
}

// DuplicateError is returned when a proposal repeats an existing claim.
type DuplicateError struct{ Match Similar }

func (e *DuplicateError) Error() string {
	return fmt.Sprintf("looks like a duplicate of %s (%q, similarity %.2f): verify or dispute it with 'kb verify', or pass --force", e.Match.ID, e.Match.Title, e.Match.Score)
}

// Add writes a new claim as unverified (OKF status: draft).
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

	var supIDs []string
	for _, ref := range in.Supersedes {
		target, err := s.Get(ref)
		if err != nil {
			return nil, fmt.Errorf("supersedes: %w", err)
		}
		if target.TypeKey != tk {
			return nil, fmt.Errorf("supersedes: %s is a %s, not a %s", target.ID, target.TypeKey, tk)
		}
		supIDs = append(supIDs, target.ID)
	}

	if len(supIDs) == 0 {
		related := s.similar(in.Title+" "+in.Description, tk)
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
	meta := okf.Meta{
		Type:        DisplayType(tk),
		Title:       in.Title,
		Description: in.Description,
		Tags:        cleanList(in.Tags),
		Status:      "draft",
		Resources:   cleanList(in.Resources),
		Supersedes:  supIDs,
		Generated:   okf.Event{By: in.Actor.String(), At: now.Format(time.RFC3339), Commit: s.HeadCommit()},
		Sources:     in.Sources,
	}
	if tc.TTLDays > 0 {
		meta.StaleAfter = now.Add(time.Duration(tc.TTLDays) * 24 * time.Hour).Format(time.RFC3339)
	}
	body := strings.TrimSpace(in.Body)
	if body == "" {
		body = in.Description
	}
	id := s.uniqueID(tc.Dir, Slugify(in.Title))
	n := &Node{ID: id, Path: filepath.Join(s.Dir, filepath.FromSlash(id)+".md"), TypeKey: tk, Doc: okf.New(meta, body+"\n")}
	if err := s.writeNode(n); err != nil {
		return nil, err
	}
	detail := in.Title
	if len(supIDs) > 0 {
		detail += " (supersedes " + strings.Join(supIDs, ", ") + ")"
	}
	if err := s.AppendLog("add", id, in.Actor, detail); err != nil {
		return nil, err
	}
	res.Node = n
	return res, nil
}

// VerifyInput records an independent check of a claim against the code.
type VerifyInput struct {
	Ref      string
	Result   string // confirmed | disputed
	Evidence string
	Actor    Actor
}

// Verify appends a verification event. The verifier's model family must
// differ from the generator's; humans may always verify.
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
	genFam := s.Cfg.FamilyOf(n.Meta().Generated.By)
	myFam := s.Cfg.Family(in.Actor)
	if myFam == genFam && myFam != "human" {
		return nil, Trust{}, fmt.Errorf("self-verification rejected: %s was generated by %s (family %s) and you are %s (family %s); a different model family or a human must verify it", n.ID, n.Meta().Generated.By, genFam, in.Actor, myFam)
	}
	n.Doc.AppendVerified(okf.Event{By: in.Actor.String(), At: s.nowString(), Result: in.Result, Commit: s.HeadCommit(), Evidence: in.Evidence})
	after := s.Trust(n)
	s.applyStatus(n, after)
	if in.Result == ResultConfirmed {
		if ttl := s.Cfg.Types[n.TypeKey].TTLDays; ttl > 0 {
			n.Doc.SetString("stale_after", s.Now().UTC().Add(time.Duration(ttl)*24*time.Hour).Format(time.RFC3339))
			after = s.Trust(n)
		}
	}
	if err := s.writeNode(n); err != nil {
		return nil, Trust{}, err
	}
	if err := s.AppendLog("verify", n.ID, in.Actor, in.Result+": "+in.Evidence); err != nil {
		return nil, Trust{}, err
	}
	return n, after, nil
}

// Resolve is a human decision on a claim: keep (confirm) or deprecate.
func (s *Store) Resolve(ref string, keep bool, note string, actor Actor) (*Node, Trust, error) {
	if !actor.IsHuman() {
		return nil, Trust{}, errors.New("only a human can resolve disputes: pass --as human:<name>")
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
	if keep {
		if note == "" {
			note = "resolved: kept"
		}
		n.Doc.AppendVerified(okf.Event{By: actor.String(), At: s.nowString(), Result: ResultConfirmed, Commit: s.HeadCommit(), Evidence: note})
		if ttl := s.Cfg.Types[n.TypeKey].TTLDays; ttl > 0 {
			n.Doc.SetString("stale_after", s.Now().UTC().Add(time.Duration(ttl)*24*time.Hour).Format(time.RFC3339))
		}
	} else {
		if note == "" {
			note = "resolved: deprecated"
		}
		n.Doc.AppendVerified(okf.Event{By: actor.String(), At: s.nowString(), Result: ResultDisputed, Commit: s.HeadCommit(), Evidence: note})
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
	if err := s.AppendLog(op, n.ID, actor, note); err != nil {
		return nil, Trust{}, err
	}
	return n, t, nil
}

// Deprecate retracts a claim. Allowed for humans and for the family that
// generated it; other agents should dispute or supersede instead.
func (s *Store) Deprecate(ref, reason string, actor Actor) (*Node, error) {
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
	myFam := s.Cfg.Family(actor)
	if myFam != "human" && myFam != s.Cfg.FamilyOf(n.Meta().Generated.By) {
		return nil, fmt.Errorf("only a human or the generating family (%s) can deprecate %s; dispute it with 'kb verify --dispute' or supersede it with 'kb add --supersedes'", s.Cfg.FamilyOf(n.Meta().Generated.By), n.ID)
	}
	reason = oneLine(reason, 300)
	if reason == "" {
		return nil, errors.New("a reason is required")
	}
	n.Doc.SetString("status", "deprecated")
	if err := s.writeNode(n); err != nil {
		return nil, err
	}
	return n, s.AppendLog("deprecate", n.ID, actor, reason)
}

// applyStatus keeps OKF `status` consistent with the derived trust state.
func (s *Store) applyStatus(n *Node, t Trust) {
	want := "draft"
	switch t.State {
	case StateCorroborated, StateReviewed:
		want = "stable"
	case StateDeprecated:
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
	switch t.State {
	case StateCorroborated, StateReviewed:
		return []string{"stable"}
	case StateDeprecated:
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

func (s *Store) uniqueID(dir, slug string) string {
	if slug == "" {
		slug = "entry"
	}
	base := dir + "/" + slug
	id := base
	for i := 2; ; i++ {
		if _, taken := s.byID[id]; !taken && !fileExists(filepath.Join(s.Dir, filepath.FromSlash(id)+".md")) {
			return id
		}
		id = fmt.Sprintf("%s-%d", base, i)
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
func (s *Store) similar(text, typeKey string) []Similar {
	q := tokenSet(text)
	if len(q) == 0 {
		return nil
	}
	sup := s.supersededIndex()
	var out []Similar
	for _, n := range s.nodes {
		if n.TypeKey != typeKey || s.TrustStatic(n, sup).State == StateDeprecated {
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
