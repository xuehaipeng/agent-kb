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
	MissingChecks int `json:"missing_checks,omitempty"`
	// MissingRevision is the entry's revision at that moment; only a later
	// rewrite settles it (the entry may have been edited before).
	MissingRevision int    `json:"missing_revision,omitempty"`
	Updated         string `json:"updated"`
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

var indexTargetRe = regexp.MustCompile(`\]\(([^)\s]+)[^)]*\)`)

// splitIndex separates an index file's pointers to topic files from
// everything else. A pointer is a link line whose target is a memory file in
// the same directory: that file is imported itself. Any other line is
// content of the index: a note, or a link to somewhere else (a runbook, a
// dashboard), which no topic file preserves. An index with pointers only is
// a table of contents; one with other lines holds notes of its own.
func splitIndex(content, dir string) (links int, rest string) {
	var keep []string
	substantive := false
	for _, l := range strings.Split(content, "\n") {
		t := strings.TrimSpace(l)
		if indexLinkRe.MatchString(l) && pointsToTopic(l, dir) {
			links++
			continue
		}
		keep = append(keep, l)
		if t != "" && !strings.HasPrefix(t, "#") {
			substantive = true
		}
	}
	if !substantive {
		return links, ""
	}
	return links, strings.TrimSpace(strings.Join(keep, "\n"))
}

// pointsToTopic reports whether a link line's first target is a Markdown
// file that exists in dir.
func pointsToTopic(line, dir string) bool {
	m := indexTargetRe.FindStringSubmatch(line)
	if m == nil {
		return false
	}
	target := m[1]
	if i := strings.IndexAny(target, "#?"); i >= 0 {
		target = target[:i]
	}
	if target == "" || strings.Contains(target, "://") || filepath.IsAbs(target) || !strings.HasSuffix(strings.ToLower(target), ".md") {
		return false
	}
	p := filepath.Join(dir, filepath.FromSlash(target))
	if !within(dir, p) {
		return false
	}
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
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
	roots := ClaudeMemoryRoots(s, o)
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
			report = append(report, fmt.Sprintf("%s: skipped (%s)", displayPath(root.Dir), root.Why))
			continue
		case len(o.Projects) > 0 && !o.wants(projectOr(s, root.Project)):
			report = append(report, fmt.Sprintf("%s: skipped (not in the selected projects)", displayPath(root.Dir)))
			continue
		}
		r, fallbacks := importMemoryRoot(s, l, root, o, false)
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
	// A directory that was imported before and is not found any more. Its
	// sources are reconciled like deleted files, but only when the
	// directory is known to be gone: an unreadable or unreachable location
	// is not a deletion.
	found := map[string]bool{}
	for _, root := range roots {
		found[root.Dir] = true
	}
	for _, dir := range trackedRoots(l) {
		if found[dir] {
			continue
		}
		root := MemoryRoot{Dir: dir, Agent: kb.Actor{Producer: "claude-code"}, Mapped: true}
		for _, e := range l.Sources {
			if e.Adapter == AdapterClaudeMemory && e.Root == dir {
				root.Project = e.Project
				break
			}
		}
		if len(o.Projects) > 0 && !o.wants(projectOr(s, root.Project)) {
			continue
		}
		gone, why := directoryGone(dir)
		if !gone {
			report = append(report, fmt.Sprintf("%s: imported before, and not checked now (%s); nothing was compared with earlier imports", displayPath(dir), why))
			continue
		}
		r, _ := importMemoryRoot(s, l, root, o, true)
		if r.missing+r.errors == 0 {
			continue // nothing left there that still backs an entry
		}
		report = append(report, r.line(root, o.DryRun)+"\n  the directory itself no longer exists")
		if !o.DryRun {
			if err := saveLedger(s, l); err != nil {
				report = append(report, "  error: could not save the import ledger: "+err.Error())
			}
		}
	}
	return report
}

