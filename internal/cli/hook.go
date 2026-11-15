package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/xuehaipeng/agent-kb/internal/extract"
	"github.com/xuehaipeng/agent-kb/internal/kb"
)

// hookInput is the JSON Claude Code passes to command hooks on stdin.
type hookInput struct {
	SessionID      string `json:"session_id"`
	TranscriptPath string `json:"transcript_path"`
	CWD            string `json:"cwd"`
	HookEventName  string `json:"hook_event_name"`
	Source         string `json:"source"`
	Reason         string `json:"reason"`
	Trigger        string `json:"trigger"`
	// Set when the event belongs to a subagent rather than the main agent.
	AgentID   string `json:"agent_id"`
	AgentType string `json:"agent_type"`
}

// hookContext prints additionalContext for events that take context only
// as JSON (SubagentStart, PreToolUse). It sets no decision.
func (a *App) hookContext(event, text string) {
	out, err := json.Marshal(map[string]any{"hookSpecificOutput": map[string]any{"hookEventName": event, "additionalContext": text}})
	if err == nil {
		fmt.Fprintln(a.Stdout, string(out))
	}
}

// cmdHook never fails the agent: errors go to the local log, exit code is 0.
func (a *App) cmdHook(args []string) int {
	if len(args) == 0 || args[0] != "claude" {
		fmt.Fprintln(a.Stderr, "usage: kb hook claude   (reads Claude Code hook JSON on stdin)")
		return 0
	}
	data, _ := io.ReadAll(io.LimitReader(a.Stdin, 1<<20))
	var in hookInput
	_ = json.Unmarshal(data, &in)
	if in.CWD != "" && a.root == "" && os.Getenv("KB_ROOT") == "" {
		_ = os.Chdir(in.CWD)
	}
	s, err := a.store()
	if err != nil {
		return 0 // not a kb-enabled repository
	}
	switch in.HookEventName {
	case "SessionStart":
		me, err := kb.DetectActor(a.as)
		if err != nil {
			me = kb.Actor{Producer: "claude-code"}
		}
		// Rules are pushed into the session; everything else is pulled. Both
		// the rules and the brief follow the session's project: outside any
		// project that is the global scope only, never every project.
		// A subagent's own SessionStart (after it compacts): it gets the
		// complete rule set, like at its start. Nothing is assumed about
		// what it loads natively.
		if in.AgentID != "" {
			if d, err := s.AgentDelivery(kb.SurfaceSessionStart); err == nil {
				fmt.Fprint(a.Stdout, d.Text)
				s.RecordDelivery(in.SessionID, in.AgentID, in.AgentType, "SessionStart:"+in.Source, d, false, in.Source == "compact")
			}
			return 0
		}
		// The whole payload is fitted to one budget: Claude Code replaces a
		// hook's stdout with a short preview once it passes 10,000
		// characters, so an oversized rule list would mostly go missing.
		// It is printed on every start, resume, clear, compaction and fork:
		// a session seen before may still hold rules that have changed.
		if d, err := s.SessionStartDelivery(me); err == nil {
			fmt.Fprint(a.Stdout, d.Text)
			s.RecordDelivery(in.SessionID, "", "", "SessionStart:"+in.Source, d, d.Native > 0, in.Source == "compact")
		}
		// Hub: pull what other machines (e.g. Hermes) recorded, in the background.
		if s.Hub && hasOrigin(s) && os.Getenv(extract.GuardEnv) == "" {
			if err := a.spawn(s, in, []string{"--root", s.Dir, "sync", "--quiet"}, "sync"); err != nil {
				logLine(s, "hook: "+err.Error())
			}
		}
	case "SubagentStart":
		// A subagent starts with its own context: it does not see what
		// SessionStart gave the main agent, and may not load CLAUDE.md or
		// the rules files (Explore, Plan and agents that omit them do not).
		// So it gets the complete applicable rule set here, whatever the
		// main agent received.
		if d, err := s.AgentDelivery(kb.SurfaceSubagentStart); err == nil && d.Text != "" {
			a.hookContext("SubagentStart", d.Text)
			s.RecordDelivery(in.SessionID, orMain(in.AgentID), in.AgentType, "SubagentStart", d, false, false)
		}
	case "PreToolUse":
		// Opt-in rule refresh: when the rules changed since this agent last
		// received them, hand it the current set before it goes on. A
		// subagent keeps what SubagentStart gave it until then, and a
		// running main session only gets rules again at its next start.
		st := s.LoadDelivery(in.SessionID)
		last := st.Agents[orMain(in.AgentID)]
		if last != nil && last.Digest == s.RuleDigest() {
			return 0
		}
		if d, err := s.AgentDelivery(kb.SurfaceRuleRefresh); err == nil && d.Text != "" {
			a.hookContext("PreToolUse", "kb: the rules that apply here changed since you last received them. They are now:\n"+d.Text)
			s.RecordDelivery(in.SessionID, orMain(in.AgentID), in.AgentType, "PreToolUse:refresh", d, false, false)
		}
	case "PreCompact", "SessionEnd", "Stop":
		if os.Getenv(extract.GuardEnv) != "" || in.TranscriptPath == "" {
			return 0
		}
		if err := a.spawnExtract(s, in); err != nil {
			logLine(s, "hook: "+err.Error())
		}
		// Share what Claude saved in its own memory (only changed files are read).
		if in.HookEventName == "SessionEnd" {
			if err := a.spawn(s, in, []string{"--root", s.Dir, "extract", "--scan", "claude-memory"}, "memory"); err != nil {
				logLine(s, "hook: "+err.Error())
			}
		}
	}
	return 0
}

