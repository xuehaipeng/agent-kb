package kb

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// ConfigFile is the bundle-root config file name.
const ConfigFile = "kb.yaml"

// TypeConfig describes one knowledge type (a directory with lifecycle rules).
type TypeConfig struct {
	Dir         string `yaml:"dir"`
	TTLDays     int    `yaml:"ttl_days,omitempty"`
	Immutable   bool   `yaml:"immutable,omitempty"`
	Description string `yaml:"description,omitempty"`
}

// RetiredTerm is a phrase that must no longer appear in the bundle.
type RetiredTerm struct {
	Term string `yaml:"term"`
	Hint string `yaml:"hint,omitempty"`
}

// Config is the content of knowledge/kb.yaml.
type Config struct {
	Version  int                   `yaml:"version"`
	CodeRoot string                `yaml:"code_root,omitempty"`
	Types    map[string]TypeConfig `yaml:"types"`
	Families struct {
		Producers map[string]string `yaml:"producers"`
		Models    map[string]string `yaml:"models"`
	} `yaml:"families"`
	Search struct {
		Limit int `yaml:"limit"`
	} `yaml:"search"`
	Lint struct {
		RetiredTerms []RetiredTerm `yaml:"retired_terms"`
		MaxBodyBytes int           `yaml:"max_body_bytes"`
	} `yaml:"lint"`
	Extract struct {
		Commands   map[string][]string `yaml:"commands"`
		MaxClaims  int                 `yaml:"max_claims"`
		MinChars   int                 `yaml:"min_chars"`
		TimeoutSec int                 `yaml:"timeout_sec"`
	} `yaml:"extract"`
}

// DefaultConfigYAML is written by `kb init` and is also the base every
// kb.yaml is merged onto, so a sparse kb.yaml stays valid.
const DefaultConfigYAML = `# AgentKB configuration. See docs/ARCHITECTURE.md in the agent-kb repository.
version: 1

# Git work tree whose files 'resources:' globs refer to (relative to this
# directory). Used for path-aware search and code-change staleness.
code_root: ..

# Knowledge types. ttl_days sets stale_after at write/confirm time.
# Immutable types can only be superseded or deprecated, never edited.
types:
  fact:
    dir: facts
    ttl_days: 90
    description: Stable project facts and invariants.
  learning:
    dir: learnings
    ttl_days: 30
    immutable: true
    description: Gotchas, root causes, debugging lessons.
  decision:
    dir: decisions
    immutable: true
    description: Architectural decisions (ADR); superseded, never edited.
  convention:
    dir: conventions
    description: Coding and workflow conventions.
  review:
    dir: reviews
    ttl_days: 14
    description: Cross-agent review packets.

# Model family resolution for the cross-family verification rule.
# An actor is "<producer>/<model>". A recognised model prefix wins over the
# producer default, so cursor/claude-sonnet-4.5 is anthropic.
families:
  producers:
    claude-code: anthropic
    claude: anthropic
    codex: openai
    chatgpt: openai
    cursor: cursor
    antigravity: google
    gemini-cli: google
    gemini: google
    grokbot: xai
    grok: xai
    hermes: nous
  models:
    claude: anthropic
    opus: anthropic
    sonnet: anthropic
    haiku: anthropic
    gpt: openai
    o1: openai
    o3: openai
    o4: openai
    codex: openai
    gemini: google
    grok: xai
    hermes: nous
    qwen: alibaba
    deepseek: deepseek
    kimi: moonshot
    glm: zhipu

search:
  limit: 8

lint:
  # - term: publish.sh
  #   hint: replaced by 'make release' in 2026-10
  retired_terms: []
  max_body_bytes: 6000

# Transcript capture ('kb extract'). The command receives the prompt on stdin
# and must print a JSON array of claims.
extract:
  commands:
    claude: [claude, -p, --model, haiku]
    codex: [codex, exec, --skip-git-repo-check, "-"]
    text: [claude, -p, --model, haiku]
  max_claims: 5
  min_chars: 1500
  timeout_sec: 240
`

// LoadConfig reads kb.yaml from the bundle directory, layered on defaults.
func LoadConfig(bundleDir string) (*Config, error) {
	cfg := &Config{}
	if err := yaml.Unmarshal([]byte(DefaultConfigYAML), cfg); err != nil {
		return nil, fmt.Errorf("default config: %w", err)
	}
	data, err := os.ReadFile(filepath.Join(bundleDir, ConfigFile))
	if err != nil {
		if os.IsNotExist(err) {
			return cfg, nil
		}
		return nil, err
	}
	var user Config
	if err := yaml.Unmarshal(data, &user); err != nil {
		return nil, fmt.Errorf("%s: %w", ConfigFile, err)
	}
	if user.CodeRoot != "" {
		cfg.CodeRoot = user.CodeRoot
	}
	if len(user.Types) > 0 {
		cfg.Types = user.Types
	}
	for k, v := range user.Families.Producers {
		cfg.Families.Producers[k] = v
	}
	for k, v := range user.Families.Models {
		cfg.Families.Models[k] = v
	}
	if user.Search.Limit > 0 {
		cfg.Search.Limit = user.Search.Limit
	}
	if user.Lint.RetiredTerms != nil {
		cfg.Lint.RetiredTerms = user.Lint.RetiredTerms
	}
	if user.Lint.MaxBodyBytes > 0 {
		cfg.Lint.MaxBodyBytes = user.Lint.MaxBodyBytes
	}
	for k, v := range user.Extract.Commands {
		cfg.Extract.Commands[k] = v
	}
	if user.Extract.MaxClaims > 0 {
		cfg.Extract.MaxClaims = user.Extract.MaxClaims
	}
	if user.Extract.MinChars > 0 {
		cfg.Extract.MinChars = user.Extract.MinChars
	}
	if user.Extract.TimeoutSec > 0 {
		cfg.Extract.TimeoutSec = user.Extract.TimeoutSec
	}
	for name, t := range cfg.Types {
		if t.Dir == "" {
			return nil, fmt.Errorf("%s: type %q has no dir", ConfigFile, name)
		}
	}
	return cfg, nil
}

// TypeKey normalises a type name ("Learning", "learnings") to its config key.
func (c *Config) TypeKey(t string) (string, bool) {
	k := strings.ToLower(strings.TrimSpace(t))
	if _, ok := c.Types[k]; ok {
		return k, true
	}
	for name, tc := range c.Types {
		if tc.Dir == k {
			return name, true
		}
	}
	return "", false
}

// TypeForDir returns the type whose directory is dir.
func (c *Config) TypeForDir(dir string) (string, bool) {
	for name, tc := range c.Types {
		if tc.Dir == dir {
			return name, true
		}
	}
	return "", false
}

// TypeNames lists configured type keys in a stable order.
func (c *Config) TypeNames() []string {
	order := map[string]int{"fact": 0, "learning": 1, "decision": 2, "convention": 3, "review": 4}
	var out []string
	for k := range c.Types {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool {
		oi, iok := order[out[i]]
		oj, jok := order[out[j]]
		switch {
		case iok && jok:
			return oi < oj
		case iok:
			return true
		case jok:
			return false
		}
		return out[i] < out[j]
	})
	return out
}

// DisplayType renders a type key the way it is written in frontmatter.
func DisplayType(key string) string {
	if key == "" {
		return ""
	}
	return strings.ToUpper(key[:1]) + key[1:]
}