// trackedRoots lists the directories the ledger holds sources of.
func trackedRoots(l *ledger) []string {
	seen := map[string]bool{}
	for _, e := range l.Sources {
		if e.Adapter == AdapterClaudeMemory && e.Root != "" {
			seen[e.Root] = true
		}
	}
	out := make([]string, 0, len(seen))
	for d := range seen {
		out = append(out, d)
	}
	sort.Strings(out)
	return out
}

// directoryGone reports whether a directory is known to have been removed:
// it does not exist, and a directory at most two levels above it does and
// can be read (for a memory directory, its project directory or the
// projects directory). Anything else (a permission error, a missing config
// directory, an unmounted home) is not evidence that it was deleted.
func directoryGone(dir string) (bool, string) {
	if _, err := os.Stat(dir); err == nil {
		return false, "it exists, but no longer counts as a memory directory of a known project"
	} else if !errors.Is(err, os.ErrNotExist) {
		return false, "it cannot be read: " + err.Error()
	}
	up := dir
	for i := 0; i < 2; i++ {
		up = filepath.Dir(up)
		if _, err := os.ReadDir(up); err == nil {
			return true, ""
		} else if !errors.Is(err, os.ErrNotExist) {
			return false, "its parent cannot be read: " + err.Error()
		}
	}
	return false, "the location it was in is not there either"
}

