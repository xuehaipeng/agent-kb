package extract

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/xuehaipeng/agent-kb/internal/kb"
	"github.com/xuehaipeng/agent-kb/internal/okf"
)

// Claude Code's auto memory is a directory per repository: a MEMORY.md index
// and one topic file per memory, each with YAML frontmatter (name,
// description, type, and often modified and originSessionId). A topic file
// is already one distilled, typed note, so it is imported without a model.
// The files are living documents: Claude edits, renames and deletes them.
// A local ledger remembers which entry each file became, so an edit updates
// that entry instead of adding another, and a deleted file is flagged
// instead of silently staying "backed by its source".
//
// The frontmatter layout is what Claude Code writes today, not a documented
// contract: anything that does not look like a typed topic goes to the
// model-based import (ImportFile) and is reported as a fallback.

const (
	// AdapterClaudeMemory names this source adapter in the ledger.
	AdapterClaudeMemory = "claude-memory"

	ledgerVersion = 1
	// maxTopicBytes bounds what is taken as one note. A longer file, or one
	// with several headings, is a mixed document: turning all of it into a
	// single entry (or an always-on rule) would not be one claim.
	maxTopicBytes = 6000
)

// MemoryRoot is one auto-memory directory and the project it belongs to.
type MemoryRoot struct {
	Dir     string
	Agent   kb.Actor
	Project string // hub: project id; repo mode: ""
	Mapped  bool   // false when no project could be established for the directory
}

// ClaudeMemoryRoots lists Claude Code's auto-memory directories.
func ClaudeMemoryRoots(s *kb.Store) []MemoryRoot {
	var out []MemoryRoot
	dirs, _ := filepath.Glob(filepath.Join(claudeDir(), "projects", "*", "memory"))
	sort.Strings(dirs)
	for _, d := range dirs {
		if st, err := os.Stat(d); err != nil || !st.IsDir() {
			continue
		}
		project, ok := claudeMemoryProject(s, filepath.Dir(d))
		out = append(out, MemoryRoot{Dir: d, Agent: kb.Actor{Producer: "claude-code"}, Project: project, Mapped: ok})
	}
	return out
}

// ledger is the machine-local record of what each source file became.
type ledger struct {
	Version int                     `json:"version"`
	Sources map[string]*ledgerEntry `json:"sources"` // by absolute source path
}

type ledgerEntry struct {
	Adapter  string   `json:"adapter"`
	Root     string   `json:"root"`
	Path     string   `json:"path"`
	Aliases  []string `json:"aliases,omitempty"` // earlier paths of the same source
	Project  string   `json:"project,omitempty"`
	Name     string   `json:"name,omitempty"` // the memory's own name, for rename detection
	Type     string   `json:"type,omitempty"` // memory type when imported
	Hash     string   `json:"hash"`           // content last seen
	Outcome  string   `json:"outcome"`
	Entry    string   `json:"entry,omitempty"`    // knowledge entry the source became
	Revision int      `json:"revision,omitempty"` // entry revision this import last wrote
	Conflict string   `json:"conflict,omitempty"` // why a changed source was not applied
	Missing  string   `json:"missing_since,omitempty"`
	// MissingChecks is how many verification events the entry had when its
	// source was first found deleted; a later confirmation settles it.
	MissingChecks int    `json:"missing_checks,omitempty"`
	Updated       string `json:"updated"`
}

func ledgerPath(s *kb.Store) string { return s.LocalPath("extract", "import-ledger.json") }

func loadLedger(s *kb.Store) (*ledger, error) {
	l := &ledger{Version: ledgerVersion, Sources: map[string]*ledgerEntry{}}
	data, err := os.ReadFile(ledgerPath(s))
	if errors.Is(err, os.ErrNotExist) {
		return l, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, l); err != nil {
		return nil, fmt.Errorf("import ledger %s: %w", ledgerPath(s), err)
	}
	if l.Version > ledgerVersion {
		return nil, fmt.Errorf("import ledger %s was written by a newer kb (version %d): upgrade kb", ledgerPath(s), l.Version)
	}
	if l.Sources == nil {
		l.Sources = map[string]*ledgerEntry{}
	}
	l.Version = ledgerVersion
	return l, nil
}

