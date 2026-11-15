package extract

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/xuehaipeng/agent-kb/internal/kb"
)

// Where Claude Code keeps what kb imports, and whose it is.
//
// A memory directory is imported only when the project it belongs to is
// established, never guessed from a directory's name alone:
//
//   - the default per-project directory: its own transcripts ran in exactly
//     one project, or its name is the encoded path of exactly one project's
//     registered checkout;
//   - a directory a repository's own settings name (autoMemoryDirectory in
//     its .claude/settings*.json): that repository's project;
//   - a subagent's project or local memory under a checkout's .claude/:
//     that checkout's project;
//   - anything shared (a user-level autoMemoryDirectory, user-level subagent
//     memory, a directory several projects wrote to): only through an
//     explicit mapping recorded with --map-root.
//
// A directory reached several ways is judged on all of it together (see
// ClaudeMemoryRoots). Everything found is reported, including what is not
// imported and why.

// MemoryRoot is one memory directory and the project it belongs to.
type MemoryRoot struct {
	Dir     string
	Agent   kb.Actor
	Project string // hub: project id or "global"; repo mode: ""
	Mapped  bool   // false when no project is established for the directory
	Kind    string // "auto memory" or "subagent memory (<scope>)"
	Why     string // how ownership was established, or why it was not
}

type rootMap struct {
	Roots map[string]string `json:"roots"` // directory -> project
}

func rootMapPath(s *kb.Store) string { return s.LocalPath("extract", "roots.json") }

func loadRootMap(s *kb.Store) *rootMap {
	m := &rootMap{}
	if data, err := os.ReadFile(rootMapPath(s)); err == nil {
		_ = json.Unmarshal(data, m)
	}
	if m.Roots == nil {
		m.Roots = map[string]string{}
	}
	return m
}

// MapRoot records, on this machine, which project a memory directory
// belongs to. spec is "<directory>=<project>"; an empty project removes
// the mapping.
func MapRoot(s *kb.Store, spec string) (string, error) {
	i := strings.LastIndex(spec, "=")
	if i <= 0 {
		return "", errors.New("--map-root takes <directory>=<project> (or <directory>= to remove a mapping)")
	}
	dir, project := expandHome(strings.TrimSpace(spec[:i])), strings.TrimSpace(spec[i+1:])
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	abs = kb.RealPath(abs)
	m := loadRootMap(s)
	if project == "" {
		delete(m.Roots, abs)
	} else {
		if st, err := os.Stat(abs); err != nil || !st.IsDir() {
			return "", fmt.Errorf("%s is not a directory", dir)
		}
		if s.Hub && project != kb.GlobalProject {
			if _, ok := s.ProjectFiles()[project]; !ok {
				return "", fmt.Errorf("unknown project %q (kb project list)", project)
			}
		}
		if !s.Hub {
			project = "" // a repo bundle has one scope
		}
		m.Roots[abs] = project
	}
	data, _ := json.MarshalIndent(m, "", "  ")
	if _, err := kb.WriteFileIfChanged(rootMapPath(s), data); err != nil {
		return "", err
	}
	if project == "" && s.Hub {
		return fmt.Sprintf("%s is no longer mapped", displayPath(abs)), nil
	}
	return fmt.Sprintf("%s now belongs to %s (on this machine)", displayPath(abs), projectOr(s, m.Roots[abs])), nil
}

func expandHome(p string) string {
	if p == "~" {
		return homeDir()
	}
	if strings.HasPrefix(p, "~/") {
		return filepath.Join(homeDir(), p[2:])
	}
	return p
}

// claudeSetting reads one key of a Claude Code settings file.
func claudeSetting(path, key string) (any, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, false
	}
	var m map[string]any
	if json.Unmarshal(data, &m) != nil {
		return nil, false
	}
	v, ok := m[key]
	return v, ok
}

