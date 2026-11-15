package kb

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Machine-local state about how kb is wired into an agent on this machine,
// and what its hooks delivered. None of it is knowledge: it lives in the
// gitignored local directory and never syncs.

// File-context modes: off by default.
const (
	FileContextPost = "post" // after a successful Read, Edit or Write
	FileContextPre  = "pre"  // also before an Edit or Write
)

// Capture modes of an installation.
const (
	CaptureFull   = "full"   // capture hooks installed (transcript and memory extraction)
	CaptureManual = "manual" // pointer, skill and rule delivery only; nothing is captured automatically
)

// InstallRecord remembers the choices made when installing into an agent,
// so a later bare install or repair keeps them instead of resetting them.
type InstallRecord struct {
	Claude map[string]*ClaudeInstall `json:"claude,omitempty"` // by scope: "user" or a repository root
}

// ClaudeInstall is one Claude Code installation (user-wide or one repository).
type ClaudeInstall struct {
	Capture     string `json:"capture"`                // CaptureFull or CaptureManual
	Settings    string `json:"settings"`               // the settings.json that carries the hooks
	NativeRules string `json:"native_rules,omitempty"` // managed rules file Claude Code loads itself ("" = not used)
	RuleRefresh bool   `json:"rule_refresh,omitempty"` // PreToolUse hook that re-delivers changed rules
	FileContext string `json:"file_context,omitempty"` // "", FileContextPost or FileContextPre
	Updated     string `json:"updated"`
}

func (s *Store) installPath() string { return s.LocalPath("install.json") }

// LoadInstall reads the local installation record (empty when none).
func (s *Store) LoadInstall() *InstallRecord {
	r := &InstallRecord{}
	if data, err := os.ReadFile(s.installPath()); err == nil {
		_ = json.Unmarshal(data, r)
	}
	if r.Claude == nil {
		r.Claude = map[string]*ClaudeInstall{}
	}
	return r
}

// SaveInstall writes the local installation record.
func (s *Store) SaveInstall(r *InstallRecord) error {
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	_, err = WriteFileIfChanged(s.installPath(), data)
	return err
}

// claudeInstallHere returns the installation that governs the current
// session: the repository's own, else the user-wide one.
func (s *Store) claudeInstallHere() *ClaudeInstall {
	r := s.LoadInstall()
	if s.CodeRoot != "" {
		if in := r.Claude[s.CodeRoot]; in != nil {
			return in
		}
	}
	return r.Claude["user"]
}

// FileContextMode is the file-context mode of the installation that governs
// this session: "" (off), FileContextPost or FileContextPre.
func (s *Store) FileContextMode() string {
	if in := s.claudeInstallHere(); in != nil {
		return in.FileContext
	}
	return ""
}

// NativeRulesText is the content of the managed rules file Claude Code
// loads natively: the global rules that apply everywhere, fitted to the
// file's declared budget. Project rules, global rules restricted to some
// projects and conditional rules never go there: the file is read in every
// repository. It is "" when there is nothing to deliver.
func (s *Store) NativeRulesText() string {
	d, err := s.NativeRulesDelivery()
	if err != nil {
		return ""
	}
	return d.Text
}

// NativeRulesDelivery plans the managed rules file.
func (s *Store) NativeRulesDelivery() (Delivery, error) {
	rs, err := s.nativeRuleSet()
	if err != nil {
		return Delivery{}, err
	}
	return s.planDelivery(SurfaceNativeRules, NativeRulesBudget, "", rs, nil, planOpts{}), nil
}

// nativeRuleSet is what the managed rules file is planned from.
func (s *Store) nativeRuleSet() (RuleSet, error) {
	rs, err := s.universalRules()
	rs.Conditional = nil // the hook names them; this file holds rules only
	return rs, err
}

// universalRules are the always-on global rules without a project
// restriction (hub mode; a repo bundle has no global scope).
func (s *Store) universalRules() (RuleSet, error) {
	if !s.Hub {
		return RuleSet{Scope: "repository"}, nil
	}
	rs, err := s.Rules([]string{GlobalProject}, false)
	rs.Scoped, rs.Project, rs.Pending = nil, "", nil
	return rs, err
}

// ClaudeConfigDir is the Claude Code configuration directory in effect for
// this process: where a session started with this environment reads its
// settings and rules files.
func ClaudeConfigDir() string {
	if d := os.Getenv("CLAUDE_CONFIG_DIR"); d != "" {
		return d
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".claude")
}