func saveLedger(s *kb.Store, l *ledger) error {
	data, err := json.MarshalIndent(l, "", "  ")
	if err != nil {
		return err
	}
	_, err = kb.WriteFileIfChanged(ledgerPath(s), data)
	return err
}

// topic is a parsed memory file.
type topic struct {
	Name, Description, Type string
	Modified, Session       string
	Body                    string
}

var errNoFrontmatter = errors.New("no frontmatter")

// parseTopic reads a memory file's frontmatter and body. Keys may sit at the
// top level or under `metadata`; anything else is ignored.
func parseTopic(data []byte) (topic, error) {
	s := strings.ReplaceAll(strings.TrimPrefix(string(data), "\xef\xbb\xbf"), "\r\n", "\n")
	if !strings.HasPrefix(s, "---\n") {
		return topic{}, errNoFrontmatter
	}
	rest := s[4:]
	end := strings.Index(rest, "\n---\n")
	body := ""
	switch {
	case end >= 0:
		body = rest[end+5:]
	case strings.HasSuffix(rest, "\n---"):
		end = len(rest) - 4
	default:
		return topic{}, errors.New("unterminated frontmatter")
	}
	var fm map[string]yaml.Node
	if err := yaml.Unmarshal([]byte(rest[:end]), &fm); err != nil {
		return topic{}, fmt.Errorf("frontmatter: %w", err)
	}
	var meta map[string]yaml.Node
	if m, ok := fm["metadata"]; ok && m.Kind == yaml.MappingNode {
		_ = m.Decode(&meta)
	}
	get := func(key string) string {
		for _, src := range []map[string]yaml.Node{fm, meta} {
			if n, ok := src[key]; ok && n.Kind == yaml.ScalarNode {
				return strings.TrimSpace(n.Value)
			}
		}
		return ""
	}
	return topic{
		Name: get("name"), Description: get("description"), Type: strings.ToLower(get("type")),
		Modified: get("modified"), Session: get("originSessionId"), Body: strings.TrimSpace(body),
	}, nil
}

var (
	indexLinkRe = regexp.MustCompile(`^\s*(?:[-*+]\s+)?\[[^\]]*\]\([^)]+\)`)
	headingRe   = regexp.MustCompile(`(?m)^#{1,6}\s`)
)

// splitIndex separates an index file's link lines (pointers to topic files)
// from everything else. An index with nothing else is only a table of
// contents; one with other lines holds notes of its own.
func splitIndex(content string) (links int, rest string) {
	var keep []string
	for _, l := range strings.Split(content, "\n") {
		switch t := strings.TrimSpace(l); {
		case indexLinkRe.MatchString(l):
			links++
		case t == "" || strings.HasPrefix(t, "#"):
			keep = append(keep, l)
		default:
			keep = append(keep, l)
			rest = "x" // marks substantive content; replaced below
		}
	}
	if rest == "" {
		return links, ""
	}
	return links, strings.TrimSpace(strings.Join(keep, "\n"))
}

// memoryKBType maps a memory type to a knowledge type. A type alone never
// makes an immutable decision, and nothing is promoted to the global scope.
func memoryKBType(t string) (string, bool) {
	switch t {
	case "feedback":
		return "convention", true // the user's corrections and confirmed approaches
	case "project", "reference":
		return "fact", true
	}
	return "", false
}

func humanize(name string) string {
	n := strings.TrimSpace(strings.NewReplacer("-", " ", "_", " ").Replace(strings.TrimSuffix(name, ".md")))
	if n == "" {
		return ""
	}
	r := []rune(n)
	return strings.ToUpper(string(r[:1])) + string(r[1:])
}

func firstLine(body string) string {
	for _, l := range strings.Split(body, "\n") {
		l = strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(l), "#>-*+ "))
		l = strings.TrimSpace(strings.ReplaceAll(l, "**", ""))
		if l != "" {
			r := []rune(l)
			if len(r) > 200 {
				l = strings.TrimSpace(string(r[:199])) + "…"
			}
			return l
		}
	}
	return ""
}

