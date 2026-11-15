package kb

import (
	"fmt"
	"regexp"
	"strings"
	"sync"

	"github.com/xuehaipeng/agent-kb/internal/okf"
)

// A rule may apply only when certain files are worked on (a path-scoped
// rule file such as Claude Code's `paths:` rules). That is instruction
// applicability, recorded in `applies_to`. It is not `resources`: resources
// say which code a claim depends on (for search and staleness) and never
// limit when a rule is delivered. A conditional rule is left out of every
// always-on surface and surfaces only for a matching file
// ('kb rules -f <path>', 'kb search -f <path>').

// OriginClaudeRules marks patterns written in Claude Code's rule syntax.
const OriginClaudeRules = "claude-rules"

const maxExpandedPatterns = 1000

// Conditional reports whether an entry applies only to matching files.
func Conditional(m *okf.Meta) bool {
	return m.AppliesTo != nil && len(m.AppliesTo.Paths) > 0
}

// ValidateApplies checks that every pattern can be matched exactly. A
// pattern that cannot is an error, never a wider or an empty match in
// disguise: the caller reports it instead of importing the rule.
func ValidateApplies(a *okf.Applicability) error {
	if a == nil {
		return nil
	}
	if len(a.Paths) == 0 {
		return fmt.Errorf("applies_to has no paths")
	}
	for _, p := range a.Paths {
		if _, err := compileApplies(p); err != nil {
			return fmt.Errorf("pattern %q: %w", p, err)
		}
	}
	return nil
}

// AppliesToFile reports whether a file (relative to the repository root)
// matches the applicability. Patterns are relative to its base directory.
// Semantics follow Claude Code's documented rule patterns:
//   - "**" crosses directories; "*", "?" and "[...]" stay within one segment
//   - a pattern without "/" matches a file name at any depth ("*.md")
//   - a leading "/" anchors it to the base directory ("/*.md")
//   - any other pattern is relative to the base directory ("src/**/*")
//   - "{a,b}" expands to alternatives; "\" escapes the next character
func AppliesToFile(a *okf.Applicability, file string) bool {
	if a == nil {
		return false
	}
	rel := strings.TrimPrefix(strings.ReplaceAll(strings.TrimSpace(file), "\\", "/"), "./")
	rel = strings.TrimPrefix(rel, "/")
	if base := strings.Trim(strings.ReplaceAll(a.Base, "\\", "/"), "/"); base != "" && base != "." {
		if !strings.HasPrefix(rel, base+"/") {
			return false
		}
		rel = strings.TrimPrefix(rel, base+"/")
	}
	if rel == "" {
		return false
	}
	for _, p := range a.Paths {
		res, err := compileApplies(p)
		if err != nil {
			continue // an unmatchable pattern matches nothing
		}
		for _, re := range res {
			if re.MatchString(rel) {
				return true
			}
		}
	}
	return false
}

var (
	appliesMu    sync.Mutex
	appliesCache = map[string][]*regexp.Regexp{}
)

func compileApplies(pattern string) ([]*regexp.Regexp, error) {
	appliesMu.Lock()
	defer appliesMu.Unlock()
	if res, ok := appliesCache[pattern]; ok {
		return res, nil
	}
	p := strings.TrimSpace(strings.ReplaceAll(pattern, "\r", ""))
	if p == "" {
		return nil, fmt.Errorf("empty pattern")
	}
	alts, err := expandBraces(p)
	if err != nil {
		return nil, err
	}
	var out []*regexp.Regexp
	for _, alt := range alts {
		re, err := globRegexp(alt)
		if err != nil {
			return nil, err
		}
		out = append(out, re)
	}
	appliesCache[pattern] = out
	return out, nil
}

// expandBraces expands {a,b} groups (nested ones too), within a bound.
func expandBraces(p string) ([]string, error) {
	start, depth := -1, 0
	for i := 0; i < len(p); i++ {
		switch p[i] {
		case '\\':
			i++
		case '{':
			if depth == 0 {
				start = i
			}
			depth++
		case '}':
			if depth == 0 {
				return nil, fmt.Errorf("unbalanced '}'")
			}
			depth--
			if depth == 0 {
				var out []string
				for _, alt := range splitTopLevel(p[start+1 : i]) {
					rest, err := expandBraces(p[:start] + alt + p[i+1:])
					if err != nil {
						return nil, err
					}
					if out = append(out, rest...); len(out) > maxExpandedPatterns {
						return nil, fmt.Errorf("expands to more than %d patterns", maxExpandedPatterns)
					}
				}
				return out, nil
			}
		}
	}
	if depth != 0 {
		return nil, fmt.Errorf("unbalanced '{'")
	}
	return []string{p}, nil
}

func splitTopLevel(s string) []string {
	var out []string
	depth, last := 0, 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '\\':
			i++
		case '{':
			depth++
		case '}':
			depth--
		case ',':
			if depth == 0 {
				out = append(out, s[last:i])
				last = i + 1
			}
		}
	}
	return append(out, s[last:])
}

// globRegexp turns one brace-free pattern into an anchored regexp.
func globRegexp(p string) (*regexp.Regexp, error) {
	rooted := strings.HasPrefix(p, "/")
	p = strings.TrimPrefix(p, "/")
	if p == "" {
		return nil, fmt.Errorf("empty pattern")
	}
	var b strings.Builder
	b.WriteString("^")
	if !rooted && !strings.Contains(p, "/") {
		b.WriteString("(?:.*/)?") // a bare name matches at any depth
	}
	r := []rune(p)
	for i := 0; i < len(r); i++ {
		switch c := r[i]; c {
		case '\\':
			if i+1 >= len(r) {
				return nil, fmt.Errorf("trailing backslash")
			}
			i++
			b.WriteString(regexp.QuoteMeta(string(r[i])))
		case '*':
			if i+1 < len(r) && r[i+1] == '*' {
				for i+1 < len(r) && r[i+1] == '*' {
					i++
				}
				if i+1 < len(r) && r[i+1] == '/' {
					i++
					b.WriteString("(?:.*/)?") // "**/" also matches no directory
				} else {
					b.WriteString(".*")
				}
			} else {
				b.WriteString("[^/]*")
			}
		case '?':
			b.WriteString("[^/]")
		case '[':
			end := -1
			for j := i + 1; j < len(r); j++ {
				if r[j] == '\\' {
					j++
					continue
				}
				if r[j] == ']' && j > i+1 {
					end = j
					break
				}
			}
			if end < 0 {
				return nil, fmt.Errorf("'[' does not start a bracket expression (escape a literal one as \\[)")
			}
			class := string(r[i+1 : end])
			if strings.Contains(class, "/") {
				return nil, fmt.Errorf("bracket expression contains '/'")
			}
			if strings.HasPrefix(class, "!") {
				class = "^" + class[1:]
			}
			b.WriteString("[" + strings.ReplaceAll(class, `\`, `\\`) + "]")
			i = end
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	b.WriteString("$")
	re, err := regexp.Compile(b.String())
	if err != nil {
		return nil, fmt.Errorf("unsupported pattern: %v", err)
	}
	return re, nil
}
