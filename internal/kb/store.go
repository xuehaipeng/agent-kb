package kb

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/xuehaipeng/agent-kb/internal/okf"
)

// DefaultBundleDir is the bundle directory name inside a repository.
const DefaultBundleDir = "knowledge"

// LocalDir holds machine-local, gitignored state inside the bundle or hub.
const LocalDir = ".kb"

// GlobalProject is the hub scope for knowledge shared by every project.
const GlobalProject = "global"

// Reserved file names that are never parsed as concepts.
var reserved = map[string]bool{"index.md": true, "log.md": true, "readme.md": true, "pointer.md": true, "project.yaml": true}

// Node is one concept file.
type Node struct {
	ID      string // repo mode: "learnings/foo"; hub mode: "<project>/learnings/foo"
	Project string // "" in repo mode; project id or "global" in hub mode
	Path    string // absolute path
	TypeKey string // config type key ("learning")
	Doc     *okf.Doc
}

// Meta is a shortcut to the typed frontmatter.
func (n *Node) Meta() *okf.Meta { return &n.Doc.Meta }

// LocalID is the ID without the project prefix ("learnings/foo").
func (n *Node) LocalID() string {
	if n.Project == "" {
		return n.ID
	}
	return strings.TrimPrefix(n.ID, n.Project+"/")
}

// ParseError records a file that could not be parsed.
type ParseError struct {
	Path string
	Err  error
}

// Bundle is one directory of typed knowledge: a repo's bundle, a hub
// project, or the hub's global scope.
type Bundle struct {
	Project string
	Dir     string
}

// Store is an opened knowledge base: either a single repo bundle
// (<repo>/knowledge) or a multi-project hub (~/.kb) with the project of the
// current working directory attached.
type Store struct {
	Dir      string // repo mode: bundle dir; hub mode: hub root
	Hub      bool
	Project  string // hub: project of the current repo ("" outside any repo)
	RepoKey  string // hub: normalized remote of the current repo
	CodeRoot string // work tree of the current repo ("" if none)
	Cfg      *Config
	Now      func() time.Time

	nodes    []*Node
	byID     map[string]*Node
	errs     []ParseError
	loaded   bool
	gits     map[string]*gitCache
	registry *registry

	lastTruncated int
	docCache      map[string]*docCorpus
}

// Open opens a repo bundle or a hub directory.
func Open(dir string) (*Store, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	if st, err := os.Stat(abs); err != nil || !st.IsDir() {
		return nil, fmt.Errorf("knowledge base %s not found (run 'kb init' in a repo or 'kb hub init')", abs)
	}
	// Resolve symlinks so paths compare cleanly with git's resolved toplevel
	// (macOS: /var -> /private/var, /tmp -> /private/tmp). Without this,
	// filepath.Rel against the toplevel yields "../.." and path-scoped checks,
	// such as the immutability lint, silently skip.
	abs = realPath(abs)
	cfg, err := LoadConfig(abs)
	if err != nil {
		return nil, err
	}
	s := &Store{Dir: abs, Cfg: cfg, Now: time.Now, gits: map[string]*gitCache{}}
	if cfg.Mode == "hub" {
		s.Hub = true
		return s, nil
	}
	code := abs
	if cfg.CodeRoot != "" {
		code = realPath(filepath.Clean(filepath.Join(abs, cfg.CodeRoot)))
	}
	s.CodeRoot = code
	return s, nil
}

// realPath resolves symlinks, falling back to the cleaned input on error.
func realPath(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return filepath.Clean(p)
}

// RealPath is realPath for other packages comparing paths with the store.
func RealPath(p string) string { return realPath(p) }

// HubHome is the hub location: $KB_HOME, else ~/.kb.
func HubHome() string {
	if h := os.Getenv("KB_HOME"); h != "" {
		return h
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ".kb"
	}
	return filepath.Join(home, ".kb")
}

// HubExists reports whether a hub is initialised at HubHome().
func HubExists() bool { return fileExists(filepath.Join(HubHome(), ConfigFile)) }

// Discover opens the knowledge base for the current directory.
func Discover(explicit string) (*Store, error) {
	wd, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	return DiscoverAt(explicit, wd)
}