// spawnExtract runs 'kb extract' detached so the session is not blocked.
func (a *App) spawnExtract(s *kb.Store, in hookInput) error {
	actor := a.as
	if actor == "" {
		actor = os.Getenv("KB_ACTOR")
	}
	if actor == "" {
		actor = "claude-code"
	}
	args := []string{"--root", s.Dir, "--as", actor, "extract", "--transcript", in.TranscriptPath, "--source", "claude", "--session", in.SessionID}
	if in.HookEventName == "SessionEnd" {
		args = append(args, "--final")
		if s.Hub && hasOrigin(s) {
			args = append(args, "--sync")
		}
	}
	return a.spawn(s, in, args, "extract")
}

// spawn starts a detached kb subprocess, logging to .kb/extract.log. It runs
// in the session's directory so a hub attaches the right project.
func (a *App) spawn(s *kb.Store, in hookInput, args []string, label string) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(s.LocalPath(), 0o755); err != nil {
		return err
	}
	logf, err := os.OpenFile(s.LocalPath("extract.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer logf.Close()
	fmt.Fprintf(logf, "--- %s %s %s session=%s\n", in.HookEventName, label, filepath.Base(in.TranscriptPath), in.SessionID)
	cmd := exec.Command(exe, args...)
	cmd.Env = append(os.Environ(), extract.GuardEnv+"=1")
	cmd.Stdout, cmd.Stderr = logf, logf
	cmd.Dir = s.Dir
	if s.CodeRoot != "" {
		cmd.Dir = s.CodeRoot
	} else if in.CWD != "" {
		cmd.Dir = in.CWD
	}
	detach(cmd)
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}

func orMain(agent string) string {
	if agent == "" {
		return "main"
	}
	return agent
}

func hasOrigin(s *kb.Store) bool {
	top := s.KnowledgeTop()
	if top == "" {
		return false
	}
	_, err := kb.Git(top, "remote", "get-url", "origin")
	return err == nil
}

func logLine(s *kb.Store, msg string) {
	_ = os.MkdirAll(s.LocalPath(), 0o755)
	f, err := os.OpenFile(s.LocalPath("extract.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintln(f, msg)
}