// knownCheckouts lists the work trees whose project is known: the current
// one and every registered checkout that still exists.
func knownCheckouts(s *kb.Store) map[string]string {
	out := map[string]string{}
	if s.CodeRoot != "" {
		out[s.CodeRoot] = s.Project
	}
	if s.Hub {
		for id := range s.ProjectFiles() {
			for _, c := range s.Checkouts(id) {
				if c.Exists {
					out[c.Path] = id
				}
			}
		}
	}
	return out
}

// What one place says about whose a memory directory is.
const (
	ownerUnknown = iota // nothing establishes a project
	ownerOne            // it belongs to one project
	ownerShared         // it is written to from several projects, or may be
)

type ownerClaim struct {
	kind, project, why string
	level              int
}

// ClaudeMemoryRoots finds Claude Code's memory directories and establishes
// which project each belongs to. The same directory can be reached several
// ways (its default location, a repository's settings, user settings, a
// settings file given on the command line), and each says something about
// its owner. Everything said about a directory is collected first and
// weighed together: a mapping made by hand settles it; otherwise anything
// that makes it shared keeps it out, however firmly something else names
// one project; otherwise the projects named must agree.
func ClaudeMemoryRoots(s *kb.Store, o ImportOptions) []MemoryRoot {
	agent := kb.Actor{Producer: "claude-code"}
	mapped := loadRootMap(s)
	checkouts := knownCheckouts(s)
	claims := map[string][]ownerClaim{}
	found := map[string]string{} // resolved directory -> the path it was first found by
	var order []string
	add := func(dir, kind string, level int, project, why string) {
		st, err := os.Stat(dir)
		if err != nil || !st.IsDir() {
			return
		}
		real := kb.RealPath(dir)
		if _, ok := found[real]; !ok {
			found[real] = dir
			order = append(order, real)
		}
		claims[real] = append(claims[real], ownerClaim{kind: kind, project: project, why: why, level: level})
	}

	// 1. The default per-project directories under the effective config dir.
	dirs, _ := filepath.Glob(filepath.Join(claudeDir(), "projects", "*", "memory"))
	sort.Strings(dirs)
	for _, d := range dirs {
		project, level, why := claudeMemoryOwner(s, filepath.Dir(d))
		add(d, "auto memory", level, project, why)
	}
	// 2. A directory a repository's own settings name belongs to that
	// repository. Local settings outrank project settings, as in Claude Code.
	for _, co := range sortedStrings(checkouts) {
		for _, f := range []string{".claude/settings.local.json", ".claude/settings.json"} {
			v, ok := claudeSetting(filepath.Join(co, filepath.FromSlash(f)), "autoMemoryDirectory")
			dir, isStr := v.(string)
			if !ok || !isStr || strings.TrimSpace(dir) == "" {
				continue
			}
			add(expandHome(dir), "auto memory", ownerOne, checkouts[co], "named by autoMemoryDirectory in "+displayPath(filepath.Join(co, filepath.FromSlash(f))))
			break
		}
	}
	// 3. A user-level or explicitly given settings file: the directory is
	// whatever project used that setting, which a scan cannot know.
	settings := append([]string{filepath.Join(claudeDir(), "settings.json")}, o.Settings...)
	for _, f := range settings {
		v, ok := claudeSetting(f, "autoMemoryDirectory")
		dir, isStr := v.(string)
		if !ok || !isStr || strings.TrimSpace(dir) == "" {
			continue
		}
		dir = expandHome(dir)
		add(dir, "auto memory", ownerShared, "", "autoMemoryDirectory in "+displayPath(f)+" is shared by every project that runs with it: map it by hand with --map-root "+displayPath(dir)+"=<project>")
	}
	// 4. Subagent memory. Project and local scope live in a checkout and
	// belong to its project. User scope spans projects: it is not turned
	// into global knowledge on its own.
	for _, co := range sortedStrings(checkouts) {
		for _, scope := range []struct{ dir, name string }{{".claude/agent-memory", "project"}, {".claude/agent-memory-local", "local"}} {
			subs, _ := filepath.Glob(filepath.Join(co, filepath.FromSlash(scope.dir), "*"))
			sort.Strings(subs)
			for _, d := range subs {
				add(d, "subagent memory ("+scope.name+")", ownerOne, checkouts[co], "under a checkout of "+projectOr(s, checkouts[co]))
			}
		}
	}
	subs, _ := filepath.Glob(filepath.Join(claudeDir(), "agent-memory", "*"))
	sort.Strings(subs)
	for _, d := range subs {
		add(d, "subagent memory (user)", ownerShared, "", "a user-level subagent's memory spans projects and is not imported as global knowledge on its own: map it by hand with --map-root "+displayPath(d)+"=<project or global>")
	}
	// 5. Mappings made by hand for directories found nowhere else.
	for _, dir := range sortedStrings(mapped.Roots) {
		add(dir, "memory", ownerOne, mapped.Roots[dir], "mapped by hand (--map-root)")
	}

	var out []MemoryRoot
	for _, real := range order {
		cs := claims[real]
		r := MemoryRoot{Dir: found[real], Agent: agent, Kind: cs[0].kind}
		if p, has := mapped.Roots[real]; has {
			r.Project, r.Mapped, r.Why = p, true, "mapped by hand (--map-root)"
		} else {
			r.Project, r.Mapped, r.Why = weighOwners(s, found[real], cs)
		}
		out = append(out, r)
	}
	return out
}

