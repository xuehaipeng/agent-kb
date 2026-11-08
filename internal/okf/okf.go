// Package okf reads and writes Open Knowledge Format (v0.2) concept files:
// Markdown with a YAML frontmatter block. Unknown frontmatter keys are
// preserved on round-trip, as the spec requires of consumers.
package okf

import (
	"bytes"
	"errors"
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// Event is a provenance record: OKF `generated` and entries of `verified`.
// Result, Commit and Evidence are AgentKB extension keys.
type Event struct {
	By       string `yaml:"by"`
	At       string `yaml:"at"`
	Result   string `yaml:"result,omitempty"`
	Rev      int    `yaml:"rev,omitempty"` // content revision the event applies to
	Commit   string `yaml:"commit,omitempty"`
	Evidence string `yaml:"evidence,omitempty"`
}

// Source is an OKF v0.2 source entry, cited from the body as [^id].
type Source struct {
	ID           string `yaml:"id"`
	Resource     string `yaml:"resource,omitempty"`
	Author       string `yaml:"author,omitempty"`
	LastModified string `yaml:"last_modified,omitempty"`
}

// Review is the AgentKB extension block carried by `type: Review` nodes.
type Review struct {
	Base                   string   `yaml:"base"`
	Head                   string   `yaml:"head"`
	Files                  []string `yaml:"files,omitempty"`
	ReviewerMustDifferFrom string   `yaml:"reviewer_must_differ_from,omitempty"`
	Concepts               []string `yaml:"concepts,omitempty"`
}

// StringList accepts either a YAML scalar or a sequence of scalars.
type StringList []string

func (s *StringList) UnmarshalYAML(n *yaml.Node) error {
	switch n.Kind {
	case yaml.ScalarNode:
		if n.Value == "" {
			*s = nil
			return nil
		}
		*s = StringList{n.Value}
		return nil
	case yaml.SequenceNode:
		var out []string
		if err := n.Decode(&out); err != nil {
			return err
		}
		*s = out
		return nil
	}
	return fmt.Errorf("line %d: expected string or list", n.Line)
}

// Meta is the typed view of the frontmatter keys AgentKB understands.
type Meta struct {
	Type        string     `yaml:"type"`
	Title       string     `yaml:"title"`
	Description string     `yaml:"description"`
	Tags        StringList `yaml:"tags"`
	Status      string     `yaml:"status"`
	Resources   StringList `yaml:"resources"`
	Revision    int        `yaml:"revision"` // content revision; 0 or absent means 1
	Projects    StringList `yaml:"projects"` // for global entries: projects they apply to
	Generated   Event      `yaml:"generated"`
	Updated     *Event     `yaml:"updated"` // last content update (revision > 1)
	Verified    []Event    `yaml:"verified"`
	StaleAfter  string     `yaml:"stale_after"`
	Sources     []Source   `yaml:"sources"`
	Supersedes  StringList `yaml:"supersedes"`
	Review      *Review    `yaml:"review"`
}

// Doc is a parsed concept file.
type Doc struct {
	Meta Meta
	Body string
	root *yaml.Node // frontmatter mapping node, kept for lossless edits
}

var ErrNoFrontmatter = errors.New("missing YAML frontmatter")

// Parse splits a concept file into frontmatter and body.
func Parse(data []byte) (*Doc, error) {
	data = bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))
	s := strings.ReplaceAll(string(data), "\r\n", "\n")
	if !strings.HasPrefix(s, "---\n") {
		return nil, ErrNoFrontmatter
	}
	rest := s[4:]
	var fm, body string
	switch {
	case strings.HasPrefix(rest, "---\n"):
		fm, body = "", rest[4:]
	case rest == "---":
		fm, body = "", ""
	default:
		end := strings.Index(rest, "\n---\n")
		if end < 0 {
			if strings.HasSuffix(rest, "\n---") {
				end = len(rest) - 4
				fm, body = rest[:end], ""
				break
			}
			return nil, errors.New("unterminated YAML frontmatter")
		}
		fm, body = rest[:end], rest[end+5:]
	}
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(fm), &doc); err != nil {
		return nil, fmt.Errorf("frontmatter: %w", err)
	}
	root := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	if doc.Kind == yaml.DocumentNode && len(doc.Content) == 1 {
		if doc.Content[0].Kind != yaml.MappingNode {
			return nil, errors.New("frontmatter must be a YAML mapping")
		}
		root = doc.Content[0]
	}
	d := &Doc{Body: body, root: root}
	if err := root.Decode(&d.Meta); err != nil {
		return nil, fmt.Errorf("frontmatter: %w", err)
	}
	return d, nil
}