// DiscoverAt resolves the knowledge base for cwd:
//  1. an explicit root (--root or $KB_ROOT): a repo bundle or a hub;
//  2. a repo bundle (<repo>/knowledge/kb.yaml) above cwd;
//  3. the hub ($KB_HOME or ~/.kb), with the project of cwd's repo attached.
func DiscoverAt(explicit, cwd string) (*Store, error) {
	if explicit == "" {
		explicit = os.Getenv("KB_ROOT")
	}
	if explicit != "" {
		s, err := Open(explicit)
		if err != nil {
			return nil, err
		}
		if s.Hub {
			s.attachProject(cwd)
		}
		return s, nil
	}
	for d := cwd; ; d = filepath.Dir(d) {
		if fileExists(filepath.Join(d, DefaultBundleDir, ConfigFile)) {
			return Open(filepath.Join(d, DefaultBundleDir))
		}
		if fileExists(filepath.Join(d, ConfigFile)) && filepath.Base(d) == DefaultBundleDir {
			return Open(d)
		}
		parent := filepath.Dir(d)
		if parent == d {
			break
		}
	}
	if HubExists() {
		s, err := Open(HubHome())
		if err != nil {
			return nil, err
		}
		if !s.Hub {
			return nil, fmt.Errorf("%s exists but is not a hub (kb.yaml lacks 'mode: hub')", HubHome())
		}
		s.attachProject(cwd)
		return s, nil
	}
	return nil, errors.New("no knowledge base found: run 'kb hub init' once (multi-project, recommended) or 'kb init' inside a repository")
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// Bundles lists the knowledge directories of this store.
func (s *Store) Bundles() []Bundle {
	if !s.Hub {
		return []Bundle{{Project: "", Dir: s.Dir}}
	}
	out := []Bundle{{Project: GlobalProject, Dir: filepath.Join(s.Dir, GlobalProject)}}
	entries, _ := os.ReadDir(filepath.Join(s.Dir, "projects"))
	for _, e := range entries {
		if e.IsDir() && !strings.HasPrefix(e.Name(), ".") {
			out = append(out, Bundle{Project: e.Name(), Dir: filepath.Join(s.Dir, "projects", e.Name())})
		}
	}
	return out
}

// BundleDir returns the directory holding a project's knowledge.
func (s *Store) BundleDir(project string) string {
	switch {
	case !s.Hub:
		return s.Dir
	case project == GlobalProject:
		return filepath.Join(s.Dir, GlobalProject)
	}
	return filepath.Join(s.Dir, "projects", project)
}

// Load parses every concept (once per Store).
func (s *Store) Load() error {
	if s.loaded {
		return nil
	}
	s.nodes, s.errs = nil, nil
	s.byID = map[string]*Node{}
	for _, b := range s.Bundles() {
		for _, tk := range s.Cfg.TypeNames() {
			root := filepath.Join(b.Dir, s.Cfg.Types[tk].Dir)
			err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
				if err != nil {
					if errors.Is(err, fs.ErrNotExist) {
						return nil
					}
					return err
				}
				if d.IsDir() {
					if p != root && strings.HasPrefix(d.Name(), ".") {
						return filepath.SkipDir
					}
					return nil
				}
				if !strings.HasSuffix(d.Name(), ".md") || reserved[strings.ToLower(d.Name())] {
					return nil
				}
				data, err := os.ReadFile(p)
				if err != nil {
					s.errs = append(s.errs, ParseError{p, err})
					return nil
				}
				doc, err := okf.Parse(data)
				if err != nil {
					s.errs = append(s.errs, ParseError{p, err})
					return nil
				}
				n := &Node{ID: s.idFor(b, p), Project: b.Project, Path: p, TypeKey: tk, Doc: doc}
				s.nodes = append(s.nodes, n)
				s.byID[n.ID] = n
				return nil
			})
			if err != nil {
				return err
			}
		}
	}
	sort.Slice(s.nodes, func(i, j int) bool { return s.nodes[i].ID < s.nodes[j].ID })
	s.loaded = true
	return nil
}

// Invalidate forces the next Load to re-read the files.
func (s *Store) Invalidate() { s.loaded = false }

func (s *Store) idFor(b Bundle, path string) string {
	rel, err := filepath.Rel(b.Dir, path)
	if err != nil {
		rel = path
	}
	id := strings.TrimSuffix(filepath.ToSlash(rel), ".md")
	if b.Project != "" {
		id = b.Project + "/" + id
	}
	return id
}

// pathFor maps an ID back to its file.
func (s *Store) pathFor(project, localID string) string {
	return filepath.Join(s.BundleDir(project), filepath.FromSlash(localID)+".md")
}

// Nodes returns all parsed concepts sorted by ID.
func (s *Store) Nodes() ([]*Node, error) {
	if err := s.Load(); err != nil {
		return nil, err
	}
	return s.nodes, nil
}

// ParseErrors returns files that failed to parse during Load.
func (s *Store) ParseErrors() []ParseError { return s.errs }