// NativeRulesActive reports whether this session can count on the managed
// rules file for the universal global rules: an installation recorded one,
// it is in the configuration directory this session reads, the file is
// there, and it holds exactly what would be written now. When it does not,
// the hooks deliver those rules themselves.
func (s *Store) NativeRulesActive() (path string, active bool) {
	in := s.claudeInstallHere()
	if in == nil || in.NativeRules == "" || !s.Hub {
		return "", false
	}
	// A session under another configuration directory never loads it.
	if filepath.Clean(filepath.Dir(filepath.Dir(in.NativeRules))) != filepath.Clean(ClaudeConfigDir()) {
		return in.NativeRules, false
	}
	data, err := os.ReadFile(in.NativeRules)
	if err != nil {
		return in.NativeRules, false
	}
	want := s.NativeRulesText()
	return in.NativeRules, want != "" && strings.Contains(string(data), strings.TrimRight(want, "\n"))
}

// RuleDigest identifies the set of rules an agent should have: every rule
// that applies, by id and revision. It changes when a rule is added,
// removed, rewritten or re-scoped, however the rules are delivered.
func (s *Store) RuleDigest() string {
	rs, err := s.AllRules(nil, false)
	if err != nil {
		return ""
	}
	var keys []string
	for _, group := range [][]*Node{rs.Scoped, rs.Global, rs.Conditional} {
		for _, n := range group {
			m := n.Meta()
			keys = append(keys, n.ID+"@"+itoa(m.Rev())+":"+m.Title+"|"+m.Description)
		}
	}
	sort.Strings(keys)
	sum := sha256.Sum256([]byte(strings.Join(keys, "\n")))
	return hex.EncodeToString(sum[:8])
}

// DeliveryState is what the hooks delivered in one session, per agent.
type DeliveryState struct {
	Session string                    `json:"session"`
	Project string                    `json:"project,omitempty"`
	Agents  map[string]*AgentDelivery `json:"agents"` // "main" or a subagent id
}

// AgentDelivery is the last rule delivery to one agent of a session.
type AgentDelivery struct {
	Type        string `json:"type,omitempty"`   // subagent type
	Digest      string `json:"digest"`           // RuleDigest delivered
	Surface     string `json:"surface"`          // how it was delivered
	Event       string `json:"event"`            // hook event and source
	Native      bool   `json:"native,omitempty"` // universal rules were left to the managed rules file
	Rules       int    `json:"rules"`            // rules the agent holds after it (in the payload or the managed file)
	Compactions int    `json:"compactions"`      // compactions of this agent seen so far
	Deliveries  int    `json:"deliveries"`       // times rules were delivered to it
	// FileContext is what the file-context hook already gave this agent
	// since its last compaction: entry id -> revision. A compaction clears
	// it, because the summary may not have kept the headers.
	FileContext map[string]int `json:"file_context,omitempty"`
	At          string         `json:"at"`
}

func (s *Store) deliveryPath(session string) string {
	return s.LocalPath("sessions", Slugify(session)+".json")
}

// LoadDelivery reads a session's delivery state (empty when none).
func (s *Store) LoadDelivery(session string) *DeliveryState {
	st := &DeliveryState{Session: session, Agents: map[string]*AgentDelivery{}}
	if data, err := os.ReadFile(s.deliveryPath(session)); err == nil {
		_ = json.Unmarshal(data, st)
	}
	if st.Agents == nil {
		st.Agents = map[string]*AgentDelivery{}
	}
	return st
}

// RecordDelivery notes that rules were delivered to an agent of a session.
// Each agent has its own record: a delivery to the main agent says nothing
// about a subagent, which starts with its own context.
func (s *Store) RecordDelivery(session, agent, agentType, event string, d Delivery, native, compacted bool) {
	if session == "" {
		return
	}
	if agent == "" {
		agent = "main"
	}
	unlock, err := s.lockFile("sessions.lock")
	if err != nil {
		return
	}
	defer unlock()
	st := s.LoadDelivery(session)
	st.Project = s.Project
	a := st.Agents[agent]
	if a == nil {
		a = &AgentDelivery{}
		st.Agents[agent] = a
	}
	a.Type, a.Digest, a.Surface, a.Event, a.Native = agentType, s.RuleDigest(), d.Surface, event, native
	a.Rules = d.Rendered + d.Native
	a.Deliveries++
	if compacted {
		a.Compactions++
		a.FileContext = nil // only this agent's: another agent's context is intact
	}
	a.At = s.Now().UTC().Format(time.RFC3339)
	if data, err := json.MarshalIndent(st, "", "  "); err == nil {
		_, _ = WriteFileIfChanged(s.deliveryPath(session), data)
	}
	s.pruneDeliveries()
	// The agent holds the rules in the payload and those the managed rules
	// file gave it: both were delivered.
	s.NoteAccess(AccessDelivered, append(append([]string{}, d.IDs...), d.NativeIDs...))
}