// New builds a document from typed metadata with a stable key order.
func New(m Meta, body string) *Doc {
	d := &Doc{Meta: m, Body: body, root: &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}}
	d.Set("type", str(m.Type))
	d.Set("title", str(m.Title))
	if m.Description != "" {
		d.Set("description", str(m.Description))
	}
	if len(m.Tags) > 0 {
		d.Set("tags", flowList(m.Tags))
	}
	if m.Status != "" {
		d.Set("status", str(m.Status))
	}
	if len(m.Resources) > 0 {
		d.Set("resources", flowList(m.Resources))
	}
	if len(m.Projects) > 0 {
		d.Set("projects", flowList(m.Projects))
	}
	if len(m.Supersedes) > 0 {
		d.Set("supersedes", flowList(m.Supersedes))
	}
	if m.Review != nil {
		d.Set("review", reviewNode(m.Review))
	}
	d.Set("generated", EventNode(m.Generated, true))
	if len(m.Verified) > 0 {
		seq := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
		for _, e := range m.Verified {
			seq.Content = append(seq.Content, EventNode(e, false))
		}
		d.Set("verified", seq)
	}
	if m.StaleAfter != "" {
		d.Set("stale_after", str(m.StaleAfter))
	}
	if len(m.Sources) > 0 {
		seq := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
		for _, s := range m.Sources {
			seq.Content = append(seq.Content, sourceNode(s))
		}
		d.Set("sources", seq)
	}
	return d
}

// Set replaces (or appends) a top-level frontmatter key and refreshes Meta.
func (d *Doc) Set(key string, val *yaml.Node) {
	for i := 0; i+1 < len(d.root.Content); i += 2 {
		if d.root.Content[i].Value == key {
			d.root.Content[i+1] = val
			d.refresh()
			return
		}
	}
	d.root.Content = append(d.root.Content, str(key), val)
	d.refresh()
}

// SetString is Set for a plain scalar.
func (d *Doc) SetString(key, val string) { d.Set(key, str(val)) }

// Has reports whether a top-level key is present.
func (d *Doc) Has(key string) bool {
	for i := 0; i+1 < len(d.root.Content); i += 2 {
		if d.root.Content[i].Value == key {
			return true
		}
	}
	return false
}

// Keys lists top-level frontmatter keys in file order.
func (d *Doc) Keys() []string {
	var out []string
	for i := 0; i+1 < len(d.root.Content); i += 2 {
		out = append(out, d.root.Content[i].Value)
	}
	return out
}

// AppendVerified appends an event to the `verified` list, creating it if needed.
func (d *Doc) AppendVerified(e Event) {
	node := EventNode(e, false)
	for i := 0; i+1 < len(d.root.Content); i += 2 {
		if d.root.Content[i].Value == "verified" {
			seq := d.root.Content[i+1]
			if seq.Kind != yaml.SequenceNode {
				seq = &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
				d.root.Content[i+1] = seq
			}
			seq.Style = 0
			seq.Content = append(seq.Content, node)
			d.refresh()
			return
		}
	}
	d.Set("verified", &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq", Content: []*yaml.Node{node}})
}

func (d *Doc) refresh() {
	var m Meta
	if err := d.root.Decode(&m); err == nil {
		d.Meta = m
	}
}

// Bytes serializes the document back to Markdown with frontmatter.
func (d *Doc) Bytes() ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteString("---\n")
	if len(d.root.Content) > 0 {
		enc := yaml.NewEncoder(&buf)
		enc.SetIndent(2)
		if err := enc.Encode(d.root); err != nil {
			return nil, err
		}
		if err := enc.Close(); err != nil {
			return nil, err
		}
	}
	buf.WriteString("---\n")
	body := d.Body
	if body != "" && !strings.HasSuffix(body, "\n") {
		body += "\n"
	}
	buf.WriteString(body)
	return buf.Bytes(), nil
}