func parseSourceTime(v string) (time.Time, bool) {
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05", "2006-01-02 15:04:05", "2006-01-02"} {
		if t, err := time.Parse(layout, strings.TrimSpace(v)); err == nil {
			return t.UTC(), true
		}
	}
	return time.Time{}, false
}

func contentHash(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// rootReport counts one memory directory's outcomes.
type rootReport struct {
	imported, updated, unchanged, renamed     int
	index, user, empty, duplicate, documented int
	fallback, conflicts, missing, ambiguous   int
	legacy                                    int
	errors                                    int
	details                                   []string
}

func (r *rootReport) line(root MemoryRoot, dry bool) string {
	scope := root.Project
	if scope == "" {
		scope = "repository"
	}
	verb := func(s string) string {
		if dry {
			return "would be " + s
		}
		return s
	}
	parts := []string{fmt.Sprintf("%d %s", r.imported, verb("imported")), fmt.Sprintf("%d %s", r.updated, verb("updated")), fmt.Sprintf("%d unchanged", r.unchanged)}
	add := func(n int, what string) {
		if n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", n, what))
		}
	}
	add(r.renamed, "renamed")
	add(r.legacy, "imported earlier through the model")
	add(r.index, "index skipped")
	add(r.user, "user memories skipped")
	add(r.empty, "empty skipped")
	add(r.duplicate, "already known")
	add(r.documented, "already documented")
	add(r.fallback, "sent to the model fallback")
	add(r.conflicts, "conflicts")
	add(r.missing, "sources deleted")
	add(r.ambiguous, "ambiguous")
	add(r.errors, "errors")
	out := fmt.Sprintf("%s [%s]: %s", displayPath(root.Dir), scope, strings.Join(parts, ", "))
	for _, d := range r.details {
		out += "\n  " + d
	}
	return out
}

// ImportClaudeMemory imports every auto-memory directory and returns one
// report block per directory.
func ImportClaudeMemory(s *kb.Store, o ImportOptions) []string {
	roots := ClaudeMemoryRoots(s)
	if len(roots) == 0 {
		return nil
	}
	unlock, err := s.LockLocal("import-ledger.lock")
	if err != nil {
		return []string{"claude memory: error: " + err.Error()}
	}
	defer unlock()
	l, err := loadLedger(s)
	if err != nil {
		return []string{"claude memory: error: " + err.Error()}
	}
	var report []string
	for _, root := range roots {
		switch {
		case !root.Mapped:
			report = append(report, fmt.Sprintf("%s: skipped (no project could be established for this directory; run kb inside its repository once)", displayPath(root.Dir)))
			continue
		case len(o.Projects) > 0 && !o.wants(projectOr(s, root.Project)):
			report = append(report, fmt.Sprintf("%s: skipped (not in the selected projects)", displayPath(root.Dir)))
			continue
		}
		r, fallbacks := importMemoryRoot(s, l, root, o)
		report = append(report, r.line(root, o.DryRun))
		if !o.DryRun {
			if err := saveLedger(s, l); err != nil {
				report = append(report, "  error: could not save the import ledger: "+err.Error())
			}
		}
		// Files this adapter does not understand keep the model-based path.
		if len(fallbacks) > 0 {
			report = append(report, ImportAll(s, fallbacks, o)...)
		}
	}
	return report
}

