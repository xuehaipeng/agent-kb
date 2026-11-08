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

// LocalDir holds machine-local, gitignored state inside the bundle.
const LocalDir = ".kb"

// Reserved file names that are never parsed as concepts.
var reserved = map[string]bool{"index.md": true, "log.md": true, "readme.md": true, "pointer.md": true}

// Node is one concept file in the bundle.
type Node struct {
	ID      string // path relative to the bundle, without ".md"
	Path    string // absolute path
	TypeKey string // config type key ("learning")
	Doc     *okf.Doc
}

// Meta is a shortcut to the typed frontmatter.
func (n *Node) Meta() *okf.Meta { return &n.Doc.Meta }

// ParseError records a file that could not be parsed.
type ParseError struct {
	Path string
	Err  error
}

// Store is an opened bundle.
type Store struct {
	Dir      string // absolute bundle directory
	CodeRoot string // absolute code work tree (may equal Dir's parent)
	Cfg      *Config
	Now      func() time.Time

	nodes  []*Node
	byID   map[string]*Node
	errs   []ParseError
	loaded bool
	git    *gitCache
}

// Open opens the bundle at dir (which must contain kb.yaml or be a bundle).
func Open(dir string) (*Store, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	if st, err := os.Stat(abs); err != nil || !st.IsDir() {
		return nil, fmt.Errorf("knowledge bundle %s not found (run 'kb init')", abs)
	}
	cfg, err := LoadConfig(abs)
	if err != nil {
		return nil, err
	}
	code := abs
	if cfg.CodeRoot != "" {
		code = filepath.Clean(filepath.Join(abs, cfg.CodeRoot))
	}
	return &Store{Dir: abs, CodeRoot: code, Cfg: cfg, Now: time.Now}, nil
}

// Discover finds the bundle: explicit root, $KB_ROOT, or walking up from cwd
// looking for <dir>/knowledge/kb.yaml (or a kb.yaml in the directory itself).
func Discover(explicit string) (*Store, error) {
	if explicit == "" {
		explicit = os.Getenv("KB_ROOT")
	}
	if explicit != "" {
		return Open(explicit)
	}
	wd, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	for d := wd; ; d = filepath.Dir(d) {
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
	return nil, errors.New("no knowledge bundle found: run 'kb init' at the repository root, or set KB_ROOT")
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// Load parses every concept in the bundle (once per Store).
func (s *Store) Load() error {
	if s.loaded {
		return nil
	}
	s.nodes, s.errs = nil, nil
	s.byID = map[string]*Node{}
	for _, tk := range s.Cfg.TypeNames() {
		tc := s.Cfg.Types[tk]
		root := filepath.Join(s.Dir, tc.Dir)
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
			n := &Node{ID: s.idFor(p), Path: p, TypeKey: tk, Doc: doc}
			s.nodes = append(s.nodes, n)
			s.byID[n.ID] = n
			return nil
		})
		if err != nil {
			return err
		}
	}
	sort.Slice(s.nodes, func(i, j int) bool { return s.nodes[i].ID < s.nodes[j].ID })
	s.loaded = true
	return nil
}

// Invalidate forces the next Load to re-read the bundle.
func (s *Store) Invalidate() { s.loaded = false }

func (s *Store) idFor(path string) string {
	rel, err := filepath.Rel(s.Dir, path)
	if err != nil {
		rel = path
	}
	return strings.TrimSuffix(filepath.ToSlash(rel), ".md")
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

// Get resolves an ID: exact ("learnings/foo"), with ".md", a path under the
// bundle, a unique basename ("foo") or a unique ID prefix.
func (s *Store) Get(ref string) (*Node, error) {
	if err := s.Load(); err != nil {
		return nil, err
	}
	r := strings.TrimSpace(filepath.ToSlash(ref))
	r = strings.TrimPrefix(r, "./")
	if b := filepath.ToSlash(filepath.Base(s.Dir)) + "/"; strings.HasPrefix(r, b) {
		r = strings.TrimPrefix(r, b)
	}
	r = strings.TrimPrefix(r, "/")
	r = strings.TrimSuffix(r, ".md")
	if n, ok := s.byID[r]; ok {
		return n, nil
	}
	var matches []*Node
	for _, n := range s.nodes {
		if filepath.Base(n.ID) == r {
			matches = append(matches, n)
		}
	}
	if len(matches) == 0 {
		for _, n := range s.nodes {
			if strings.HasPrefix(n.ID, r) || strings.HasPrefix(filepath.Base(n.ID), r) {
				matches = append(matches, n)
			}
		}
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
	s.git = nil
}