// Get resolves a reference: an exact ID, an ID relative to the current
// project or global scope ("learnings/foo"), a unique basename ("foo") or a
// unique prefix. Ties prefer the current project, then global.
func (s *Store) Get(ref string) (*Node, error) {
	if err := s.Load(); err != nil {
		return nil, err
	}
	r := strings.TrimSpace(filepath.ToSlash(ref))
	r = strings.TrimPrefix(r, "./")
	if !s.Hub {
		if b := filepath.ToSlash(filepath.Base(s.Dir)) + "/"; strings.HasPrefix(r, b) {
			r = strings.TrimPrefix(r, b)
		}
	} else {
		r = strings.TrimPrefix(r, "projects/")
	}
	r = strings.TrimPrefix(r, "/")
	r = strings.TrimSuffix(r, ".md")
	if n, ok := s.byID[r]; ok {
		return n, nil
	}
	if s.Hub {
		for _, p := range []string{s.Project, GlobalProject} {
			if p == "" {
				continue
			}
			if n, ok := s.byID[p+"/"+r]; ok {
				return n, nil
			}
		}
	}
	pick := func(match func(*Node) bool) []*Node {
		var all, cur, glob []*Node
		for _, n := range s.nodes {
			if !match(n) {
				continue
			}
			all = append(all, n)
			if s.Project != "" && n.Project == s.Project {
				cur = append(cur, n)
			} else if n.Project == GlobalProject {
				glob = append(glob, n)
			}
		}
		if len(cur) == 1 {
			return cur
		}
		if len(cur) == 0 && len(glob) == 1 {
			return glob
		}
		return all
	}
	matches := pick(func(n *Node) bool { return filepath.Base(n.ID) == r })
	if len(matches) == 0 {
		matches = pick(func(n *Node) bool {
			return strings.HasPrefix(n.ID, r) || strings.HasPrefix(n.LocalID(), r) || strings.HasPrefix(filepath.Base(n.ID), r)
		})
	}
	switch len(matches) {
	case 1:
		return matches[0], nil
	case 0:
		return nil, fmt.Errorf("no knowledge entry %q", ref)
	}
	var ids []string
	for _, m := range matches {
		ids = append(ids, m.ID)
	}
	if len(ids) > 5 {
		ids = append(ids[:5], "...")
	}
	return nil, fmt.Errorf("%q is ambiguous: %s", ref, strings.Join(ids, ", "))
}

// resolveRef resolves a reference stored inside node n (e.g. supersedes),
// relative to n's own project first.
func (s *Store) resolveRef(n *Node, ref string) *Node {
	r := strings.TrimSuffix(strings.TrimPrefix(filepath.ToSlash(strings.TrimSpace(ref)), "/"), ".md")
	if n.Project != "" {
		if t, ok := s.byID[n.Project+"/"+r]; ok {
			return t
		}
	}
	if t, ok := s.byID[r]; ok {
		return t
	}
	return nil
}

// writeNode serializes and atomically writes a node, updating the cache.
func (s *Store) writeNode(n *Node) error {
	data, err := n.Doc.Bytes()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(n.Path), 0o755); err != nil {
		return err
	}
	if err := atomicWrite(n.Path, data); err != nil {
		return err
	}
	if s.loaded {
		if _, ok := s.byID[n.ID]; !ok {
			s.nodes = append(s.nodes, n)
			sort.Slice(s.nodes, func(i, j int) bool { return s.nodes[i].ID < s.nodes[j].ID })
		}
		s.byID[n.ID] = n
	}
	return nil
}

func atomicWrite(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".kb-tmp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(name)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return err
	}
	if err := os.Chmod(name, 0o644); err != nil {
		os.Remove(name)
		return err
	}
	return os.Rename(name, path)
}

// WriteFileIfChanged writes data when it differs from the file on disk.
func WriteFileIfChanged(path string, data []byte) (bool, error) {
	old, err := os.ReadFile(path)
	if err == nil && string(old) == string(data) {
		return false, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return false, err
	}
	return true, atomicWrite(path, data)
}

// LocalPath returns a path inside the gitignored local state directory.
func (s *Store) LocalPath(parts ...string) string {
	return filepath.Join(append([]string{s.Dir, LocalDir}, parts...)...)
}

// AppendLog appends one line to the append-only log.md.
func (s *Store) AppendLog(op, id string, actor Actor, detail string) error {
	p := filepath.Join(s.Dir, "log.md")
	if !fileExists(p) {
		if err := os.WriteFile(p, []byte(logHeader), 0o644); err != nil {
			return err
		}
	}
	f, err := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	line := fmt.Sprintf("- [%s] %s %s by %s", s.Now().UTC().Format(time.RFC3339), op, id, actor)
	if detail != "" {
		line += " - " + oneLine(detail, 160)
	}
	_, err = f.WriteString(line + "\n")
	return err
}

const logHeader = "# Knowledge log\n\nAppend-only record of changes made through `kb`. Newest last. Do not edit.\n\n"

func (s *Store) nowString() string { return s.Now().UTC().Format(time.RFC3339) }

func oneLine(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	if max > 0 && len([]rune(s)) > max {
		r := []rune(s)
		s = strings.TrimSpace(string(r[:max-1])) + "…"
	}
	return s
}

// Refresh drops cached nodes and git state, for long-lived processes.
func (s *Store) Refresh() {
	s.loaded = false
	s.gits = map[string]*gitCache{}
	s.docCache = nil
}

// recordChange runs after every mutation: append to log.md, regenerate the
// (deterministic) indexes so a commit right after a kb write passes lint,
// and in hub mode commit the change to the hub's git history.
func (s *Store) recordChange(op, id string, actor Actor, detail string) error {
	if err := s.AppendLog(op, id, actor, detail); err != nil {
		return err
	}
	if _, err := s.WriteIndex(); err != nil {
		return err
	}
	if s.Hub && s.autocommit() {
		s.commitHub(fmt.Sprintf("kb: %s %s by %s", op, id, actor))
	}
	return nil
}