// importMemoryRoot processes one directory against the ledger.
func importMemoryRoot(s *kb.Store, l *ledger, root MemoryRoot, o ImportOptions) (*rootReport, []FileSource) {
	r := &rootReport{}
	var fallbacks []FileSource
	now := s.Now().UTC()
	entries, err := os.ReadDir(root.Dir)
	if err != nil {
		r.errors++
		r.details = append(r.details, "error: "+err.Error()+" (nothing was compared with earlier imports)")
		return r, nil
	}
	type file struct {
		path, hash string
		data       []byte
		mtime      time.Time
	}
	complete := true
	var files []file
	present := map[string]bool{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		p := filepath.Join(root.Dir, e.Name())
		data, err := os.ReadFile(p)
		st, serr := os.Stat(p)
		if err != nil || serr != nil {
			complete = false
			r.errors++
			r.details = append(r.details, fmt.Sprintf("error: %s could not be read", e.Name()))
			present[p] = true // unreadable is not deleted
			continue
		}
		if len(data) > maxImportBytes {
			data = data[:maxImportBytes]
		}
		present[p] = true
		files = append(files, file{path: p, hash: contentHash(data), data: data, mtime: st.ModTime().UTC()})
	}

	// Sources this ledger knows in this directory that are no longer there.
	gone := map[string]*ledgerEntry{}
	for p, e := range l.Sources {
		if e.Adapter == AdapterClaudeMemory && e.Root == root.Dir && !present[p] {
			gone[p] = e
		}
	}
	// A new file that carries a vanished source's content (or its name) is
	// that source renamed, when the match is unique both ways.
	newByHash, newByName := map[string][]string{}, map[string][]string{}
	names := map[string]string{}
	for _, f := range files {
		if _, known := l.Sources[f.path]; known || filepath.Base(f.path) == "MEMORY.md" {
			continue
		}
		newByHash[f.hash] = append(newByHash[f.hash], f.path)
		if t, err := parseTopic(f.data); err == nil && t.Name != "" {
			names[f.path] = t.Name
			newByName[t.Name] = append(newByName[t.Name], f.path)
		}
	}
	ambiguous := map[string]bool{}
	// A destination can stand for one vanished source only, whichever way it
	// was matched; and the candidates are counted on the set as it was, so
	// the outcome does not depend on the order sources are looked at.
	taken := map[string]bool{}     // new paths already matched to a source
	movedFrom := map[string]bool{} // vanished sources matched to a new path
	vanished := map[string]*ledgerEntry{}
	for p, g := range gone {
		vanished[p] = g
	}
	move := func(from string, to string) {
		taken[to], movedFrom[from] = true, true
		r.renamed++
		if o.DryRun {
			return
		}
		e := gone[from]
		delete(gone, from)
		delete(l.Sources, from)
		e.Aliases = append(e.Aliases, from)
		e.Path, e.Missing, e.MissingChecks = to, "", 0
		l.Sources[to] = e
	}
	free := func(paths []string) []string {
		var out []string
		for _, p := range paths {
			if !taken[p] && !ambiguous[p] {
				out = append(out, p)
			}
		}
		return out
	}
	for _, key := range sortedKeysOf(vanished) {
		e := vanished[key]
		if e.Entry == "" {
			continue
		}
		sameHash, sameName := 0, 0
		for _, g := range vanished {
			if g.Entry != "" && g.Hash == e.Hash {
				sameHash++
			}
			if g.Entry != "" && e.Name != "" && g.Name == e.Name {
				sameName++
			}
		}
		switch cands := free(newByHash[e.Hash]); {
		case len(cands) == 1 && sameHash == 1:
			move(key, cands[0])
			continue
		case len(cands) > 0:
			for _, c := range cands {
				ambiguous[c] = true
			}
			ambiguous[key] = true
			r.ambiguous++
			r.details = append(r.details, fmt.Sprintf("ambiguous: %d new file(s) have the content of %d deleted source(s) of %s; nothing was imported or flagged for them (make the files differ, or deprecate the entry)", len(cands), sameHash, e.Entry))
			continue
		}
		if cands := free(newByName[e.Name]); e.Name != "" && len(cands) == 1 && sameName == 1 {
			move(key, cands[0])
		}
	}

	for _, f := range files {
		base := filepath.Base(f.path)
		if ambiguous[f.path] || (o.DryRun && taken[f.path]) {
			continue
		}
		if base == "MEMORY.md" {
			links, rest := splitIndex(string(f.data))
			if len([]rune(rest)) < minImportContentLen {
				r.index++
				continue
			}
			// A legacy or mixed index holds notes of its own. Its pointers
			// to topic files are left out: those files are imported directly.
			r.fallback++
			r.details = append(r.details, fmt.Sprintf("fallback: %s holds notes besides its %d link(s)", base, links))
			fallbacks = append(fallbacks, FileSource{Path: f.path, Kind: KindMemory, Agent: root.Agent, Project: root.Project, Content: rest})
			continue
		}
		t, err := parseTopic(f.data)
		kbType, known := memoryKBType(t.Type)
		reason, skip := "", ""
		switch {
		case err != nil:
			reason = "no usable frontmatter"
		case t.Type == "user":
			// Notes about the person stay in the agent's private memory.
			skip = "is now a user memory"
		case !known:
			reason = fmt.Sprintf("unknown memory type %q", t.Type)
		case t.Body == "":
			skip = "is now empty"
		case len(t.Body) > maxTopicBytes:
			reason = fmt.Sprintf("longer than one note (%d bytes)", len(t.Body))
		case len(headingRe.FindAllString(t.Body, -1)) > 1:
			reason = "several sections: not a single note"
		}
		// A source that already became an entry keeps that identity. If it
		// can no longer be imported as one note, the model path must not
		// take it: that path only adds entries, so the old one (a delivered
		// rule, say) would stay as it was beside a new one. It is flagged
		// for a merge by hand instead.
		if tracked := l.Sources[f.path]; tracked != nil && tracked.Entry != "" && (reason != "" || skip != "") {
			why := reason
			if why == "" {
				why = skip
			}
			switch {
			case tracked.Conflict != "" && o.AcceptConflicts:
				if !o.DryRun {
					tracked.Hash, tracked.Conflict, tracked.Missing, tracked.Updated = f.hash, "", "", now.Format(time.RFC3339)
				}
				r.unchanged++
			case tracked.Hash == f.hash && tracked.Conflict == "":
				r.unchanged++ // settled earlier, and unchanged since
			default:
				r.conflicts++
				r.details = append(r.details, fmt.Sprintf("conflict: %s can no longer be imported as one note (%s), so %s was left as it is. Update or deprecate it by hand, then run this scan with --accept-conflicts", base, why, tracked.Entry))
				if !o.DryRun {
					tracked.Hash, tracked.Conflict, tracked.Missing, tracked.Updated = f.hash, "the source is no longer a single typed note: "+why, "", now.Format(time.RFC3339)
				}
			}
			continue
		}
		switch skip {
		case "is now a user memory":
			r.user++
			continue
		case "is now empty":
			r.empty++
			continue
		}
		if reason != "" {
			r.fallback++
			r.details = append(r.details, fmt.Sprintf("fallback: %s (%s)", base, reason))
			fallbacks = append(fallbacks, FileSource{Path: f.path, Kind: KindMemory, Agent: root.Agent, Project: root.Project})
			continue
		}

		title := strings.Join(strings.Fields(t.Description), " ")
		if title == "" {
			title = humanize(t.Name)
		}
		if title == "" {
			title = humanize(base)
		}
		desc := firstLine(t.Body)
		if desc == "" {
			desc = title
		}
		srcID := KindMemory + "-" + kb.Slugify(root.Agent.Producer)
		date, basis := f.mtime, kb.BasisSourceMtime
		if d, ok := parseSourceTime(t.Modified); ok {
			date, basis = d, kb.BasisSourceUpdatedAt
		}
		sources := []okf.Source{{
			ID: srcID, Resource: displayPath(f.path), Author: root.Agent.String(),
			LastModified: date.Format(time.RFC3339), IngestedAt: now.Format(time.RFC3339), Session: t.Session,
		}}
		body := t.Body + fmt.Sprintf(" [^%s]", srcID)
		evidence := fmt.Sprintf("saved in %s's memory (%s)", root.Agent.Producer, displayPath(f.path))

		e := l.Sources[f.path]
		if e != nil && e.Missing != "" && !o.DryRun {
			e.Missing, e.MissingChecks = "", 0 // the file is back
		}
		// A conflict stands until someone settles it: it is reported on
		// every scan, not once. --accept-conflicts records the entry as it
		// is now (after a merge by hand) as the new baseline.
		if e != nil && e.Conflict != "" {
			if o.AcceptConflicts {
				if !o.DryRun {
					if n, err := s.Get(e.Entry); err == nil {
						e.Revision = n.Meta().Rev()
					}
					e.Hash, e.Type, e.Conflict, e.Updated = f.hash, t.Type, "", now.Format(time.RFC3339)
				}
				r.unchanged++
				continue
			}
			if e.Hash == f.hash {
				r.conflicts++
				r.details = append(r.details, fmt.Sprintf("conflict (unresolved): %s and %s differ: %s. Merge by hand with kb update, then run this scan with --accept-conflicts", base, e.Entry, e.Conflict))
				continue
			}
		}
		if e != nil && e.Hash == f.hash {
			r.unchanged++
			continue
		}
		record := func(outcome, entry string, rev int, conflict string) {
			if o.DryRun {
				return
			}
			ne := &ledgerEntry{Adapter: AdapterClaudeMemory, Root: root.Dir, Path: f.path, Project: root.Project, Name: t.Name, Type: t.Type,
				Hash: f.hash, Outcome: outcome, Entry: entry, Revision: rev, Conflict: conflict, Updated: now.Format(time.RFC3339)}
			if e != nil {
				ne.Aliases = e.Aliases
			}
			l.Sources[f.path] = ne
		}

		// A source that already became an entry: apply the edit to it.
		if e != nil && e.Entry != "" {
			if e.Type != "" && e.Type != t.Type {
				if oldType, _ := memoryKBType(e.Type); oldType != kbType {
					r.conflicts++
					msg := fmt.Sprintf("memory type changed from %s to %s", e.Type, t.Type)
					r.details = append(r.details, fmt.Sprintf("conflict: %s: %s; %s was left as it is (update or deprecate it by hand)", base, msg, e.Entry))
					record(e.Outcome, e.Entry, e.Revision, msg)
					continue
				}
			}
			if o.DryRun {
				r.updated++
				continue
			}
			n, _, err := s.Update(kb.UpdateInput{
				Ref: e.Entry, Title: &title, Description: &desc, Body: &body, Tags: []string{t.Type}, SetTags: true,
				Reason: "its source memory changed (" + displayPath(f.path) + ")", Evidence: evidence, Actor: root.Agent,
				Sources: sources, SetSources: true, ExpectRevision: e.Revision,
				SourceOnly: true, SourceDate: date, SourceDateBasis: basis,
			})
			var conflict *kb.RevisionConflictError
			switch {
			case errors.As(err, &conflict):
				r.conflicts++
				r.details = append(r.details, fmt.Sprintf("conflict: %s changed, but %s was edited since the last import (revision %d, imported %d): merge by hand with kb update, then run this scan with --accept-conflicts", base, e.Entry, conflict.Got, conflict.Want))
				record(e.Outcome, e.Entry, e.Revision, "entry edited since the last import")
			case err != nil:
				r.conflicts++
				r.details = append(r.details, fmt.Sprintf("conflict: %s changed, but %s could not be updated: %v", base, e.Entry, err))
				record(e.Outcome, e.Entry, e.Revision, err.Error())
			default:
				r.updated++
				record("imported", n.ID, n.Meta().Rev(), "")
			}
			continue
		}

		// A file the earlier, model-based import already took. Its entries
		// are not linked to it, so importing it again here would duplicate
		// them: an unchanged one is done, a changed one keeps the model path
		// it has been on (whose prompt knows the existing entries).
		if e == nil || e.Outcome == "imported-by-model" {
			if st := loadState(s, f.path); st.Hash != "" {
				if st.Hash == legacyHash(f.data) {
					r.legacy++
					record("imported-by-model", "", 0, "")
					continue
				}
				r.fallback++
				r.details = append(r.details, fmt.Sprintf("fallback: %s (changed since the model imported it; its entries are not linked to it)", base))
				fallbacks = append(fallbacks, FileSource{Path: f.path, Kind: KindMemory, Agent: root.Agent, Project: root.Project})
				continue
			}
		}

		// A new source.
		if ms := s.DocCoverage(projectOr(s, root.Project), title+" "+desc); len(ms) > 0 && ms[0].Coverage >= kb.DocWarnCoverage {
			r.documented++
			record("documented", "", 0, "")
			continue
		}
		if o.DryRun {
			r.imported++
			continue
		}
		res, err := s.Add(kb.AddInput{
			Type: kbType, Title: title, Description: desc, Body: body, Tags: []string{t.Type},
			Project: root.Project, Actor: root.Agent, Evidence: evidence, NewProject: true,
			Sources: sources, SourceOnly: true, SourceDate: date, SourceDateBasis: basis,
		})
		var dup *kb.DuplicateError
		var documented *kb.DocCoveredError
		switch {
		case errors.As(err, &dup):
			r.duplicate++
			r.details = append(r.details, fmt.Sprintf("already known: %s says what %s does", base, dup.Match.ID))
			record("duplicate", "", 0, "")
		case errors.As(err, &documented):
			r.documented++
			record("documented", "", 0, "")
		case err != nil:
			// Not recorded: the next scan tries again.
			r.errors++
			r.details = append(r.details, fmt.Sprintf("error: %s: %v", base, err))
		default:
			r.imported++
			record("imported", res.Node.ID, res.Node.Meta().Rev(), "")
		}
	}

	// A source is reported deleted only when the whole directory was read:
	// an unreadable file or directory is not evidence that anything is gone.
	if !complete {
		if len(gone) > 0 {
			r.details = append(r.details, "the directory was not read completely: deleted sources were not checked")
		}
		return r, fallbacks
	}
	for _, key := range sortedKeysOf(gone) {
		e, ok := gone[key]
		if !ok || ambiguous[key] {
			continue
		}
		if e.Entry == "" {
			if !o.DryRun {
				delete(l.Sources, key) // nothing was imported from it
			}
			continue
		}
		if movedFrom[key] {
			continue // matched to a new path in this (dry) run
		}
		n, err := s.Get(e.Entry)
		if err != nil || s.Trust(n).State == kb.StateDeprecated {
			if !o.DryRun {
				delete(l.Sources, key) // the entry is retired: nothing left to review
			}
			continue
		}
		// Someone looked at the entry after the deletion was reported: it
		// now stands on that check (or rewrite), not on the vanished file.
		if e.Missing != "" && reviewedSince(n, e.MissingChecks, e.Revision) {
			if !o.DryRun {
				delete(l.Sources, key)
			}
			continue
		}
		r.missing++
		if e.Missing == "" && !o.DryRun {
			e.Missing, e.MissingChecks = now.Format(time.RFC3339), len(n.Meta().Verified)
		}
		r.details = append(r.details, fmt.Sprintf("source deleted: %s no longer backs %s; the entry is kept. If it still holds: kb verify %s --confirm -e \"...\"; if not: kb deprecate %s -m \"...\"", filepath.Base(key), e.Entry, e.Entry, e.Entry))
	}
	return r, fallbacks
}

