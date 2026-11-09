package cli

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/xuehaipeng/agent-kb/internal/kb"
)

// seedItem is one entry in 'kb add --from': a JSON array or JSON lines.
type seedItem struct {
	Type        string   `json:"type"`
	Title       string   `json:"title"`
	Description string   `json:"description"`
	Body        string   `json:"body"`
	Evidence    string   `json:"evidence"`
	Project     string   `json:"project"`
	Global      bool     `json:"global"`
	Projects    []string `json:"projects"`
	Tags        []string `json:"tags"`
	Resources   []string `json:"resources"`
	Supersedes  []string `json:"supersedes"`
}

func parseSeed(data []byte) ([]seedItem, error) {
	data = bytes.TrimSpace(data)
	if len(data) == 0 {
		return nil, errors.New("no entries on input")
	}
	if data[0] == '[' {
		var items []seedItem
		if err := json.Unmarshal(data, &items); err != nil {
			return nil, fmt.Errorf("parse JSON array: %w", err)
		}
		return items, nil
	}
	var items []seedItem
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 1<<20), 8<<20)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "//") {
			continue
		}
		var it seedItem
		if err := json.Unmarshal([]byte(line), &it); err != nil {
			return nil, fmt.Errorf("line %d: %w", n, err)
		}
		items = append(items, it)
	}
	return items, sc.Err()
}

// seed adds many entries in one call, e.g. an agent seeding what it already
// knows about some projects. Each entry goes through the same checks as
// 'kb add' (duplicates, restated docs, unknown projects, secrets); problems
// are reported per entry and never stop the rest.
func (a *App) seed(from string, defaults kb.AddInput) error {
	var data []byte
	var err error
	if from == "-" {
		data, err = io.ReadAll(a.Stdin)
	} else {
		data, err = os.ReadFile(from)
	}
	if err != nil {
		return err
	}
	items, err := parseSeed(data)
	if err != nil {
		return err
	}
	s, err := a.store()
	if err != nil {
		return err
	}
	var added, dupes, documented, failed int
	for i, it := range items {
		in := kb.AddInput{
			Type: firstNonEmpty(it.Type, defaults.Type), Title: it.Title, Description: it.Description, Body: it.Body,
			Evidence: firstNonEmpty(it.Evidence, defaults.Evidence), Project: firstNonEmpty(it.Project, defaults.Project),
			Projects: it.Projects, Tags: append(it.Tags, defaults.Tags...), Resources: it.Resources, Supersedes: it.Supersedes,
			Actor: defaults.Actor, Force: defaults.Force, NewProject: defaults.NewProject,
		}
		if it.Global {
			in.Project = kb.GlobalProject
		}
		label := it.Title
		if label == "" {
			label = fmt.Sprintf("entry %d", i+1)
		}
		res, err := s.Add(in)
		var dup *kb.DuplicateError
		var doc *kb.DocCoveredError
		var unknown *kb.UnknownProjectError
		switch {
		case errors.As(err, &dup):
			dupes++
			a.out("duplicate  %q: already %s (update it with kb update %s)\n", label, dup.Match.ID, dup.Match.ID)
		case errors.As(err, &doc):
			documented++
			a.out("documented %q: the repo's docs already say it in %s\n", label, doc.Match.Section.Ref())
		case errors.As(err, &unknown):
			failed++
			a.out("skipped    %q: %v\n", label, err)
		case err != nil:
			failed++
			a.out("skipped    %q: %v\n", label, err)
		default:
			added++
			a.out("added      %s [%s]\n", res.Node.ID, res.Trust.Badge())
			for _, d := range res.DocOverlap {
				a.out("           overlaps %s (coverage %.2f): keep only what the doc doesn't say\n", d.Section.Ref(), d.Coverage)
			}
		}
	}
	a.out("seeded %d of %d (%d duplicate, %d already documented, %d skipped)\n", added, len(items), dupes, documented, failed)
	return nil
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