// pruneDeliveries drops session records not touched for 30 days.
func (s *Store) pruneDeliveries() {
	entries, err := os.ReadDir(s.LocalPath("sessions"))
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-30 * 24 * time.Hour)
	for _, e := range entries {
		if info, err := e.Info(); err == nil && !e.IsDir() && strings.HasSuffix(e.Name(), ".json") && info.ModTime().Before(cutoff) {
			_ = os.Remove(filepath.Join(s.LocalPath("sessions"), e.Name()))
		}
	}
}

// DeliveryStates lists recorded sessions, most recent first.
func (s *Store) DeliveryStates(limit int) []*DeliveryState {
	entries, err := os.ReadDir(s.LocalPath("sessions"))
	if err != nil {
		return nil
	}
	type item struct {
		st *DeliveryState
		at time.Time
	}
	var items []item
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(s.LocalPath("sessions"), e.Name()))
		info, ierr := e.Info()
		if err != nil || ierr != nil {
			continue
		}
		st := &DeliveryState{}
		if json.Unmarshal(data, st) == nil && st.Session != "" {
			items = append(items, item{st, info.ModTime()})
		}
	}
	sort.Slice(items, func(i, j int) bool { return items[i].at.After(items[j].at) })
	var out []*DeliveryState
	for i, it := range items {
		if limit > 0 && i >= limit {
			break
		}
		out = append(out, it.st)
	}
	return out
}

// RenderDeliveryDiagnostics describes how rules reach Claude Code on this
// machine: the managed rules file, the installation choices, and what each
// agent of the recent sessions last received compared with the rules now.
func (s *Store) RenderDeliveryDiagnostics() string {
	var b strings.Builder
	now := s.RuleDigest()
	fmt.Fprintf(&b, "rules now: %s (changes when a rule that applies here is added, removed, rewritten or re-scoped)\n", now)
	in := s.claudeInstallHere()
	switch {
	case in == nil:
		b.WriteString("claude installation: none recorded on this machine ('kb install claude'); a bare install uses full capture\n")
	default:
		fc := in.FileContext
		if fc == "" {
			fc = "off"
		}
		fmt.Fprintf(&b, "claude installation: capture %s; rule refresh hook %s; file context %s; hooks in %s\n", in.Capture, onOff(in.RuleRefresh), fc, in.Settings)
	}
	switch path, active := s.NativeRulesActive(); {
	case path == "":
		b.WriteString("managed rules file: not used; SessionStart delivers every rule\n")
	case active:
		fmt.Fprintf(&b, "managed rules file: %s is current; Claude Code loads the global rules that apply everywhere from it (also after compaction), SessionStart delivers the rest\n", path)
	case s.NativeRulesText() == "":
		fmt.Fprintf(&b, "managed rules file: %s is not written: no global rule applies everywhere\n", path)
	default:
		fmt.Fprintf(&b, "managed rules file: %s is missing or out of date, so SessionStart delivers every rule itself (fallback); 'kb install claude' or the next rule change rewrites it\n", path)
	}
	b.WriteString("subagents: SubagentStart gives each one the complete rule set; nothing is assumed about what a subagent loads itself, and a fork inherits its parent's context\n")
	states := s.DeliveryStates(5)
	if len(states) == 0 {
		b.WriteString("recent sessions: none recorded\n")
		return b.String()
	}
	b.WriteString("recent sessions (what each agent last received):\n")
	for _, st := range states {
		project := st.Project
		if project == "" {
			project = "no project"
		}
		fmt.Fprintf(&b, "  %s [%s]\n", st.Session, project)
		var agents []string
		for id := range st.Agents {
			agents = append(agents, id)
		}
		sort.Strings(agents)
		for _, id := range agents {
			a := st.Agents[id]
			name := id
			if a.Type != "" {
				name += " (" + a.Type + ")"
			}
			state := "current"
			if a.Digest != now {
				// The agent keeps the old rules until something delivers
				// again: say so instead of showing the new digest.
				state = "STALE: it has " + a.Digest + "; refreshed at its next start or compaction"
				if in != nil && in.RuleRefresh {
					state += ", or before its next tool call (rule refresh is on)"
				}
			}
			via := a.Surface + " on " + a.Event
			if a.Native {
				via += ", universal rules from the managed file"
			}
			fmt.Fprintf(&b, "    %s: %s; via %s; %d deliver(ies), %d compaction(s); %s\n", name, state, via, a.Deliveries, a.Compactions, a.At)
		}
	}
	return b.String()
}

func onOff(v bool) string {
	if v {
		return "on"
	}
	return "off"
}
