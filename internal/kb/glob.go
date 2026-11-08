package kb

import (
	"path"
	"strings"
)

// MatchGlob reports whether a repo-relative file path matches a resource
// pattern. Semantics, close to .gitignore:
//   - "**" matches zero or more path segments; "*", "?", "[...]" match within one
//   - a pattern without "/" matches the file's basename at any depth ("*.go")
//   - a pattern ending in "/" (or naming a directory) matches everything below it
func MatchGlob(pattern, file string) bool {
	p := normGlobPath(pattern)
	f := normGlobPath(file)
	if p == "" || f == "" {
		return false
	}
	if strings.HasSuffix(strings.TrimSpace(pattern), "/") {
		p += "/**"
	}
	if !strings.Contains(p, "/") {
		if ok, _ := path.Match(p, path.Base(f)); ok {
			return true
		}
		return matchSegments(strings.Split(p, "/"), strings.Split(f, "/"))
	}
	if matchSegments(strings.Split(p, "/"), strings.Split(f, "/")) {
		return true
	}
	// Literal directory prefix: "pkg/inference" covers "pkg/inference/x.go".
	if !strings.ContainsAny(p, "*?[") && strings.HasPrefix(f, p+"/") {
		return true
	}
	return false
}

func normGlobPath(s string) string {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\\", "/"))
	for strings.HasPrefix(s, "./") {
		s = s[2:]
	}
	s = strings.TrimPrefix(s, "/")
	return strings.TrimSuffix(s, "/")
}

func matchSegments(pat, segs []string) bool {
	for len(pat) > 0 {
		if pat[0] == "**" {
			for len(pat) > 1 && pat[1] == "**" {
				pat = pat[1:]
			}
			if len(pat) == 1 {
				return true
			}
			for i := 0; i <= len(segs); i++ {
				if matchSegments(pat[1:], segs[i:]) {
					return true
				}
			}
			return false
		}
		if len(segs) == 0 {
			return false
		}
		ok, err := path.Match(pat[0], segs[0])
		if err != nil || !ok {
			return false
		}
		pat, segs = pat[1:], segs[1:]
	}
	return len(segs) == 0
}

// MatchAny reports whether file matches any of the patterns.
func MatchAny(patterns []string, file string) bool {
	for _, p := range patterns {
		if MatchGlob(p, file) {
			return true
		}
	}
	return false
}