// ContentKey returns the frontmatter and body with the keys AgentKB is allowed
// to mutate removed, for immutability checks.
func (d *Doc) ContentKey(mutable map[string]bool) string {
	var b strings.Builder
	for i := 0; i+1 < len(d.root.Content); i += 2 {
		k := d.root.Content[i].Value
		if mutable[k] {
			continue
		}
		out, _ := yaml.Marshal(d.root.Content[i+1])
		b.WriteString(k)
		b.WriteString("=")
		b.Write(out)
	}
	b.WriteString("\x00")
	b.WriteString(strings.TrimSpace(d.Body))
	return b.String()
}

func str(s string) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: s}
}

func flowList(items []string) *yaml.Node {
	seq := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq", Style: yaml.FlowStyle}
	total := 0
	for _, it := range items {
		seq.Content = append(seq.Content, str(it))
		total += len(it) + 2
	}
	if total > 70 {
		seq.Style = 0
	}
	return seq
}

// EventNode renders an Event; flow style keeps short records on one line.
func EventNode(e Event, flow bool) *yaml.Node {
	m := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	if flow {
		m.Style = yaml.FlowStyle
	}
	add := func(k, v string) {
		if v != "" {
			m.Content = append(m.Content, str(k), str(v))
		}
	}
	add("by", e.By)
	add("at", e.At)
	add("result", e.Result)
	if e.Rev > 0 {
		m.Content = append(m.Content, str("rev"), &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!int", Value: fmt.Sprint(e.Rev)})
	}
	add("commit", e.Commit)
	add("evidence", e.Evidence)
	return m
}

// IntNode renders an integer scalar.
func IntNode(v int) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!int", Value: fmt.Sprint(v)}
}

// Rev returns the content revision (absent or 0 means 1).
func (m *Meta) Rev() int {
	if m.Revision < 1 {
		return 1
	}
	return m.Revision
}

// EventRev returns the revision an event applies to (absent means 1).
func EventRev(e Event) int {
	if e.Rev < 1 {
		return 1
	}
	return e.Rev
}

func sourceNode(s Source) *yaml.Node {
	m := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	add := func(k, v string) {
		if v != "" {
			m.Content = append(m.Content, str(k), str(v))
		}
	}
	add("id", s.ID)
	add("resource", s.Resource)
	add("author", s.Author)
	add("last_modified", s.LastModified)
	return m
}

func reviewNode(r *Review) *yaml.Node {
	m := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	m.Content = append(m.Content, str("base"), str(r.Base), str("head"), str(r.Head))
	if len(r.Files) > 0 {
		m.Content = append(m.Content, str("files"), flowList(r.Files))
	}
	if r.ReviewerMustDifferFrom != "" {
		m.Content = append(m.Content, str("reviewer_must_differ_from"), str(r.ReviewerMustDifferFrom))
	}
	if len(r.Concepts) > 0 {
		m.Content = append(m.Content, str("concepts"), flowList(r.Concepts))
	}
	return m
}

// ReviewNode exposes reviewNode for callers updating a review block.
func ReviewNode(r *Review) *yaml.Node { return reviewNode(r) }

// ListNode renders a list of strings (flow style when short).
func ListNode(items []string) *yaml.Node { return flowList(items) }