// weighOwners decides a directory's project from everything said about it.
func weighOwners(s *kb.Store, dir string, cs []ownerClaim) (project string, ok bool, why string) {
	owners := map[string]string{} // project -> the first reason given for it
	var names []string
	unknown := ""
	for _, c := range cs {
		switch c.level {
		case ownerShared:
			return "", false, c.why
		case ownerOne:
			if _, has := owners[c.project]; !has {
				owners[c.project] = c.why
				names = append(names, projectOr(s, c.project))
			}
		default:
			if unknown == "" {
				unknown = c.why
			}
		}
	}
	switch len(owners) {
	case 0:
		return "", false, unknown
	case 1:
		for p, reason := range owners {
			return p, true, reason
		}
	}
	sort.Strings(names)
	return "", false, "several projects are named as its owner (" + strings.Join(names, ", ") + "): map it by hand with --map-root " + displayPath(dir) + "=<project>"
}

func sortedStrings[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// claudeMemoryOwner establishes the project of a default memory directory
// (~/.claude/projects/<dir>): the project its transcripts ran in, when that
// is one project; otherwise the project whose registered checkout encodes
// to the directory's name, when that is one project.
func claudeMemoryOwner(s *kb.Store, projectDir string) (project string, level int, why string) {
	const none = "no project could be established for this directory: run kb inside its repository once, or map it by hand with --map-root"
	const repo, elsewhere = "\x00repo", "\x00elsewhere"
	mapIt := ": map it by hand with --map-root " + displayPath(filepath.Join(projectDir, "memory")) + "=<project>"
	projects := map[string]bool{}
	byCWD := map[string]string{}
	// Every transcript counts: the memory still there may come from any
	// session, and a file's name says nothing about which. A session that
	// ran somewhere this store does not know (another repository) counts
	// too: it is evidence that the directory is not one project's alone.
	ts, _ := filepath.Glob(filepath.Join(projectDir, "*.jsonl"))
	sort.Strings(ts)
	for _, t := range ts {
		tr, err := ReadMeta(t)
		if err != nil || tr.CWD == "" {
			continue
		}
		p, known := byCWD[tr.CWD]
		if !known {
			p = elsewhere
			if s.Hub {
				if id := s.ForDir(tr.CWD).Project; id != "" {
					p = id
				}
			} else {
				root := s.GitTop()
				if root == "" {
					root = s.CodeRoot
				}
				if within(root, tr.CWD) {
					p = repo
				}
			}
			byCWD[tr.CWD] = p
		}
		projects[p] = true
	}
	outside := projects[elsewhere]
	delete(projects, elsewhere)
	byTranscripts := len(projects) > 0
	name := filepath.Base(projectDir)
	if !byTranscripts {
		if s.Hub {
			for id := range s.ProjectFiles() {
				for _, c := range s.Checkouts(id) {
					if claudeProjectDirName(c.Path) == name {
						projects[id] = true
					}
				}
			}
		} else if s.CodeRoot != "" && claudeProjectDirName(s.CodeRoot) == name {
			projects[repo] = true
		}
	}
	var names []string
	for p := range projects {
		if p == repo {
			p = "this repository"
		}
		names = append(names, p)
	}
	sort.Strings(names)
	switch {
	case len(projects) == 0:
		return "", ownerUnknown, none
	case len(projects) > 1 && byTranscripts:
		return "", ownerShared, "its transcripts ran in several projects (" + strings.Join(names, ", ") + "), so whose memory it holds is not known" + mapIt
	case len(projects) > 1:
		return "", ownerShared, "its name matches checkouts of several projects (" + strings.Join(names, ", ") + ")" + mapIt
	case outside:
		where := "outside any project known here"
		if !s.Hub {
			where = "outside it"
		}
		return "", ownerShared, "sessions that wrote to it ran in " + names[0] + " and also " + where + ", so whose memory it holds is not known" + mapIt
	}
	for p := range projects {
		project = strings.TrimPrefix(p, repo)
	}
	if byTranscripts {
		return project, ownerOne, "its transcripts ran in this project"
	}
	return project, ownerOne, "its name is the encoded path of a registered checkout"
}

// SourcesReport describes what a scan would look at: memory directories and
// their ownership, and how far back transcripts are available.
func SourcesReport(s *kb.Store, o ImportOptions) []string {
	var out []string
	roots := ClaudeMemoryRoots(s, o)
	out = append(out, fmt.Sprintf("Claude Code config directory: %s", displayPath(claudeDir())))
	mappedN := 0
	for _, r := range roots {
		if r.Mapped {
			mappedN++
		}
	}
	out = append(out, fmt.Sprintf("memory directories: %d found, %d with an established project, %d not imported", len(roots), mappedN, len(roots)-mappedN))
	for _, r := range roots {
		files, _ := filepath.Glob(filepath.Join(r.Dir, "*.md"))
		state := "-> " + projectOr(s, r.Project) + ": " + r.Why
		if !r.Mapped {
			state = "NOT IMPORTED: " + r.Why
		}
		out = append(out, fmt.Sprintf("  %s [%s, %d file(s)] %s", displayPath(r.Dir), r.Kind, len(files), state))
	}
	// Transcripts: what is still on disk bounds any backfill.
	days, where := 30, "the default"
	for _, f := range append([]string{filepath.Join(claudeDir(), "settings.json")}, o.Settings...) {
		if v, ok := claudeSetting(f, "cleanupPeriodDays"); ok {
			if n, isNum := v.(float64); isNum {
				days, where = int(n), "cleanupPeriodDays in "+displayPath(f)
			}
		}
	}
	out = append(out, fmt.Sprintf("transcript retention: %d day(s) (%s). Managed, project and command-line settings can change it; a scan only reads the files named here.", days, where))
	if v, ok := claudeSetting(filepath.Join(claudeDir(), "settings.json"), "desktopSessionCleanupPeriodDays"); ok {
		out = append(out, fmt.Sprintf("  Claude Desktop and Cowork transcripts have their own limit: desktopSessionCleanupPeriodDays = %v", v))
	}
	var oldest, newest time.Time
	n := 0
	ts, _ := filepath.Glob(filepath.Join(claudeDir(), "projects", "*", "*.jsonl"))
	for _, t := range ts {
		st, err := os.Stat(t)
		if err != nil {
			continue
		}
		n++
		if oldest.IsZero() || st.ModTime().Before(oldest) {
			oldest = st.ModTime()
		}
		if st.ModTime().After(newest) {
			newest = st.ModTime()
		}
	}
	if n == 0 {
		out = append(out, "transcripts on disk: none")
	} else {
		age := int(time.Since(oldest).Hours() / 24)
		out = append(out, fmt.Sprintf("transcripts on disk: %d, the oldest last written %d day(s) ago. Nothing older can be backfilled; 'kb extract --scan claude --days %d' covers what is there.", n, age, age+1))
	}
	return out
}