// reviewedSince reports whether an entry was confirmed or rewritten after
// its source was found deleted: a confirmation of its current revision
// recorded since (beyond the checks it had then), or a revision newer than
// the one the import last wrote. Events are counted, not compared by time:
// an import and the scan that finds its source gone can share a timestamp.
func reviewedSince(n *kb.Node, checksThen, importedRev int) bool {
	m := n.Meta()
	if importedRev > 0 && m.Rev() > importedRev {
		return true
	}
	for i, e := range m.Verified {
		if i < checksThen || okf.EventRev(e) != m.Rev() {
			continue
		}
		if e.Result == "" || e.Result == kb.ResultConfirmed {
			return true
		}
	}
	return false
}

// legacyHash is the content hash ImportFile records for a file.
func legacyHash(data []byte) string {
	return contentHash([]byte(strings.TrimSpace(kbBlockRe.ReplaceAllString(string(data), ""))))
}

func sortedKeysOf(m map[string]*ledgerEntry) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// ScanMemories imports agents' memory files: Claude Code's typed auto memory
// through the deterministic adapter, everything else through the model.
func ScanMemories(s *kb.Store, which string, o ImportOptions) []string {
	var report []string
	if which == "claude" || which == "all" {
		report = append(report, ImportClaudeMemory(s, o)...)
	}
	if srcs := MemorySources(s, which); len(srcs) > 0 {
		report = append(report, ImportAll(s, srcs, o)...)
	}
	if len(report) == 0 {
		report = append(report, "no files found")
	}
	return report
}