// importMemoryRoot processes one directory against the ledger. With removed
// set, the directory is known to have been removed: it is read as empty.
func importMemoryRoot(s *kb.Store, l *ledger, root MemoryRoot, o ImportOptions, removed bool) (*rootReport, []FileSource) {
	r := &rootReport{}
	var fallbacks []FileSource
	now := s.Now().UTC()
	entries, err := os.ReadDir(root.Dir)
	if removed {
		entries, err = nil, nil
	}
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
	// A vanished source whose entry was retired has nothing left to
	// continue: it takes no part in rename matching, so it can neither
	// claim a new file nor make one ambiguous. That is what makes
	// "deprecate the entry" a way out of an ambiguous rename: the new files
	// are then plain new sources.
	for p, e := range gone {
		if e.Entry != "" && !entryAlive(s, e.Entry) {
			delete(gone, p)
			if !o.DryRun {
				delete(l.Sources, p)
			}
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
		// The entry names its source: point it at the new file. A rename
		// whose content also changed is rewritten by the update that
		// follows; one whose content did not change would otherwise keep
		// naming a file that no longer exists.
		if e.Entry != "" {
			if err := s.RelocateSource(e.Entry, KindMemory+"-"+kb.Slugify(root.Agent.Producer), displayPath(to), root.Agent); err != nil {
				r.details = append(r.details, fmt.Sprintf("renamed: %s still names %s as its source (%v)", e.Entry, filepath.Base(from), err))
			}
		}
		e.Aliases = append(e.Aliases, from)
		e.Path, e.Missing, e.MissingChecks, e.MissingRevision = to, "", 0, 0
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
	// An ambiguous group is withheld as a whole: every vanished source that
	// shares the content or the name, not just the one being looked at.
	// Otherwise the next one in the group would find its candidates already
	// set aside and be reported deleted, though which one survived is unknown.
	quarantine := func(match func(*ledgerEntry) bool, cands []string) {
		for _, c := range cands {
			ambiguous[c] = true
		}
		for p, g := range vanished {
			if g.Entry != "" && match(g) {
				ambiguous[p] = true
			}
		}
		r.ambiguous++
	}
	// Content first, for every vanished source, then names. A source whose
	// exact content sits in one new file is that file, whatever else shares
	// its name; only what content cannot settle is matched by name. Doing
	// both per source would let an earlier source's name match set aside a
	// file that a later source matches exactly.
	for _, key := range sortedKeysOf(vanished) {
		e := vanished[key]
		if e.Entry == "" || ambiguous[key] {
			continue
		}
		sameHash := 0
		for _, g := range vanished {
			if g.Entry != "" && g.Hash == e.Hash {
				sameHash++
			}
		}
		switch cands := free(newByHash[e.Hash]); {
		case len(cands) == 1 && sameHash == 1:
			move(key, cands[0])
		case len(cands) > 0:
			quarantine(func(g *ledgerEntry) bool { return g.Hash == e.Hash }, cands)
			r.details = append(r.details, fmt.Sprintf("ambiguous: %d new file(s) have the content of %d deleted source(s) (%s among them); nothing was imported or flagged for them (make the files differ, or deprecate the entry)", len(cands), sameHash, e.Entry))
		}
	}
	for _, key := range sortedKeysOf(vanished) {
		e := vanished[key]
		if e.Entry == "" || e.Name == "" || ambiguous[key] || movedFrom[key] {
			continue
		}
		// Sources that share the name and are still unaccounted for.
		open := func(p string, g *ledgerEntry) bool {
			return g.Entry != "" && g.Name == e.Name && !movedFrom[p] && !ambiguous[p]
		}
		sameName := 0
		for p, g := range vanished {
			if open(p, g) {
				sameName++
			}
		}
		switch cands := free(newByName[e.Name]); {
		case len(cands) == 1 && sameName == 1:
			move(key, cands[0])
		case len(cands) > 0:
			// Several new files carry the vanished source's name (a renamed
			// and edited copy next to a backup), or several vanished
			// sources share it. Which one continues the source is not
			// knowable, so none is imported and nothing is reported deleted.
			for _, c := range cands {
				ambiguous[c] = true
			}
			for p, g := range vanished {
				if open(p, g) {
					ambiguous[p] = true
				}
			}
			r.ambiguous++
			r.details = append(r.details, fmt.Sprintf("ambiguous: %d new file(s) carry the name %q of %d deleted source(s) (%s among them); nothing was imported or flagged for them (keep one file per source with that name, or deprecate the entries)", len(cands), e.Name, sameName, e.Entry))
		}
	}

	for _, f := range files {
		base := filepath.Base(f.path)
		if ambiguous[f.path] || (o.DryRun && taken[f.path]) {
			continue
		}
		if base == "MEMORY.md" {
			links, rest := splitIndex(string(f.data), root.Dir)
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
		if tracked := l.Sources[f.path]; tracked != nil && entryAlive(s, tracked.Entry) && (reason != "" || skip != "") {
			why := reason
			if why == "" {
				why = skip
			}
			switch {
			case tracked.Conflict != "" && o.AcceptConflicts:
				if !o.DryRun {
					// The entry as merged by hand is the new baseline, so
					// that a later edit of a source that is a single note
					// again updates it instead of conflicting forever.
					if n, err := s.Get(tracked.Entry); err == nil {
						tracked.Revision = n.Meta().Rev()
					}
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
			e.Missing, e.MissingChecks, e.MissingRevision = "", 0, 0 // the file is back
		}
		// A conflict stands until someone settles it: it is reported on
		// every scan, not once. --accept-conflicts records the entry as it
		// is now (after a merge by hand) as the new baseline.
		// Retiring the entry settles a conflict: there is nothing left to
		// protect, and the source is free to be imported again (that is the
		// way out a type conflict advertises). Checked before a standing
		// conflict is reported or accepted, so it also works when the
		// source has not changed since.
		if e != nil && e.Conflict != "" && !entryAlive(s, e.Entry) {
			e = nil
		}
		if e != nil && e.Conflict != "" {
			if o.AcceptConflicts {
				// The entry as it is now is the baseline. Its imported type
				// is kept: accepting never makes a later edit of a source
				// of another type apply to it.
				if !o.DryRun {
					if n, err := s.Get(e.Entry); err == nil {
						e.Revision = n.Meta().Rev()
					}
					e.Hash, e.Conflict, e.Updated = f.hash, "", now.Format(time.RFC3339)
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
		// --force reconsiders an unchanged source that owns no entry: one
		// recorded as already known or documented may be importable now
		// (the entry it duplicated was deprecated, say). A source that owns
		// an entry stays as it is: forcing never rewrites or duplicates it.
		forced := o.Force && e != nil && e.Entry == "" && e.Conflict == ""
		if e != nil && e.Hash == f.hash && !forced {
			r.unchanged++
			continue
		}
		// The memory type recorded with the source: the one its entry was
		// imported as. A conflict must not replace it.
		recType := t.Type
		record := func(outcome, entry string, rev int, conflict string) {
			if o.DryRun {
				return
			}
			ne := &ledgerEntry{Adapter: AdapterClaudeMemory, Root: root.Dir, Path: f.path, Project: root.Project, Name: t.Name, Type: recType,
				Hash: f.hash, Outcome: outcome, Entry: entry, Revision: rev, Conflict: conflict, Updated: now.Format(time.RFC3339)}
			if e != nil {
				ne.Aliases = e.Aliases
			}
			l.Sources[f.path] = ne
		}

		// A source whose entry was retired is free to become a new one: that
		// is how a memory whose type changed is taken over (deprecate the
		// old entry; the memory then imports as its new type).
		if e != nil && e.Entry != "" {
			if n, err := s.Get(e.Entry); err != nil || s.Trust(n).State == kb.StateDeprecated {
				e = nil
			} else if n.TypeKey != kbType {
				// Decided on the entry's actual type, on every scan: an
				// update cannot change an entry's type, so applying the
				// source would put, say, a project fact into a delivered
				// rule. Accepting the conflict does not lift this.
				r.conflicts++
				msg := fmt.Sprintf("the memory is now a %s, but its entry is a %s", t.Type, n.TypeKey)
				r.details = append(r.details, fmt.Sprintf("conflict: %s: %s; %s was left as it is. Deprecate it and the memory imports as a new entry, or keep both as they are", base, msg, e.Entry))
				if e.Type != "" {
					recType = e.Type
				}
				record(e.Outcome, e.Entry, e.Revision, msg)
				continue
			}
		}

		// A source that already became an entry: apply the edit to it.
		if e != nil && e.Entry != "" {
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
				if st.Hash == legacyHash(f.data) && !forced {
					r.legacy++
					record("imported-by-model", "", 0, "")
					continue
				}
				if forced {
					// The model path honors --force itself.
					r.fallback++
					r.details = append(r.details, fmt.Sprintf("fallback: %s (forced re-import through the model, which imported it earlier)", base))
					fallbacks = append(fallbacks, FileSource{Path: f.path, Kind: KindMemory, Agent: root.Agent, Project: root.Project})
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
		if e.Missing != "" && reviewedSince(n, e.MissingChecks, e.MissingRevision) {
			if !o.DryRun {
				delete(l.Sources, key)
			}
			continue
		}
		r.missing++
		if e.Missing == "" && !o.DryRun {
			e.Missing, e.MissingChecks, e.MissingRevision = now.Format(time.RFC3339), len(n.Meta().Verified), n.Meta().Rev()
		}
		r.details = append(r.details, fmt.Sprintf("source deleted: %s no longer backs %s; the entry is kept. If it still holds: kb verify %s --confirm -e \"...\"; if not: kb deprecate %s -m \"...\"", filepath.Base(key), e.Entry, e.Entry, e.Entry))
	}
	return r, fallbacks
}

// entryAlive reports whether a tracked entry still exists and is not
// deprecated.
func entryAlive(s *kb.Store, id string) bool {
	if id == "" {
		return false
	}
	n, err := s.Get(id)
	return err == nil && s.Trust(n).State != kb.StateDeprecated
}

// reviewedSince reports whether an entry was confirmed or rewritten after
// its source was found deleted: a confirmation of its current revision
// beyond the checks it had then, or a revision newer than the one it had
// then. Both baselines are taken at detection. Events are counted, not
// compared by time (an import and the scan that finds its source gone can
// share a timestamp), and an edit made before the deletion does not count.
func reviewedSince(n *kb.Node, checksThen, revisionThen int) bool {
	m := n.Meta()
	if revisionThen > 0 && m.Rev() > revisionThen {
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
