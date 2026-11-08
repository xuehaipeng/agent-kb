package kb

import (
	"errors"
	"os"
	"strings"
)

// Actor identifies who wrote or verified something, using the OKF
// convention: "<producer>/<version>" for agents and "human:<id>" for people.
type Actor struct {
	Producer string // claude-code, codex, hermes, ...; "human" for people
	Model    string // optional model/version part, or the human id
}

// ParseActor parses "claude-code/opus-4.5", "codex", or "human:xue".
func ParseActor(s string) Actor {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(strings.ToLower(s), "human:") {
		return Actor{Producer: "human", Model: strings.TrimSpace(s[len("human:"):])}
	}
	if i := strings.Index(s, "/"); i >= 0 {
		return Actor{Producer: strings.ToLower(s[:i]), Model: s[i+1:]}
	}
	return Actor{Producer: strings.ToLower(s)}
}

func (a Actor) String() string {
	if a.Producer == "human" {
		return "human:" + a.Model
	}
	if a.Model == "" {
		return a.Producer
	}
	return a.Producer + "/" + a.Model
}

// IsHuman reports whether the actor is a person.
func (a Actor) IsHuman() bool { return a.Producer == "human" }

// Zero reports whether no actor is set.
func (a Actor) Zero() bool { return a.Producer == "" }

// Family resolves the model family used by the cross-family rule.
func (c *Config) Family(a Actor) string {
	if a.IsHuman() {
		return "human"
	}
	if a.Model != "" {
		m := strings.ToLower(a.Model)
		best, bestLen := "", 0
		for prefix, fam := range c.Families.Models {
			if strings.HasPrefix(m, prefix) && len(prefix) > bestLen {
				best, bestLen = fam, len(prefix)
			}
		}
		if best != "" {
			return best
		}
	}
	if fam, ok := c.Families.Producers[a.Producer]; ok {
		return fam
	}
	return a.Producer
}

// FamilyOf is Family for an actor string.
func (c *Config) FamilyOf(s string) string { return c.Family(ParseActor(s)) }

// ErrNoActor is returned when a write cannot be attributed.
var ErrNoActor = errors.New("cannot tell which agent is calling: set KB_ACTOR (e.g. KB_ACTOR=codex or KB_ACTOR=claude-code/opus-4.5, or human:<name>) or pass --as")

// DetectActor resolves the calling actor: explicit value, then KB_ACTOR, then
// environment fingerprints of known agents. People must identify explicitly
// with human:<id>, because human verification overrides disputes.
func DetectActor(explicit string) (Actor, error) {
	if explicit != "" {
		return withModel(ParseActor(explicit)), nil
	}
	if v := os.Getenv("KB_ACTOR"); v != "" {
		return withModel(ParseActor(v)), nil
	}
	if p := detectProducer(os.Environ()); p != "" {
		return withModel(Actor{Producer: p}), nil
	}
	return Actor{}, ErrNoActor
}

func withModel(a Actor) Actor {
	if a.Model == "" && !a.IsHuman() {
		a.Model = os.Getenv("KB_MODEL")
	}
	return a
}

func detectProducer(env []string) string {
	has := func(prefix string) bool {
		for _, kv := range env {
			if strings.HasPrefix(kv, prefix) {
				return true
			}
		}
		return false
	}
	switch {
	case has("CLAUDECODE=1"), has("CLAUDE_CODE_ENTRYPOINT="):
		return "claude-code"
	case has("CODEX_SANDBOX"), has("CODEX_THREAD_ID="), has("CODEX_SESSION_ID="):
		return "codex"
	case has("HERMES_SESSION"), has("HERMES_AGENT"):
		return "hermes"
	case has("CURSOR_AGENT="), has("CURSOR_TRACE_ID="):
		return "cursor"
	case has("ANTIGRAVITY_"):
		return "antigravity"
	}
	return ""
}

// ActorFromClientName maps an MCP clientInfo.name to a producer.
func ActorFromClientName(name string) Actor {
	n := strings.ToLower(name)
	switch {
	case strings.Contains(n, "claude"):
		return Actor{Producer: "claude-code"}
	case strings.Contains(n, "codex"):
		return Actor{Producer: "codex"}
	case strings.Contains(n, "cursor"):
		return Actor{Producer: "cursor"}
	case strings.Contains(n, "hermes"):
		return Actor{Producer: "hermes"}
	case strings.Contains(n, "antigravity"):
		return Actor{Producer: "antigravity"}
	case strings.Contains(n, "gemini"):
		return Actor{Producer: "gemini-cli"}
	case strings.Contains(n, "grok"):
		return Actor{Producer: "grokbot"}
	}
	return Actor{}
}
